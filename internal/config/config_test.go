package config_test

import (
	"os"
	"path/filepath"
	"strings"
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

// A key that decodes to nothing is almost always a misspelling of one that would have
// decoded, and the manifest already refuses those. Silence is more expensive here:
// `library-path` for `library_path` mirrors tens of gigabytes into the default directory
// with no diagnostic at all.
func TestAnUnknownConfigKeyIsRefusedRatherThanIgnored(t *testing.T) {
	dir := t.TempDir()
	body := "library-path = \"/mnt/big/unity\"\nconcurrency = 4\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(dir, config.Flags{})
	if err == nil {
		t.Fatal("Load accepted a misspelled key and silently kept the default library path")
	}
	if !strings.Contains(err.Error(), "library-path") {
		t.Errorf("error %q does not name the key that did not decode", err)
	}
}

// With no home and no XDG variable there is nowhere to put a 75 GB mirror, and the old
// answer was a relative "unity-library" under whatever directory the user happened to be
// in. An error naming the four ways to say where it should go is the better outcome.
func TestNoHomeAndNoXDGRefusesRatherThanPickingARelativeLibrary(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("UNITY_SYNC_LIBRARY", "")

	if _, err := config.Load(t.TempDir(), config.Flags{}); err == nil {
		t.Fatal("Load invented a library path with no home directory to put one under")
	}

	// But a run that says where its library is has no use for a home directory at all.
	cfg, err := config.Load(t.TempDir(), config.Flags{LibraryPath: "/mnt/big/unity"})
	if err != nil {
		t.Fatalf("Load with an explicit --library still needed a home: %v", err)
	}
	if cfg.LibraryPath != "/mnt/big/unity" {
		t.Errorf("LibraryPath = %q, want the flag's value", cfg.LibraryPath)
	}
}

// A config.toml that exists but cannot be read must not look like one that is absent.
// Load's own contract says a missing file is fine and an unreadable one is not, and the
// difference is expensive: dropping the file silently loses library_path and mirrors tens
// of gigabytes into the default directory with no diagnostic.
func TestAnUnreadableConfigIsAnErrorRatherThanNoConfig(t *testing.T) {
	dir := t.TempDir()
	// --config pointed at the file rather than the directory holding it, which the flag's
	// own help text ("user config dir") invites. Statting <file>/config.toml is ENOTDIR.
	notADir := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(notADir, []byte("library_path = \"/mnt/big\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(notADir, config.Flags{LibraryPath: "/tmp/x"}); err == nil {
		t.Error("Load treated an unreadable config dir as an absent config")
	}

	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file regardless")
	}
	locked := t.TempDir()
	if err := os.WriteFile(filepath.Join(locked, "config.toml"), []byte("concurrency = 4\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(locked, config.Flags{LibraryPath: "/tmp/x"}); err == nil {
		t.Error("Load silently dropped a config.toml it had no permission to read")
	}
}
