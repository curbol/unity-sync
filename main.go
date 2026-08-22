// Command unity-sync mirrors the assets owned on the Unity Asset Store into a local
// library, downloading only what changed since the last run.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/curbol/unity-sync/internal/config"
	"github.com/curbol/unity-sync/internal/lockfile"
	"github.com/curbol/unity-sync/internal/manifest"
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
		usage()
		return 1, errors.New("a subcommand is required")
	}
	cmd, rest := args[0], args[1:]

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgDir := fs.String("config", "", "user config dir (default $XDG_CONFIG_HOME/unity-sync)")
	manifestFlag := fs.String("manifest", "", "project manifest path (default: nearest unity-sync.toml walking up)")
	sessionFlag := fs.String("session", "", "session file: a pasted-curl or cookies.txt export")
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
		usage()
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
			usage()
		}
		return 0, nil
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage()
			return 0, nil
		}
		return 1, err
	}

	// flag.Parse stops at the first non-flag argument, so an unchecked positional would
	// silently swallow every flag after it: `sync foo --dry-run` would download.
	if cmd == "update" {
		if fs.NArg() > 1 {
			return 1, fmt.Errorf("update takes at most one version, got %d arguments", fs.NArg())
		}
		return 0, selfupdate.Run(version, fs.Arg(0))
	}
	if fs.NArg() > 0 {
		return 1, fmt.Errorf("%s takes no positional arguments (got %q); to limit assets use --only %s",
			cmd, fs.Arg(0), fs.Arg(0))
	}

	configDir := config.ResolveDir(*cfgDir)
	cfg, err := config.Load(configDir)
	if err != nil {
		return 1, err
	}
	if *library != "" {
		cfg.LibraryPath = *library
	}
	if *concurrency > 0 {
		cfg.Concurrency = *concurrency
	}
	if *sessionFlag != "" {
		cfg.SessionSource = *sessionFlag
	}

	manifestPath, err := resolveManifest(*manifestFlag, cmd)
	if err != nil {
		return 1, err
	}
	lockPath := manifest.LockPath(manifestPath)

	if cmd == "list" {
		return 0, list(stdout, lockPath)
	}

	cookie, err := resolveSession(cfg, configDir)
	if err != nil {
		return 1, err
	}
	client := store.New(cookie, version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := client.Bootstrap(ctx); err != nil {
		return 1, err
	}

	if cmd == "select" {
		return 0, selectAssets(ctx, client, manifestPath, *addr)
	}
	return syncOrStatus(ctx, client, cfg, manifestPath, lockPath, *only, *verify, cmd == "status" || *dryRun)
}

// resolveManifest finds the project manifest. Every command needs one except select,
// which creates it in the working directory when no ancestor has one.
func resolveManifest(flagValue, cmd string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
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

func resolveSession(cfg config.Config, configDir string) (string, error) {
	src := cfg.SessionSource
	if src == "" {
		if found, ok := session.Discover(configDir); ok {
			src = found
		}
	}
	if src == "" {
		return "", fmt.Errorf("no session configured: save a pasted-curl file as %s, "+
			"set session_source in config.toml, or pass --session",
			filepath.Join(configDir, "session.curl"))
	}
	return session.Resolve(src)
}

func selectAssets(ctx context.Context, client *store.Client, manifestPath, addr string) error {
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
	for _, e := range dropped {
		fmt.Fprintf(os.Stderr, "no longer owned, dropping from the manifest: %s (%s)\n", e.Name, e.ID)
	}
	chosen, err := web.Serve(ctx, addr, owned, m.EnabledIDs())
	if err != nil {
		return err
	}
	m.SetEnabled(chosen)
	if err := manifest.Save(manifestPath, m); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "saved %d selected asset(s) to %s\n", len(chosen), manifestPath)
	return nil
}

func syncOrStatus(ctx context.Context, client *store.Client, cfg config.Config,
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
		return 1, err
	}
	printReport(stdout, rep, dry, cfg.LibraryPath)
	if rep.Failed() {
		return 1, nil
	}
	return 0, nil
}

func printReport(w io.Writer, rep syncer.Report, dry bool, libraryPath string) {
	counts := map[string]int{}
	for _, r := range rep.Results {
		counts[r.Class.String()]++
	}
	verb := "sync"
	if dry {
		verb = "status (no changes made)"
	}
	fmt.Fprintf(w, "%s: %d asset(s) considered\n", verb, len(rep.Results))
	for _, class := range []string{"new", "changed", "download-now", "cache-missing", "adopted", "unchanged", "undownloadable"} {
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
	// A dropped asset leaves its bytes on disk; the summary names them so the user can
	// decide, because this tool never deletes a package it once mirrored.
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

func usage() {
	fmt.Fprint(os.Stderr, strings.TrimLeft(`
unity-sync mirrors the assets you own on the Unity Asset Store.

  unity-sync select    pick which assets to mirror (opens a local page)
  unity-sync status    what a sync would change; downloads nothing
  unity-sync sync      download the delta and update the lockfile
  unity-sync list      print the current lockfile
  unity-sync update    replace this binary with the latest release
  unity-sync version   print the installed version

Flags: --config --manifest --session --library --only --concurrency --verify
       --dry-run --addr
`, "\n"))
}
