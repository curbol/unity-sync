// Package humanize renders machine values for people. It exists as its own package
// because the two callers that need it, the run summary and the select page, sit in
// different layers and neither is a sensible owner for the other's formatting.
package humanize

import "fmt"

// units are the SI prefixes Bytes steps through. The store reports sizes in decimal, and
// so does the Asset Store page, so a kilobyte here is 1000 bytes rather than 1024.
const units = "kMGTPE"

// Bytes renders a byte count in decimal SI units.
//
// The unit index is clamped because n arrives from the store's downloadSize, a JSON
// string this tool does not get to choose. Indexing the unit table with an unbounded
// exponent panics on a large enough value, and one of the two callers formats inside a
// download goroutine, where a panic takes the whole run down rather than one asset.
func Bytes(n int64) string {
	const step = 1000
	if n < step {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(step), 0
	for v := n / step; v >= step; v /= step {
		div *= step
		exp++
		if exp == len(units)-1 {
			break
		}
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), units[exp])
}
