package atomicfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func emit(s string) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	}
}

func stub(t *testing.T, v *func(*os.File) error, f func(*os.File) error) {
	t.Helper()
	prev := *v
	*v = f
	t.Cleanup(func() { *v = prev })
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The lockfile is written once per resolved asset precisely so a kill at asset 90 of 100
// keeps the 89, and bytes still in the page cache when the rename returns are exactly the
// record that loses. Ordering is checked by reading the destination from inside the flush:
// before the rename it still holds the previous write.
func TestWriteFlushesTheFileBeforeItRenames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unity-sync.lock.json")
	if err := Write(path, ".tmp-*", emit("first")); err != nil {
		t.Fatal(err)
	}

	var calls int
	var atFlush string
	stub(t, &syncFile, func(f *os.File) error {
		calls++
		atFlush = readFile(t, path)
		return f.Sync()
	})
	if err := Write(path, ".tmp-*", emit("second")); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("Write flushed the file %d times, want 1: without it the rename can outrun the bytes", calls)
	}
	if atFlush != "first" {
		t.Errorf("the destination held %q when the flush ran, so the rename happened first "+
			"and the flush no longer protects anything", atFlush)
	}
}

// The file flush makes the bytes durable and says nothing about the name: until the
// directory is flushed, a crash can leave it naming the file the rename replaced. So the
// directory flush has to see the new name already in place, and has to be of the
// directory that holds it.
func TestWriteFlushesTheDirectoryAfterItRenames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unity-sync.toml")
	if err := Write(path, ".tmp-*", emit("first")); err != nil {
		t.Fatal(err)
	}

	var flushed []string
	var atFlush string
	stub(t, &syncDir, func(d *os.File) error {
		flushed = append(flushed, d.Name())
		atFlush = readFile(t, path)
		return nil
	})
	if err := Write(path, ".tmp-*", emit("second")); err != nil {
		t.Fatal(err)
	}
	if len(flushed) != 1 || flushed[0] != dir {
		t.Fatalf("flushed directories %q, want exactly %q", flushed, dir)
	}
	if atFlush != "second" {
		t.Errorf("the destination held %q when the directory was flushed; it must follow the rename", atFlush)
	}
}

// Windows refuses to sync a directory handle, and by then the write has landed.
func TestADirectoryFlushThatFailsDoesNotFailTheWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unity-sync.toml")
	stub(t, &syncDir, func(*os.File) error { return errors.New("access denied") })
	if err := Write(path, ".tmp-*", emit("landed")); err != nil {
		t.Fatalf("Write reported %v for a write that landed", err)
	}
	if got := readFile(t, path); got != "landed" {
		t.Errorf("content = %q, want %q", got, "landed")
	}
}

// A flush that fails is a write that did not reach the disk, so it has to fail rather than
// be renamed into place, and it must not leave its temp in a directory the user commits.
func TestAFailedFlushLeavesThePriorFileAndNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unity-sync.lock.json")
	if err := Write(path, ".tmp-*", emit("prior")); err != nil {
		t.Fatal(err)
	}
	stub(t, &syncFile, func(*os.File) error { return errors.New("disk went away") })
	if err := Write(path, ".tmp-*", emit("next")); err == nil {
		t.Fatal("Write reported success after the flush failed")
	}
	if got := readFile(t, path); got != "prior" {
		t.Errorf("content = %q, want the prior file untouched", got)
	}
	assertOnly(t, dir, "unity-sync.lock.json")
}

// The temp has to live in the destination's own directory, or the rename crosses a
// filesystem and fails with EXDEV on the ordinary Linux layout: a tmpfs /tmp and the
// project on /home. t.TempDir() and $TMPDIR are one filesystem under test, so nothing
// else here would notice.
func TestAFailedEncodeLeavesThePriorFileAndNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unity-sync.toml")
	if err := Write(path, ".tmp-*", emit("prior")); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("encoder gave up")
	err := Write(path, ".tmp-*", func(w io.Writer) error {
		named, ok := w.(interface{ Name() string })
		if !ok {
			t.Fatalf("Write handed the encoder a %T, which cannot be asked where it lives", w)
		}
		if got := filepath.Dir(named.Name()); got != dir {
			t.Errorf("temp is in %q, want %q; a rename out of there fails with EXDEV", got, dir)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the encoder's own error", err)
	}
	if got := readFile(t, path); got != "prior" {
		t.Errorf("content = %q, want the prior file untouched", got)
	}
	assertOnly(t, dir, "unity-sync.toml")
}

func TestWriteKeepsTheModeOfTheFileItRewrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports 0666 for every writable file; there are no mode bits to keep")
	}
	dir := t.TempDir()
	created := filepath.Join(dir, "new.json")
	if err := Write(created, ".tmp-*", emit("x")); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dir, "existing.toml")
	if err := os.WriteFile(existing, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Write(existing, ".tmp-*", emit("new")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{created: 0o644, existing: 0o640} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode = %v, want %v", filepath.Base(path), got, want)
		}
	}
}

func TestSyncDirInFlushesTheDirectoryTheRootNames(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pub", "asset"), 0o755); err != nil {
		t.Fatal(err)
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var flushed []string
	stub(t, &syncDir, func(d *os.File) error {
		fi, err := d.Stat()
		if err != nil || !fi.IsDir() {
			t.Errorf("flushed %q, which is not a directory", d.Name())
		}
		flushed = append(flushed, d.Name())
		return nil
	})
	SyncDirIn(rt, filepath.FromSlash("pub/asset"))
	if want := filepath.Join(root, "pub", "asset"); len(flushed) != 1 || flushed[0] != want {
		t.Errorf("flushed %q, want exactly the one directory named", flushed)
	}
}

func assertOnly(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != name {
			t.Errorf("%q was left beside %s", e.Name(), name)
		}
	}
}
