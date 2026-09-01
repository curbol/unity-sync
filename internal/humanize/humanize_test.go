package humanize

import (
	"strings"
	"testing"
)

// n comes from the store's downloadSize, a JSON string this tool does not choose. An
// unclamped unit index panics past the end of the table, and one caller formats inside a
// download goroutine where that takes the whole run down rather than one asset.
func TestBytesNeverPanicsOnAValueTheStoreCouldSend(t *testing.T) {
	for _, n := range []int64{0, 1, 999, 1000, 23 << 30, 999_999_999_999_999, 1_000_000_000_000_000, 1<<63 - 1} {
		got := Bytes(n)
		if got == "" {
			t.Errorf("Bytes(%d) = %q", n, got)
		}
		if !strings.HasSuffix(got, "B") {
			t.Errorf("Bytes(%d) = %q, want a byte unit", n, got)
		}
	}
}

func TestBytesRendersDecimalUnits(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{999, "999 B"},
		{1000, "1.0 kB"},
		{1_500_000, "1.5 MB"},
		{24_696_061_952, "24.7 GB"},
		{1_000_000_000_000_000, "1.0 PB"},
	} {
		if got := Bytes(tc.n); got != tc.want {
			t.Errorf("Bytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
