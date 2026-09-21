// Package config resolves the user-scoped settings: where the session comes from, where
// the library lives, and how many downloads may run at once. Built-in defaults are
// overridden by config.toml in the XDG config dir, then by environment, then by flags.
// Project-scoped settings (the asset allowlist) live in the manifest, not here, and no
// machine-specific path is baked in.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the resolved user-scoped configuration.
type Config struct {
	// SessionSource is "browser" to read a signed-in Firefox-family session, or a path to
	// a pasted-curl file, a cookies.txt, a browser profile, or a recovery.jsonlz4.
	SessionSource string

	// LibraryPath is where packages are mirrored. A user may point this at Unity's own
	// Asset Store-5.x directory; whether the Editor recognises the layout is untested.
	LibraryPath string

	// Concurrency bounds simultaneous downloads. Two by default: packages here reach
	// 23 GB, and the store is someone else's infrastructure.
	Concurrency int
}

type fileConfig struct {
	SessionSource string `toml:"session_source"`
	LibraryPath   string `toml:"library_path"`
	Concurrency   int    `toml:"concurrency"`
}

// Flags are the command-line overrides. They are the highest-precedence source, and they
// are applied here rather than by the caller so that every level of the chain gets the
// same treatment: a flag assigned after Load returned would skip ExpandHome, and a
// `--library '~/lib'` the shell did not expand would mirror into a directory named "~".
type Flags struct {
	LibraryPath   string
	SessionSource string
	Concurrency   int
}

// ResolveDir picks the directory holding config.toml: an explicit flag, else
// $UNITY_SYNC_CONFIG_DIR, else $XDG_CONFIG_HOME/unity-sync, else ~/.config/unity-sync.
// A path from any of these can be written with a leading "~", which no shell expands when
// it comes out of an environment variable, so each one is expanded here rather than
// reaching filepath as a directory literally named "~".
//
// A directory the user named has to exist; one this function fell back to does not. That
// asymmetry is the whole reason provenance is settled here rather than in Load, which sees
// only a string. An absent config is the ordinary first-run case, so Load treats it as
// nothing to read — and a misspelled --config is indistinguishable from it, which drops
// library_path and mirrors tens of gigabytes into the default directory, announced only by
// the "library:" line the summary prints once the download pass is over. It is the same
// harm the unreadable-file and unknown-key checks in Load already refuse, arriving by the
// likeliest route of the three.
//
// With no home and no XDG variable it falls back to a relative "unity-sync". That only
// ever decides where an optional file is looked for, so a wrong answer costs a config
// that is not found rather than data written somewhere unintended, which is why this one
// degrades where defaultLibraryPath refuses.
func ResolveDir(flag string) (string, error) {
	if flag != "" {
		return named(ExpandHome(flag), "--config")
	}
	if v := os.Getenv("UNITY_SYNC_CONFIG_DIR"); v != "" {
		return named(ExpandHome(v), "$UNITY_SYNC_CONFIG_DIR")
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(ExpandHome(v), "unity-sync"), nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "unity-sync"), nil
	}
	return "unity-sync", nil
}

// named checks a config directory the user chose. Load's own "not a directory" message
// covers the file case, so this one is only about a path that is not there at all.
func named(dir, source string) (string, error) {
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%s names %s, which does not exist; "+
				"a config directory that is not there reads as no config at all, so "+
				"library_path and session_source would both be silently ignored", source, dir)
		}
		return "", fmt.Errorf("%s names %s: %w", source, dir, err)
	}
	return dir, nil
}

// defaultLibraryPath is $XDG_DATA_HOME/unity-sync, else ~/.local/share/unity-sync. App
// data rather than ~/.cache, so an OS cache cleaner cannot wipe a 75 GB mirror.
//
// It refuses rather than falling back to a relative path. A mirror runs to tens of
// gigabytes, and writing that into whatever directory the user happened to be in is a
// worse outcome than an error naming the four ways to say where it should go.
func defaultLibraryPath() (string, error) {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(ExpandHome(v), "unity-sync"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to put the library under (%w): set XDG_DATA_HOME "+
			"or UNITY_SYNC_LIBRARY, put library_path in config.toml, or pass --library", err)
	}
	return filepath.Join(home, ".local", "share", "unity-sync"), nil
}

func defaults() Config {
	return Config{Concurrency: 2}
}

