package session

import (
	"encoding/binary"
	"testing"
)

// The mozlz4 decoder is the tool's only hand-written binary parser, it runs over a file
// another program wrote, and every bound in it is hand-checked: the token nibbles and the
// 255-continuation in readLength, the literal run against len(src), the match offset
// against len(dst), the byte-at-a-time overlapping copy, and both places the output is
// measured against the header's declared size.
//
// The rest of the suite proves those against blocks the test file encoded itself, so it
// exercises the shapes a correct encoder produces rather than the shapes a corrupt file
// does — and TestDecodesARealSessionStore, the one test that reads a real artefact, skips
// unless UNITY_SYNC_REAL_SESSIONSTORE points at one. A fuzz target is what covers the
// space in between. CI runs it for a few seconds per build; a crash here is an index
// running past its slice on input a user could really have on disk.

func FuzzDecodeMozLZ4(f *testing.F) {
	hdr := func(size uint32, body []byte) []byte {
		b := append([]byte(mozlz4Magic), 0, 0, 0, 0)
		binary.LittleEndian.PutUint32(b[len(mozlz4Magic):], size)
		return append(b, body...)
	}
	f.Add(hdr(0, nil))
	f.Add(hdr(16, []byte{0xff}))
	f.Add(hdr(64, []byte{0xf0, 0xff, 0xff, 0xff, 0xff}))
	f.Add(hdr(32, []byte{0x0f, 0x00, 0x00}))
	f.Add(hdr(1024, []byte{0x1f, 'a', 0x01, 0x00, 0xff, 0xff}))
	f.Add(hdr(8, []byte{0x40, 'a', 'b', 'c', 'd', 0x00, 0x00}))
	f.Add([]byte(mozlz4Magic))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := decodeMozLZ4(data)
		// The header's declared size is a claim off disk, so it is a ceiling on what is
		// allocated as well as on what is returned.
		if err == nil && len(out) > 256<<20 {
			t.Fatalf("decoded %d bytes past the ceiling", len(out))
		}
	})
}

// Drives the block decoder directly, so the budget is not spent rediscovering the 12-byte
// container prefix.
func FuzzLZ4Block(f *testing.F) {
	f.Add([]byte{0xff}, 16)
	f.Add([]byte{0xf0, 0xff, 0xff, 0xff}, 64)
	f.Add([]byte{0x0f, 0x00, 0x00}, 32)
	f.Add([]byte{0x1f, 'a', 0x01, 0x00, 0xff}, 1024)
	f.Add([]byte{0x40, 'a', 'b', 'c', 'd', 0x00, 0x00}, 8)
	f.Add([]byte{}, 0)

	f.Fuzz(func(t *testing.T, src []byte, want int) {
		if want < 0 || want > 1<<20 {
			t.Skip()
		}
		out, err := lz4Decompress(src, want)
		// Returning nil means the block decoded to exactly what the header declared.
		// Anything else has to be an error, or a caller reads a short buffer as a session.
		if err == nil && len(out) != want {
			t.Fatalf("no error but produced %d bytes for a declared %d", len(out), want)
		}
	})
}
