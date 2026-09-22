// Command unity-sync mirrors the assets owned on the Unity Asset Store into a local
// library, downloading only what changed since the last run.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/curbol/unity-sync/internal/config"
	"github.com/curbol/unity-sync/internal/lockfile"
	"github.com/curbol/unity-sync/internal/manifest"
	"github.com/curbol/unity-sync/internal/model"
	"github.com/curbol/unity-sync/internal/selfupdate"
	"github.com/curbol/unity-sync/internal/session"
	"github.com/curbol/unity-sync/internal/store"
	"github.com/curbol/unity-sync/internal/syncer"
	"github.com/curbol/unity-sync/internal/web"
)

// version is stamped at release time.
var version = "dev"

const defaultSelectAddr = "127.0.0.1:8788"

// stdout is where the tool's own output goes, so tests can capture it. Progress and
// diagnostics stay on stderr.
var stdout io.Writer = os.Stdout

// storeBaseURL points the client somewhere other than the real store. Empty outside
// tests, and a package variable for the same reason stdout is: run's config → session →
// client → write chain is otherwise unreachable without going to the live store, so the
// one place that decides which cookie reaches it, and the fact that neither committed
// file is written until the store has answered, would ship untested.
var storeBaseURL string

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "unity-sync:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
	if len(args) == 0 {
		usage(os.Stderr)
		return 1, errors.New("a subcommand is required")
	}
	cmd, rest := args[0], args[1:]

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgDir := fs.String("config", "", "user config dir (default $XDG_CONFIG_HOME/unity-sync)")
	manifestFlag := fs.String("manifest", "", "project manifest path (default: nearest unity-sync.toml walking up)")
	sessionFlag := fs.String("session", "", "session source: \"browser\" to read a signed-in\n"+
		"Firefox-family profile, or a path to a pasted-curl file, a cookies.txt,\n"+
		"a browser profile directory, or a recovery.jsonlz4")
	library := fs.String("library", "", "library directory (overrides config / UNITY_SYNC_LIBRARY)")
	only := fs.String("only", "", "limit to assets whose slug matches this glob")
	concurrency := fs.Int("concurrency", 0, "max simultaneous downloads (overrides config)")
	verify := fs.Bool("verify", false, "re-hash cached files instead of the cheap size+metadata check")
	dryRun := fs.Bool("dry-run", false, "on sync, classify and report only")
	addr := fs.String("addr", defaultSelectAddr, "on select, the address to serve the page at")

	switch cmd {
	case "select", "status", "sync", "list", "update", "version",
		"-h", "--help", "help", "-v", "--version":
	default:
		usage(os.Stderr)
		return 1, fmt.Errorf("unknown subcommand %q", cmd)
	}
	switch cmd {
	case "-h", "--help", "help", "version", "-v", "--version":
		// These return before flag parsing, so their positionals are checked here or not
		// at all: `unity-sync version foo` should not quietly succeed.
		if len(rest) > 0 {
			return 1, fmt.Errorf("%s takes no arguments (got %q)", cmd, rest[0])
		}
		if cmd == "version" || cmd == "-v" || cmd == "--version" {
			fmt.Fprintln(stdout, "unity-sync", version)
		} else {
			// Asked for on purpose, so it goes to stdout where it can be piped, and it
			// carries the flag descriptions rather than a bare list of names.
			help(stdout, fs)
		}
		return 0, nil
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			help(stdout, fs)
			return 0, nil
		}
		// The FlagSet's own output stays discarded on this branch: run returns the error
		// and main prints it, so letting the flag package print too says it twice.
		return 1, err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// flag.Parse stops at the first non-flag argument, so an unchecked positional would
	// silently swallow every flag after it: `sync foo --dry-run` would download.
	if cmd == "update" {
		if fs.NArg() > 1 {
			return 1, fmt.Errorf("update takes at most one version, got %d arguments", fs.NArg())
		}
		if err := selfupdate.Run(ctx, stdout, version, fs.Arg(0)); err != nil {
			return 1, err
		}
		return 0, nil
	}
	if fs.NArg() > 0 {
		// --only reaches syncOrStatus and nothing else, so suggesting it to select or list
		// points at a flag they parse, accept and ignore: the page would render every owned
		// asset while reading as filtered.
		if cmd == "sync" || cmd == "status" {
			return 1, fmt.Errorf("%s takes no positional arguments (got %q); to limit assets use --only %s",
				cmd, fs.Arg(0), fs.Arg(0))
		}
		return 1, fmt.Errorf("%s takes no positional arguments (got %q)", cmd, fs.Arg(0))
	}
	// Beside the other input checks rather than at the point of use: --addr depends on
	// neither the session nor the store, and refusing it later means reading the user's
	// session file and spending a request to the live store before saying the address was
	// never going to be served.
	if cmd == "select" {
		if err := checkLoopback(*addr); err != nil {
			return 2, err
		}
	}
	// A zero is how the config chain spells "not supplied", so it cannot also mean the
	// number the user typed: --concurrency 0 would otherwise run at the configured
	// default and say nothing. flag.Int alone cannot tell the two apart, so ask the
	// FlagSet which flags were actually visited.
	if flagSet(fs, "concurrency") && *concurrency < 1 {
		return 2, fmt.Errorf("--concurrency %d is not a number of simultaneous downloads; "+
			"pass 1 or more, or omit it to use the configured default", *concurrency)
	}

	configDir, err := config.ResolveDir(*cfgDir)
	if err != nil {
		return 1, err
	}
	cfg, err := config.Load(configDir, config.Flags{
		LibraryPath:   *library,
		SessionSource: *sessionFlag,
		Concurrency:   *concurrency,
	})
	if err != nil {
		return 1, err
	}

	manifestPath, err := resolveManifest(*manifestFlag, cmd)
	if err != nil {
		return 1, err
	}
	lockPath := manifest.LockPath(manifestPath)

	if cmd == "list" {
		if err := list(stdout, lockPath); err != nil {
			return 1, err
		}
		return 0, nil
	}

	cookie, err := resolveSession(cfg, configDir)
	if err != nil {
		return 1, err
	}
	var opts []store.Option
	if storeBaseURL != "" {
		opts = append(opts, store.WithBaseURL(storeBaseURL))
	}
	client := store.New(cookie, version, opts...)

	if err := client.Bootstrap(ctx); err != nil {
		return 1, err
	}

	if cmd == "select" {
		// Bound here rather than inside selectAssets: a port already in use is worth
		// finding out about before a run spends a full enumeration discovering it.
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			return 1, err
		}
		if err := selectAssets(ctx, client, manifestPath, ln); err != nil {
			return 1, err
		}
		return 0, nil
	}
	return syncOrStatus(ctx, client, cfg, manifestPath, lockPath, *only, *verify, cmd == "status" || *dryRun)
}

