package session

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// mozlz4Magic prefixes every file Mozilla writes through its lz4 helper.
const mozlz4Magic = "mozLz40\x00"

// errNotMozLZ4 means the file is not one of Mozilla's compressed blobs. It is separate
// from a corruption error because a caller scanning several profiles wants to skip a
// stray file rather than fail the whole scan.
var errNotMozLZ4 = errors.New("not a mozlz4 file")

// decodeMozLZ4 unpacks the "mozLz40\0" container: the magic, a little-endian uint32
// holding the decompressed size, then a single raw LZ4 block.
//
// The block format is decoded here rather than pulled from a dependency. This is the
// tool's only compressed input and the format is a few dozen lines, against a module that
// would have to be vendored into a build that otherwise has one dependency.
func decodeMozLZ4(raw []byte) ([]byte, error) {
	if len(raw) < len(mozlz4Magic)+4 || string(raw[:len(mozlz4Magic)]) != mozlz4Magic {
		return nil, errNotMozLZ4
	}
	size := binary.LittleEndian.Uint32(raw[len(mozlz4Magic):])
	// The header is the only thing declaring the output length, and it comes off disk, so
	// it is a claim rather than a fact. Cap it: a session store runs to a few megabytes,
	// and a corrupt or hostile header should not turn into a gigabyte allocation.
	const maxDecompressed = 256 << 20
	if size > maxDecompressed {
		return nil, fmt.Errorf("mozlz4 header claims %d bytes, past the %d-byte ceiling", size, maxDecompressed)
	}
	return lz4Decompress(raw[len(mozlz4Magic)+4:], int(size))
}

// lz4Decompress expands one raw LZ4 block into exactly want bytes.
//
// A block is a sequence of sequences. Each starts with a token byte: the high nibble is
// the literal run length, the low nibble is the match length minus four. A nibble of 15
// means "add the following bytes until one is not 255". After the literals comes a
// little-endian uint16 back-reference offset into the output produced so far. The final
// sequence carries literals and stops, with no offset.
//
// Every index is bounds-checked against the input, and every match against the output
// already written, because this parses a file that another program wrote.
func lz4Decompress(src []byte, want int) ([]byte, error) {
	dst := make([]byte, 0, want)
	var i int

	readLength := func(n int) (int, error) {
		if n != 15 {
			return n, nil
		}
		for {
			if i >= len(src) {
				return 0, errors.New("lz4: length runs past the end of the block")
			}
			b := int(src[i])
			i++
			n += b
			if b != 255 {
				return n, nil
			}
			if n > want {
				return 0, errors.New("lz4: length exceeds the declared output size")
			}
		}
	}

	for i < len(src) {
		token := src[i]
		i++

		literals, err := readLength(int(token >> 4))
		if err != nil {
			return nil, err
		}
		if literals > 0 {
			if i+literals > len(src) {
				return nil, errors.New("lz4: literal run runs past the end of the block")
			}
			dst = append(dst, src[i:i+literals]...)
			i += literals
		}

		// The last sequence is literals only: no offset follows it.
		if i == len(src) {
			break
		}
		if i+2 > len(src) {
			return nil, errors.New("lz4: truncated match offset")
		}
		offset := int(binary.LittleEndian.Uint16(src[i:]))
		i += 2
		if offset == 0 || offset > len(dst) {
			return nil, fmt.Errorf("lz4: match offset %d is outside the %d bytes decoded so far", offset, len(dst))
		}

		length, err := readLength(int(token & 0x0F))
		if err != nil {
			return nil, err
		}
		length += 4 // the minimum match, which the encoder subtracts

		// Copied one byte at a time on purpose: a match may overlap the region it is
		// copying from, which is how the format encodes a repeating run, and a bulk copy
		// would read the pre-overlap bytes instead of the ones just written.
		start := len(dst) - offset
		for n := 0; n < length; n++ {
			dst = append(dst, dst[start+n])
		}
		if len(dst) > want {
			return nil, fmt.Errorf("lz4: output grew past the declared %d bytes", want)
		}
	}

	if len(dst) != want {
		return nil, fmt.Errorf("lz4: decoded %d bytes, header declared %d", len(dst), want)
	}
	return dst, nil
}
