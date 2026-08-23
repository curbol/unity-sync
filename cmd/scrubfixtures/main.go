// Command scrubfixtures regenerates testdata/store from the raw captures kept outside
// version control. Run it from the repo root; it is the only supported way to change
// those fixtures, because hand-editing them is how account data gets committed.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/curbol/unity-sync/internal/fixtures"
)

func main() {
	from := flag.String("from", "captures", "directory holding the raw captures")
	to := flag.String("to", "testdata/store", "directory to write scrubbed fixtures into")
	flag.Parse()

	if err := run(*from, *to); err != nil {
		fmt.Fprintln(os.Stderr, "scrubfixtures:", err)
		os.Exit(1)
	}
}

func run(from, to string) error {
	matches, err := filepath.Glob(filepath.Join(from, "*.json"))
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return fmt.Errorf("no captures in %s: this needs the raw SearchMyAssets responses from a "+
			"signed-in session, one .json per page, saved there yourself. They are git-excluded "+
			"because every row carries an entitlement id — see docs/design.md", from)
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	for _, src := range matches {
		raw, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		clean, err := fixtures.Scrub(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		dst := filepath.Join(to, filepath.Base(src))
		if err := os.WriteFile(dst, clean, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes)\n", dst, len(clean))
	}
	return nil
}