// flagSet reports whether a flag was given on the command line, as opposed to holding its
// zero default. flag.Value cannot answer that for a numeric flag whose zero is also a
// meaningful thing to type.
func flagSet(fs *flag.FlagSet, name string) bool {
	var seen bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

// checkLoopback refuses a select address that is not this machine's own.
//
// The page is the account's purchase history and it carries the token that spends the one
// save the run accepts, so the handler checks each request's Host against the address it
// bound. That check can only ever be as good as the address: a wildcard bind has no one
// address to match, and Host is written by the client, so a request off the network
// claiming "localhost" is indistinguishable from a browser on this machine. Naming a
// specific non-loopback address is a deliberate exposure and stays allowed; a wildcard is
// almost always someone reaching for a port number, which --addr 127.0.0.1:PORT gives.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad --addr %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if host == "" {
		return fmt.Errorf("--addr %q binds every interface, which would serve your owned-asset "+
			"list to anything that can reach this machine: name an address, e.g. 127.0.0.1%s",
			addr, addr)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		// A hostname resolves at bind time, and whether it lands on loopback is not
		// something this can answer without repeating the lookup the listener will do.
		return fmt.Errorf("--addr %q must name an IP address or localhost", addr)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("--addr %q binds every interface, which would serve your owned-asset "+
			"list to anything that can reach this machine: name an address, e.g. 127.0.0.1", addr)
	}
	return nil
}

