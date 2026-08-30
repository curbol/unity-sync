// Package manifest is the committed project manifest, unity-sync.toml: the allowlist of
// assets a project draws from. It is discovered by walking up from the working directory
// and lives with the consuming project, not with the tool, and it carries no account
// identity.
//
// Only `select` ever writes it.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/curbol/unity-sync/internal/model"
)

// FileName is what Discover looks for.
const FileName = "unity-sync.toml"

// Entry is one asset's selection state. It keys on ID because that is the only stable
// identity: a publisher rename changes Name, and matching on the name would silently
// reset a selection to disabled.
type Entry struct {
	ID      string `toml:"id"`
	Name    string `toml:"name"`
	Enabled bool   `toml:"enabled"`
}

// Manifest is the whole file.
type Manifest struct {
	Assets []Entry `toml:"asset"`
}

// Discover walks up from startDir looking for the manifest, returning its path and true
// on the first hit.
func Discover(startDir string) (string, bool) {
	dir := startDir
	for {
		p := filepath.Join(dir, FileName)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// LockPath is the lockfile that belongs beside a manifest.
func LockPath(manifestPath string) string {
	return strings.TrimSuffix(manifestPath, ".toml") + ".lock.json"
}

// Load reads the manifest, returning an empty one if the file does not exist. An unknown
// key is an error rather than a shrug: Save re-encodes from the struct, so a key that
// decodes to nothing here would be deleted from the user's file on the next write.
func Load(path string) (Manifest, error) {
	var m Manifest
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return m, nil
	}
	md, err := toml.DecodeFile(path, &m)
	if err != nil {
		return Manifest{}, err
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, 0, len(un))
		for _, k := range un {
			keys = append(keys, k.String())
		}
		return Manifest{}, fmt.Errorf("%s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	for i, e := range m.Assets {
		if e.ID == "" {
			return Manifest{}, fmt.Errorf("%s: [[asset]] #%d has no id", path, i+1)
		}
	}
	return m, nil
}

// Save writes the manifest atomically, entries sorted by id for a stable diff.
func Save(path string, m Manifest) error {
	// Sort a copy: the value receiver shares the caller's backing array, so sorting in
	// place would reorder their slice behind their back.
	m.Assets = append([]Entry(nil), m.Assets...)
	sort.Slice(m.Assets, func(i, j int) bool { return m.Assets[i].ID < m.Assets[j].ID })

	tmp, err := os.CreateTemp(filepath.Dir(path), ".unity-sync-manifest-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := toml.NewEncoder(tmp).Encode(m); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	// Flushed before the rename: this file is committed and hand-curated, and a rename that
	// returns before the bytes are durable can leave it empty after a crash.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	// CreateTemp makes the file 0600 and the rename carries that over, which would quietly
	// strip group and other from a file this design wants committed and hand-edited.
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// ErrWouldEmpty is returned by Reconcile when the owned set is empty but the manifest is
// not. An enumeration that legitimately returns nothing is indistinguishable from one
// made with the wrong org active, and this is the only file in the system a user curates
// by hand.
type ErrWouldEmpty struct{ Existing int }

func (e *ErrWouldEmpty) Error() string {
	return fmt.Sprintf("refusing to rewrite the manifest: the store reported no owned assets while "+
		"the manifest holds %d (a session with a different active org looks exactly like this)", e.Existing)
}

// Reconcile rebuilds the allowlist against what is currently owned: existing selections
// are preserved by id, newly-owned assets are added disabled, and assets no longer owned
// drop out. It returns the entries it dropped so the caller can report them rather than
// removing them silently.
//
// Selection is opt-in: buying an asset must never cause it to download on the next run.
func (m *Manifest) Reconcile(owned []model.Asset) (dropped []Entry, err error) {
	if len(owned) == 0 && len(m.Assets) > 0 {
		return nil, &ErrWouldEmpty{Existing: len(m.Assets)}
	}
	prev := make(map[string]Entry, len(m.Assets))
	for _, e := range m.Assets {
		prev[e.ID] = e
	}
	out := make([]Entry, 0, len(owned))
	stillOwned := make(map[string]bool, len(owned))
	for _, a := range owned {
		stillOwned[a.ID] = true
		out = append(out, Entry{ID: a.ID, Name: a.Name, Enabled: prev[a.ID].Enabled})
	}
	for _, e := range m.Assets {
		if !stillOwned[e.ID] {
			dropped = append(dropped, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	m.Assets = out
	return dropped, nil
}

// EnabledIDs is the set a run may act on.
func (m Manifest) EnabledIDs() map[string]bool {
	set := map[string]bool{}
	for _, e := range m.Assets {
		if e.Enabled {
			set[e.ID] = true
		}
	}
	return set
}

// SetEnabled applies an id->enabled selection, as the select page returns it.
func (m *Manifest) SetEnabled(enabled map[string]bool) {
	for i := range m.Assets {
		m.Assets[i].Enabled = enabled[m.Assets[i].ID]
	}
}

// UnknownIDs returns manifest entries naming assets the account does not own. They are
// reported rather than ignored: a refunded asset, a typo, or an id invisible to the
// current session's org would otherwise produce complete silence from status and sync.
func (m Manifest) UnknownIDs(owned []model.Asset) []Entry {
	ownedIDs := make(map[string]bool, len(owned))
	for _, a := range owned {
		ownedIDs[a.ID] = true
	}
	var unknown []Entry
	for _, e := range m.Assets {
		if !ownedIDs[e.ID] {
			unknown = append(unknown, e)
		}
	}
	return unknown
}
