// Package unitypackage reads the metadata the Asset Store stamps into every
// .unitypackage. A package is gzip, and the store puts a JSON descriptor in the gzip
// FEXTRA field — not the comment field, which is empty on every real package. Reading
// it costs a header parse, so a cached file can be identified without decompressing
// gigabytes or hashing them.
package unitypackage

import (
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// subfieldID is the extra-field id the store uses for its descriptor.
var subfieldID = [2]byte{'A', '$'}

// ErrNoMetadata means the file is a readable gzip stream that carries no store
// descriptor. Callers treat this as unverifiable-by-metadata rather than as corruption,
// because the alternative is refusing to cache a package that may be perfectly good.
var ErrNoMetadata = errors.New("no Asset Store metadata in gzip extra field")

// Metadata is the descriptor's fields, all of which the store encodes as JSON strings.
type Metadata struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Version      string `json:"version"`
	VersionID    string `json:"version_id"`
	UploadID     string `json:"upload_id"`
	UnityVersion string `json:"unity_version"`
	PubDate      string `json:"pubdate"`
}

// Read parses the gzip header from r and returns the store descriptor. It reads only as
// far as the header, so passing a whole multi-gigabyte package costs nothing extra.
func Read(r io.Reader) (Metadata, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return Metadata{}, fmt.Errorf("not a readable gzip stream: %w", err)
	}
	defer zr.Close()
	return fromExtra(zr.Header.Extra)
}

// ReadFile is Read against a path, reading only the header.
func ReadFile(path string) (Metadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return Metadata{}, err
	}
	defer f.Close()
	return Read(f)
}

// fromExtra walks the RFC 1952 subfield list. The walk is driven by each subfield's own
// length, never by an assumed prefix: XLEN is a uint16, so a descriptor may legitimately
// sit far past where every package sampled so far happens to end it, and a
// prefix-limited reader would silently report "no metadata" for such a package —
// downgrading the hard wrong-asset check into a warning.
func fromExtra(extra []byte) (Metadata, error) {
	for i := 0; i+4 <= len(extra); {
		id1, id2 := extra[i], extra[i+1]
		length := int(binary.LittleEndian.Uint16(extra[i+2 : i+4]))
		start := i + 4
		end := start + length
		if end > len(extra) {
			return Metadata{}, fmt.Errorf("extra subfield %q claims %d bytes but only %d remain",
				string([]byte{id1, id2}), length, len(extra)-start)
		}
		if id1 == subfieldID[0] && id2 == subfieldID[1] {
			var m Metadata
			if err := json.Unmarshal(extra[start:end], &m); err != nil {
				return Metadata{}, fmt.Errorf("metadata subfield is not valid JSON: %w", err)
			}
			if m.ID == "" {
				return Metadata{}, fmt.Errorf("metadata subfield carries no product id")
			}
			return m, nil
		}
		i = end
	}
	return Metadata{}, ErrNoMetadata
}