// resolveManifest finds the project manifest. Every command needs one except select,
// which creates it in the working directory when no ancestor has one.
func resolveManifest(flagValue, cmd string) (string, error) {
	if flagValue != "" {
		return namedManifest(config.ExpandHome(flagValue), cmd)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if p, ok := manifest.Discover(wd); ok {
		return p, nil
	}
	if cmd == "select" {
		return filepath.Join(wd, manifest.FileName), nil
	}
	return "", fmt.Errorf("no %s found in this directory or its parents; run `unity-sync select` to create one",
		manifest.FileName)
}

// namedManifest checks a manifest path the user typed, the way config.ResolveDir checks a
// config directory they named and for the same reason. A discovered path exists by
// construction; a typed one can be a typo, and manifest.Load reads an absent file as an
// empty allowlist rather than an error — so `sync --manifest <typo>` selects nothing,
// mirrors nothing, exits 0, and still writes a lockfile into the directory it was pointed
// at. In a script that reads as a successful sync of an empty library.
//
// It is also where --manifest gets the "~" expansion the other path flags get from the
// config chain, which they go through Flags to reach.
func namedManifest(path, cmd string) (string, error) {
	if cmd == "select" {
		// select creates the manifest, so the file itself need not be there yet — but the
		// directory that would hold it must be, or the page renders, takes the one save it
		// accepts, answers "Saved", and only then fails to write.
		dir := filepath.Dir(path)
		fi, err := os.Stat(dir)
		if err != nil {
			return "", fmt.Errorf("--manifest names %s, whose directory cannot be read: %w", path, err)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("--manifest names %s, but %s is not a directory", path, dir)
		}
		return path, nil
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("--manifest names %s, which does not exist; a manifest that is not "+
				"there reads as an empty allowlist, so %s would mirror nothing and still succeed "+
				"(run `unity-sync select` to create one)", path, cmd)
		}
		return "", fmt.Errorf("--manifest names %s: %w", path, err)
	}
	return path, nil
}

func resolveSession(cfg config.Config, configDir string) (store.Credential, error) {
	src := cfg.SessionSource
	if src == "" {
		if found, ok := session.Discover(configDir); ok {
			src = found
		}
	}
	if src == "" {
		return "", fmt.Errorf("no session configured: set session_source = %q in config.toml to read "+
			"it from a signed-in Firefox-family browser, save a pasted-curl file as %s, or pass "+
			"--session", session.BrowserKeyword, filepath.Join(configDir, "session.curl"))
	}
	got, err := session.ResolveFrom(src)
	if err != nil {
		return "", err
	}
	// Which profile a search settled on is not obvious, and a run against the wrong
	// signed-in account is otherwise silent. The keyword is not the only source that
	// searches: a directory is a profile root too, and picks among the profiles under it.
	// Path, never Header: the one is the file's name and the other is the live credential.
	if got.Path != src {
		fmt.Fprintln(os.Stderr, "session: read from", got.Path)
	}
	return store.Credential(got.Header), nil
}

// enumerator is the slice of the store client that `select` needs.
type enumerator interface {
	Enumerate(ctx context.Context) ([]model.Asset, error)
}

