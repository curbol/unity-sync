package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/curbol/unity-sync/internal/config"
)

// isolate clears every variable the resolver reads, so a developer's own environment
// cannot make these pass or fail. It returns the home directory it pinned.
func isolate(t *testing.T) string {
	t.Helper()
	for _, k := range []string{"UNITY_SYNC_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
		"UNITY_SYNC_LIBRARY", "UNITY_SYNC_SESSION"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	home := t.TempDir()
	// os.UserHomeDir reads USERPROFILE on Windows and HOME everywhere else, so both are
	// pinned: setting only one leaves the resolver pointed at the real account.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func TestResolveDirPrecedence(t *testing.T) {
	home := isolate(t)

	if got, want := config.ResolveDir(""), filepath.Join(home, ".config", "unity-sync"); got != want {
		t.Errorf("bare default = %q, want %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got, want := config.ResolveDir(""), filepath.Join("/xdg", "unity-sync"); got != want {
		t.Errorf("XDG_CONFIG_HOME = %q, want %q", got, want)
	}
	t.Setenv("UNITY_SYNC_CONFIG_DIR", "/envdir")
	if got := config.ResolveDir(""); got != "/envdir" {
		t.Errorf("UNITY_SYNC_CONFIG_DIR = %q, want /envdir (env beats XDG)", got)
	}
	if got := config.ResolveDir("/flagdir"); got != "/flagdir" {
		t.Errorf("flag = %q, want /flagdir (flag beats env)", got)
	}
}

func TestLoadDefaultsWhenNoFileExists(t *testing.T) {
	home := isolate(t)
	c, err := config.Load(t.TempDir(), config.Flags{})
	if err != nil {
		t.Fatalf("Load with no config.toml: %v", err)
	}
	if c.Concurrency != 2 {
		t.Errorf("Concurrency = %d, want 2", c.Concurrency)
	}
	if c.SessionSource != "" {
		t.Errorf("SessionSource = %q, want empty: there is no browser default", c.SessionSource)
	}
	want := filepath.Join(home, ".local", "share", "unity-sync")
	if c.LibraryPath != want {
		t.Errorf("LibraryPath = %q, want %q", c.LibraryPath, want)
	}
}

func TestFileThenEnvPrecedence(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	body := "session_source = \"/from/file.curl\"\nlibrary_path = \"/from/file/lib\"\nconcurrency = 7\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := config.Load(dir, config.Flags{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SessionSource != "/from/file.curl" || c.LibraryPath != "/from/file/lib" || c.Concurrency != 7 {
		t.Fatalf("file values not applied: %+v", c)
	}

	t.Setenv("UNITY_SYNC_LIBRARY", "/from/env/lib")
	t.Setenv("UNITY_SYNC_SESSION", "/from/env.curl")
	c, err = config.Load(dir, config.Flags{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LibraryPath != "/from/env/lib" {
		t.Errorf("LibraryPath = %q, want the env value to beat the file", c.LibraryPath)
	}
	if c.SessionSource != "/from/env.curl" {
		t.Errorf("SessionSource = %q, want the env value to beat the file", c.SessionSource)
	}
	if c.Concurrency != 7 {
		t.Errorf("Concurrency = %d, want the file value to survive (no env override exists)", c.Concurrency)
	}
}

// Every source of a path gets the same expansion. A flag applied after Load returned
// would skip it, and since filepath never expands "~", `--library '~/packages'` from a
// script the shell did not expand would mirror a 75 GB library into a directory literally
// named "~" under the working directory, silently and without an error.
func TestTildeExpandsFromEverySource(t *testing.T) {
	t.Run("config.toml", func(t *testing.T) {
		home := isolate(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.toml"),
			[]byte("library_path = \"~/packages\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := config.Load(dir, config.Flags{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := filepath.Join(home, "packages"); c.LibraryPath != want {
			t.Errorf("LibraryPath = %q, want %q", c.LibraryPath, want)
		}
	})

	t.Run("environment", func(t *testing.T) {
		home := isolate(t)
		t.Setenv("UNITY_SYNC_LIBRARY", "~/packages")
		c, err := config.Load(t.TempDir(), config.Flags{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := filepath.Join(home, "packages"); c.LibraryPath != want {
			t.Errorf("LibraryPath = %q, want %q", c.LibraryPath, want)
		}
	})

	t.Run("flags", func(t *testing.T) {
		home := isolate(t)
		c, err := config.Load(t.TempDir(), config.Flags{
			LibraryPath:   "~/packages",
			SessionSource: "~/session.curl",
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := filepath.Join(home, "packages"); c.LibraryPath != want {
			t.Errorf("LibraryPath = %q, want %q", c.LibraryPath, want)
		}
		if want := filepath.Join(home, "session.curl"); c.SessionSource != want {
			t.Errorf("SessionSource = %q, want %q", c.SessionSource, want)
		}
	})

	t.Run("config dir", func(t *testing.T) {
		home := isolate(t)
		if got, want := config.ResolveDir("~/cfg"), filepath.Join(home, "cfg"); got != want {
			t.Errorf("ResolveDir(flag) = %q, want %q", got, want)
		}
		t.Setenv("UNITY_SYNC_CONFIG_DIR", "~/envcfg")
		if got, want := config.ResolveDir(""), filepath.Join(home, "envcfg"); got != want {
			t.Errorf("ResolveDir($UNITY_SYNC_CONFIG_DIR) = %q, want %q", got, want)
		}
	})

	t.Run("library default from XDG_DATA_HOME", func(t *testing.T) {
		home := isolate(t)
		t.Setenv("XDG_DATA_HOME", "~/data")
		c, err := config.Load(t.TempDir(), config.Flags{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := filepath.Join(home, "data", "unity-sync"); c.LibraryPath != want {
			t.Errorf("LibraryPath = %q, want %q", c.LibraryPath, want)
		}
	})
}

func TestMalformedConfigIsAnError(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("concurrency = \"two\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(dir, config.Flags{}); err == nil {
		t.Error("Load accepted a malformed config.toml")
	}
}
