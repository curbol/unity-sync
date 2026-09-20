package fixtures

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
)

// Package builds a .unitypackage carrying the store's descriptor for productID at
// versionID, padded out to size bytes.
//
// It lives here, rather than beside each suite that needs one, because the FEXTRA layout
// it writes is the one internal/unitypackage parses, and three test packages each
// encoding it by hand is three things that have to move together when the layout does,
// with no compiler to say so. Miss one and that package's suite goes on passing against
// a package shape the reader no longer accepts.
//
// It builds only well-formed packages. A test that needs a malformed one should write the
// bytes itself, where the malformation is visible.
func Package(productID, versionID string, size int) []byte {
	descriptor := []byte(`{"id":"` + productID + `","version_id":"` + versionID + `"}`)
	extra := []byte{'A', '$', 0, 0}
	binary.LittleEndian.PutUint16(extra[2:4], uint16(len(descriptor)))
	extra = append(extra, descriptor...)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Header.Extra = extra
	zw.Write(bytes.Repeat([]byte("x"), 32))
	zw.Close()

	out := buf.Bytes()
	for len(out) < size {
		out = append(out, 0)
	}
	return out
}

// MozLZ4 wraps payload in the "mozLz40\0" container Gecko writes its session store in: the
// magic, a little-endian uint32 holding the decompressed size, then one LZ4 block.
//
// The block is all literals, which is a legal encoding of any input and the only one worth
// hand-writing. It lives here for the reason Package does: the container is a format this
// tool reads rather than writes, so a suite outside internal/session that needs a
// realistic session store would otherwise encode it a second time.
func MozLZ4(payload []byte) []byte {
	out := append([]byte("mozLz40\x00"), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(out[8:], uint32(len(payload)))

	n := len(payload)
	if n < 15 {
		out = append(out, byte(n<<4))
	} else {
		// A literal run of 15 or more sets the nibble to 15 and continues in whole bytes,
		// 255 meaning "and more".
		out = append(out, 0xF0)
		for rem := n - 15; ; {
			if rem < 255 {
				out = append(out, byte(rem))
				break
			}
			out = append(out, 0xFF)
			rem -= 255
		}
	}
	return append(out, payload...)
}