func selectAssets(ctx context.Context, client enumerator, manifestPath string, ln net.Listener) error {
	// Closed here as well as by Serve's own shutdown, because every return above it leaves
	// the port bound otherwise.
	defer func() { _ = ln.Close() }()

	m, err := manifest.Load(manifestPath)
	if err != nil {
		return err
	}
	owned, err := client.Enumerate(ctx)
	if err != nil {
		return err
	}
	dropped, err := m.Reconcile(owned)
	if err != nil {
		return err
	}
	chosen, err := web.Serve(ctx, ln, owned, m.EnabledIDs())
	if err != nil {
		return err
	}
	m.SetEnabled(chosen)
	if err := manifest.Save(manifestPath, m); err != nil {
		return err
	}
	// After the save, because Reconcile only rewrote the copy in memory: a run that ends
	// at the page — a closed tab, an interrupt — leaves every dropped entry in the file,
	// and announcing the drop before the write makes that a false statement.
	for _, e := range dropped {
		fmt.Fprintf(os.Stderr, "no longer owned, dropped from the manifest: %s (%s)\n", e.Name, e.ID)
	}
	fmt.Fprintf(stdout, "saved %d selected asset(s) to %s\n", len(chosen), manifestPath)
	return nil
}

func syncOrStatus(ctx context.Context, client syncer.Store, cfg config.Config,
	manifestPath, lockPath, only string, verify, dry bool) (int, error) {

	m, err := manifest.Load(manifestPath)
	if err != nil {
		return 1, err
	}
	prior, err := lockfile.Load(lockPath)
	if err != nil {
		return 1, err
	}
	selected := m.EnabledIDs()
	if len(selected) == 0 {
		fmt.Fprintln(os.Stderr, "nothing is enabled in", manifestPath, "— run `unity-sync select` to choose assets")
	}

	rep, err := syncer.Run(ctx, client, prior, lockPath, syncer.Options{
		LibraryRoot: cfg.LibraryPath,
		Selected:    selected,
		OnlyGlob:    only,
		DryRun:      dry,
		FullVerify:  verify,
		Concurrency: cfg.Concurrency,
		Manifest:    m,
		Progress:    func(s string) { fmt.Fprintln(os.Stderr, s) },
	})
	if err != nil {
		// Run hands back the report alongside an error, and a late failure — the final
		// lockfile write, say — comes after a full download pass whose outcome the user
		// still needs. An error raised before any of that has nothing to show.
		if len(rep.Results) > 0 {
			printReport(stdout, rep, dry, cfg.LibraryPath)
		}
		return 1, err
	}
	printReport(stdout, rep, dry, cfg.LibraryPath)
	if rep.Failed() {
		return 1, nil
	}
	return 0, nil
}