// Load merges built-in defaults, an optional config.toml in dir, then the environment
// (UNITY_SYNC_LIBRARY, UNITY_SYNC_SESSION), then flags. A missing config.toml is not an
// error; an unreadable or malformed one is.
//
// Every level runs through this one function so that none of them can be normalised
// differently from the others.
func Load(dir string, f Flags) (Config, error) {
	c := defaults()
	// Asked of the directory itself, because the errno from statting through it does not
	// answer the same way everywhere: Windows reports a path that runs through a regular
	// file as ERROR_PATH_NOT_FOUND, which errors.Is reads as fs.ErrNotExist, so the check
	// below cannot tell "--config named the file" from "there is no config here" on the
	// one platform where the flag's own wording ("user config dir") invites the mistake.
	// A dir that does not exist at all still falls through, which is the absent case.
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		return Config{}, fmt.Errorf("%s is not a directory: --config names the directory "+
			"holding config.toml, not the file itself", dir)
	}
	path := filepath.Join(dir, "config.toml")
	// Only a genuinely absent file is skipped. A permission error otherwise looks exactly
	// like "no config here": library_path is dropped and the run mirrors tens of gigabytes
	// into the default directory with no diagnostic, which is what the undecoded-key
	// check below exists to prevent.
	if _, err := os.Stat(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	} else if err == nil {
		var fc fileConfig
		md, err := toml.DecodeFile(path, &fc)
		if err != nil {
			return Config{}, err
		}
		// A key that decodes to nothing is almost always a misspelling of one that would
		// have decoded, and silence is expensive here: `library-path` for `library_path`
		// mirrors tens of gigabytes into the default directory with no diagnostic.
		if un := md.Undecoded(); len(un) > 0 {
			keys := make([]string, 0, len(un))
			for _, k := range un {
				keys = append(keys, k.String())
			}
			return Config{}, fmt.Errorf("%s: unknown key(s): %s", path, strings.Join(keys, ", "))
		}
		// The refusal --concurrency already gets, for the same reason: zero is not a number
		// of simultaneous downloads, and overlay's merge guard cannot tell a zero someone
		// typed from a key nobody wrote. Asked of the metadata rather than the value, so an
		// absent key stays the ordinary case.
		if md.IsDefined("concurrency") && fc.Concurrency < 1 {
			return Config{}, fmt.Errorf("%s: concurrency = %d is not a number of simultaneous "+
				"downloads; set 1 or more, or remove the key to use the default", path, fc.Concurrency)
		}
		overlay(&c, fc)
	}
	if v := os.Getenv("UNITY_SYNC_LIBRARY"); v != "" {
		c.LibraryPath = v
	}
	if v := os.Getenv("UNITY_SYNC_SESSION"); v != "" {
		c.SessionSource = v
	}
	if f.SessionSource != "" {
		c.SessionSource = f.SessionSource
	}
	if f.LibraryPath != "" {
		c.LibraryPath = f.LibraryPath
	}
	if f.Concurrency > 0 {
		c.Concurrency = f.Concurrency
	}
	// Last, and only when no level above supplied one: the default needs a home directory
	// and a run that names its own library has no use for one.
	if c.LibraryPath == "" {
		p, err := defaultLibraryPath()
		if err != nil {
			return Config{}, err
		}
		c.LibraryPath = p
	}
	c.LibraryPath = ExpandHome(c.LibraryPath)
	c.SessionSource = ExpandHome(c.SessionSource)
	return c, nil
}

func overlay(c *Config, fc fileConfig) {
	if fc.SessionSource != "" {
		c.SessionSource = fc.SessionSource
	}
	if fc.LibraryPath != "" {
		c.LibraryPath = fc.LibraryPath
	}
	if fc.Concurrency > 0 {
		c.Concurrency = fc.Concurrency
	}
}

// ExpandHome resolves a leading "~", which no shell expands when the value came out of
// an environment variable, a config file, or PowerShell's own tab completion.
//
// The separator is tested with os.IsPathSeparator rather than against "/", because the
// native spelling on Windows is `~\lib` and that is what tab completion there produces.
// Matching only "~/" leaves it alone, and the run then mirrors tens of gigabytes into a
// directory literally named "~". On Linux a backslash is an ordinary filename character
// and IsPathSeparator says so, so nothing changes there.
//
// Exported because --manifest is resolved outside this package and has to expand the same
// way the paths inside it do. Re-implementing it there is how one flag ends up matching
// only "~/" while the rest match both separators.
func ExpandHome(p string) string {
	if p == "~" || (len(p) > 1 && p[0] == '~' && os.IsPathSeparator(p[1])) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}
