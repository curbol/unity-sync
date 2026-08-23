// Package config resolves the user-scoped settings: where the session comes from, where
// the library lives, and how many downloads may run at once. Built-in defaults are
// overridden by config.toml in the XDG config dir, then by environment, then by flags.
// Project-scoped settings (the asset allowlist) live in the manifest, not here, and no
// machine-specific path is baked in.
package config

import (
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

// ResolveDir picks the directory holding config.toml: an explicit flag, else
// $UNITY_SYNC_CONFIG_DIR, else $XDG_CONFIG_HOME/unity-sync, else ~/.config/unity-sync.
func ResolveDir(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("UNITY_SYNC_CONFIG_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "unity-sync")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "unity-sync")
	}
	return "unity-sync"
}

// defaultLibraryPath is $XDG_DATA_HOME/unity-sync, else ~/.local/share/unity-sync. App
// data rather than ~/.cache, so an OS cache cleaner cannot wipe a 75 GB mirror.
func defaultLibraryPath() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "unity-sync")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "unity-sync")
	}
	return "unity-library"
}

func defaults() Config {
	return Config{
		LibraryPath: defaultLibraryPath(),
		Concurrency: 2,
	}
}

// Load merges built-in defaults, an optional config.toml in dir, then the environment
// (UNITY_SYNC_LIBRARY, UNITY_SYNC_SESSION). A missing config.toml is not an error; an
// unreadable or malformed one is.
func Load(dir string) (Config, error) {
	c := defaults()
	path := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(path); err == nil {
		var fc fileConfig
		if _, err := toml.DecodeFile(path, &fc); err != nil {
			return Config{}, err
		}
		overlay(&c, fc)
	}
	if v := os.Getenv("UNITY_SYNC_LIBRARY"); v != "" {
		c.LibraryPath = v
	}
	if v := os.Getenv("UNITY_SYNC_SESSION"); v != "" {
		c.SessionSource = v
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