func printReport(w io.Writer, rep syncer.Report, dry bool, libraryPath string) {
	counts := map[syncer.Class]int{}
	for _, r := range rep.Results {
		counts[r.Class]++
	}
	verb := "sync"
	if dry {
		verb = "status (no changes made)"
	}
	fmt.Fprintf(w, "%s: %d owned, %d selected\n", verb, rep.Owned, rep.Selected)
	// The order comes from syncer rather than a list spelled out here, so a class added
	// there cannot go missing from the tally while still counting toward the total.
	for _, class := range syncer.Classes() {
		if n := counts[class]; n > 0 {
			fmt.Fprintf(w, "  %-15s %d\n", class, n)
		}
	}
	if rep.Swept > 0 {
		fmt.Fprintf(w, "  reclaimed       %d abandoned download(s)\n", rep.Swept)
	}
	fmt.Fprintf(w, "library: %s\n", libraryPath)

	for _, r := range rep.Results {
		if r.Warning != "" {
			fmt.Fprintf(w, "warning: %s\n", r.Warning)
		}
	}
	for _, r := range rep.Results {
		if r.Err != nil {
			fmt.Fprintf(w, "failed: %s: %v\n", r.Asset.Name, r.Err)
		}
	}
	// A permanently gone asset is reported and then deliberately left out of the exit
	// status, or one dead product would fail every run forever. Without this line the
	// summary prints "failed:" and the command exits 0, which reads as a tool that gave
	// up and lied about it rather than as the one outcome re-running cannot change.
	if rep.Permanent > 0 {
		fmt.Fprintf(w, "%d of those are permanent: the store no longer serves those bytes, "+
			"so another run will not fix them and they do not fail this one\n", rep.Permanent)
	}
	// One line, not one per asset. An expired session cancels the pool with hundreds of
	// assets still queued, and naming each as its own failure buries the one line that
	// says what went wrong.
	if rep.NotAttempted > 0 {
		fmt.Fprintf(w, "not attempted: %d asset(s), because the run stopped early\n", rep.NotAttempted)
	}
	// The per-asset warnings above say which ones and why. This says what it means: the
	// lockfile is what the next run reads, so an unrecorded package is work that will be
	// done again — and for a relocation, done by re-downloading the whole thing.
	if rep.Unrecorded > 0 {
		fmt.Fprintf(w, "%d asset(s) are in the cache but not in the lockfile, so the next "+
			"run will redo that work; fix whatever stopped the write and run again\n", rep.Unrecorded)
	}
	// A tally of one among hundreds of owned assets does not tell the user which package
	// the store stopped serving, and that is the only thing they can act on.
	for _, r := range rep.Results {
		if r.Class == syncer.Undownloadable {
			fmt.Fprintf(w, "delisted, cannot be downloaded: %s (%s), state %s\n",
				r.Asset.Name, r.Asset.ID, r.Asset.State)
		}
	}
	// A dropped asset leaves its bytes on disk; the summary names them so the user can
	// decide. Losing ownership of an asset never deletes its package: a run only ever
	// removes a copy it is replacing with a newer one of that same asset.
	for _, e := range rep.Removed {
		if e.CachePath != "" {
			fmt.Fprintf(w, "no longer owned: %s — %s (%d bytes) left in place\n", e.Name, e.CachePath, e.SizeBytes)
		} else {
			fmt.Fprintf(w, "no longer owned: %s\n", e.Name)
		}
	}
	for _, e := range rep.Unknown {
		fmt.Fprintf(w, "manifest lists asset %s (%s), which this account does not own\n", e.ID, e.Name)
	}
}

func list(w io.Writer, lockPath string) error {
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		return err
	}
	if len(lf.Assets) == 0 {
		fmt.Fprintln(w, "no lockfile yet at", lockPath)
		return nil
	}
	keys := make([]string, 0, len(lf.Assets))
	for k := range lf.Assets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	mirrored := 0
	for _, k := range keys {
		e := lf.Assets[k]
		state := "owned"
		if e.Tracked {
			state = "mirrored"
			mirrored++
		}
		fmt.Fprintf(w, "%-10s %-10s %-8s %s\n", state, e.Version.Name, e.AssetID, e.Name)
	}
	fmt.Fprintf(w, "\n%d owned, %d mirrored\n", len(keys), mirrored)
	return nil
}

// usage is what an error prints: the command list and where to find the flags. The
// descriptions themselves would bury the diagnostic that follows.
func usage(w io.Writer) {
	commands(w)
	fmt.Fprintln(w, `Run "unity-sync <command> --help" for the flags.`)
}

// help is what an explicit --help prints, on stdout so it can be piped: the same list plus
// every flag's own description.
func help(w io.Writer, fs *flag.FlagSet) {
	commands(w)
	fmt.Fprintln(w, "Flags:")
	fs.SetOutput(w)
	fs.PrintDefaults()
}

func commands(w io.Writer) {
	fmt.Fprint(w, strings.TrimLeft(`
unity-sync mirrors the assets you own on the Unity Asset Store.

  unity-sync select    pick which assets to mirror (opens a local page)
  unity-sync status    what a sync would change; downloads nothing
  unity-sync sync      download the delta and update the lockfile
  unity-sync list      print the current lockfile
  unity-sync update    replace this binary with the latest release
  unity-sync version   print the installed version

`, "\n"))
}
