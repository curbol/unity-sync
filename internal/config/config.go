// Package config resolves the user-scoped settings: where the session comes from, where
// the library lives, and how many downloads may run at once. Built-in defaults are
// overridden by config.toml in the XDG config dir, then by environment, then by flags.
// Project-scoped settings (the asset allowlist) live in the manifest, not here, and no
// machine-specific path is baked in.
package config

import (
	"fmt"
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
// same treatment: a flag assigned after Load returned would skip expandHome, and a
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
// With no home and no XDG variable it falls back to a relative "unity-sync". That only
// ever decides where an optional file is looked for, so a wrong answer costs a config
// that is not found rather than data written somewhere unintended, which is why this one
// degrades where defaultLibraryPath refuses.
func ResolveDir(flag string) string {
	if flag != "" {
		return expandHome(flag)
	}
	if v := os.Getenv("UNITY_SYNC_CONFIG_DIR"); v != "" {
		return expandHome(v)
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(expandHome(v), "unity-sync")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "unity-sync")
	}
	return "unity-sync"
}

// defaultLibraryPath is $XDG_DATA_HOME/unity-sync, else ~/.local/share/unity-sync. App
// data rather than ~/.cache, so an OS cache cleaner cannot wipe a 75 GB mirror.
//
// It refuses rather than falling back to a relative path. A mirror runs to tens of
// gigabytes, and writing that into whatever directory the user happened to be in is a
// worse outcome than an error naming the four ways to say where it should go.
func defaultLibraryPath() (string, error) {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(expandHome(v), "unity-sync"), nil
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
	path := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(path); err == nil {
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
	c.LibraryPath = expandHome(c.LibraryPath)
	c.SessionSource = expandHome(c.SessionSource)
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

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
