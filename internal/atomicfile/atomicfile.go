// Package atomicfile replaces a committed file in one step: write a temp beside it, flush
// it, rename it over the destination, then flush the directory. The lockfile and the
// project manifest both travel with the consuming project, so a reader must never see a
// half-written one and a crash must never leave the previous one truncated or the new one
// unnamed.
package atomicfile

import (
	"io"
	"os"
	"path/filepath"
)

// syncFile and syncDir are indirected so a test can watch each flush happen, and happen on
// the right side of the rename. Neither has an effect observable on a machine that does
// not lose power, so deleting either call leaves every other assertion green.
var (
	syncFile = (*os.File).Sync
	syncDir  = (*os.File).Sync
)

// Write replaces path with whatever write emits. The temp is created from tempPattern in
// path's own directory, so the rename is same-filesystem, and it is removed on every
// failure path: the directory is one the user commits. The file keeps the mode path
// already had, or 0644 when it is being created, because os.CreateTemp opens at 0600 and
// the rename carries that over.
//
// The file flush is what makes the rename mean something: renaming is atomic against a
// concurrent reader but says nothing about durability, so without it a crash can leave a
// full-length file of zeros where the record was. The directory flush after it is what
// makes the new name durable, so a crash cannot leave the directory still naming the file
// the rename replaced.
func Write(path, tempPattern string, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), tempPattern)
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(modeOf(path)); err != nil {
		return fail(err)
	}
	if err := write(tmp); err != nil {
		return fail(err)
	}
	if err := syncFile(tmp); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	// On Windows the rename fails outright when the destination is held open by an
	// editor or an on-access scanner, so this failure is an ordinary one there.
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	SyncDir(filepath.Dir(path))
	return nil
}

// SyncDir flushes the directory entries a rename changed.
//
// Best-effort: Windows hands out no syncable handle for a directory, and by the time this
// runs the rename has happened, so failing here would report as lost a write that landed.
func SyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = syncDir(d)
	_ = d.Close()
}

// SyncDirIn is SyncDir for a directory named inside rt, opened through the root like
// every other operation on a path the caller has confined to it.
func SyncDirIn(rt *os.Root, name string) {
	d, err := rt.Open(name)
	if err != nil {
		return
	}
	_ = syncDir(d)
	_ = d.Close()
}

func modeOf(path string) os.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o644
}
