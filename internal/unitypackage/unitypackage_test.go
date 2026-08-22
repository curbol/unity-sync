package unitypackage_test

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/unitypackage"
)

// realHeader is the first kilobyte of an actual downloaded package, committed as a
// fixture. Everything else here is synthetic, so this is the only case proving the
// parser matches what the store really emits.
func realHeader(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "store", "package_header_sample.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func TestReadsTheDescriptorFromARealPackageHeader(t *testing.T) {
	m, err := unitypackage.Read(bytes.NewReader(realHeader(t)))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.ID != "323439" {
		t.Errorf("ID = %q, want 323439", m.ID)
	}
	if m.VersionID != "1361204" {
		t.Errorf("VersionID = %q, want 1361204", m.VersionID)
	}
	if m.Version != "1.9" {
		t.Errorf("Version = %q, want 1.9", m.Version)
	}
	if m.UnityVersion != "6000.0.32f1" {
		t.Errorf("UnityVersion = %q, want 6000.0.32f1", m.UnityVersion)
	}
}

// gzipWithExtra builds a valid gzip stream whose extra field holds the given subfields.
func gzipWithExtra(t *testing.T, subfields ...[]byte) []byte {
	t.Helper()
	var extra []byte
	for _, s := range subfields {
		extra = append(extra, s...)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Header.Extra = extra
	if _, err := zw.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func subfield(id string, payload []byte) []byte {
	out := []byte{id[0], id[1], 0, 0}
	binary.LittleEndian.PutUint16(out[2:4], uint16(len(payload)))
	return append(out, payload...)
}

// XLEN is a uint16, so a descriptor can legitimately sit tens of kilobytes in. A reader
// that only inspects a fixed prefix passes every other case here and then silently
// reports "no metadata" in production, turning the hard wrong-asset check into a
// warning.
func TestFindsADescriptorFarPastAnyObservedOffset(t *testing.T) {
	filler := subfield("XX", bytes.Repeat([]byte{0}, 40000))
	real := subfield("A$", []byte(`{"id":"999","version_id":"42"}`))
	m, err := unitypackage.Read(bytes.NewReader(gzipWithExtra(t, filler, real)))
	if err != nil {
		t.Fatalf("Read with a 40 KB leading subfield: %v", err)
	}
	if m.ID != "999" || m.VersionID != "42" {
		t.Errorf("got %+v, want id 999 / version_id 42", m)
	}
}

func TestForeignSubfieldsAloneMeanNoMetadata(t *testing.T) {
	body := gzipWithExtra(t, subfield("QQ", []byte("something else")))
	_, err := unitypackage.Read(bytes.NewReader(body))
	if !errors.Is(err, unitypackage.ErrNoMetadata) {
		t.Errorf("Read = %v, want ErrNoMetadata", err)
	}
}

func TestNoExtraFieldMeansNoMetadata(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("payload"))
	zw.Close()
	_, err := unitypackage.Read(bytes.NewReader(buf.Bytes()))
	if !errors.Is(err, unitypackage.ErrNoMetadata) {
		t.Errorf("Read = %v, want ErrNoMetadata", err)
	}
}

func TestTruncatedHeaderIsAnError(t *testing.T) {
	full := realHeader(t)
	if _, err := unitypackage.Read(bytes.NewReader(full[:20])); err == nil {
		t.Error("Read accepted a truncated gzip header")
	}
}

func TestNonGzipIsAnError(t *testing.T) {
	if _, err := unitypackage.Read(strings.NewReader("<html>sign in</html>")); err == nil {
		t.Error("Read accepted a body that is not gzip at all")
	}
}

func TestDescriptorWithoutAProductIdIsRejected(t *testing.T) {
	body := gzipWithExtra(t, subfield("A$", []byte(`{"version_id":"42"}`)))
	if _, err := unitypackage.Read(bytes.NewReader(body)); err == nil {
		t.Error("Read accepted a descriptor with no product id; the id gate depends on it")
	}
}
