// SPDX-License-Identifier: MIT

package binaryutil_test

import (
	"bytes"
	"testing"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
)

// ============================================================================
// CRC-32 tests
// ============================================================================

func TestCRC32KnownValue(t *testing.T) {
	// CRC-32C of empty slice — the CRC-32C of zero bytes with IV=0xFFFFFFFF and
	// final XOR 0xFFFFFFFF is 0x00000000.
	got := binaryutil.CRC32([]byte{})
	if got != 0x00000000 {
		t.Errorf("CRC32([]) = 0x%08X, want 0x00000000", got)
	}
}

func TestCRC32NonEmpty(t *testing.T) {
	// Verify CRC32 is deterministic.
	data := []byte("hello world")
	a := binaryutil.CRC32(data)
	b := binaryutil.CRC32(data)
	if a != b {
		t.Errorf("CRC32 is not deterministic: %08X != %08X", a, b)
	}
}

func TestVerifyCRC32(t *testing.T) {
	data := []byte("test data for checksum")
	checksum := binaryutil.CRC32(data)
	if !binaryutil.VerifyCRC32(data, checksum) {
		t.Error("VerifyCRC32 failed for valid checksum")
	}
	if binaryutil.VerifyCRC32(data, checksum^0x1) {
		t.Error("VerifyCRC32 should fail for corrupted checksum")
	}
}

// ============================================================================
// GUID round-trip tests
// ============================================================================

func TestGUIDToString(t *testing.T) {
	// A GUID with known byte layout.
	guid := [16]byte{
		0x6b, 0x29, 0xfe, 0x4d, 0x2f, 0xfb, 0x4e, 0x25,
		0xb8, 0x35, 0x40, 0x05, 0x44, 0x00, 0x00, 0x00,
	}
	s := binaryutil.GUIDToString(guid)
	if len(s) != 36 {
		t.Errorf("GUIDToString produced string of length %d, want 36: %q", len(s), s)
	}
	// Must contain dashes at positions 8, 13, 18, 23.
	for _, pos := range []int{8, 13, 18, 23} {
		if s[pos] != '-' {
			t.Errorf("GUIDToString: expected '-' at position %d, got %q in %q", pos, s[pos], s)
		}
	}
}

func TestGUIDFromString(t *testing.T) {
	src := [16]byte{
		0x6b, 0x29, 0xfe, 0x4d, 0x2f, 0xfb, 0x4e, 0x25,
		0xb8, 0x35, 0x40, 0x05, 0x44, 0x00, 0x00, 0x00,
	}
	s := binaryutil.GUIDToString(src)
	got, err := binaryutil.GUIDFromString(s)
	if err != nil {
		t.Fatalf("GUIDFromString(%q) error: %v", s, err)
	}
	if got != src {
		t.Errorf("GUID round-trip mismatch:\n got %v\nwant %v", got, src)
	}
}

func TestGUIDEqual(t *testing.T) {
	a := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	b := a
	if !binaryutil.GUIDEqual(a, b) {
		t.Error("GUIDEqual: identical GUIDs reported not equal")
	}
	b[0] = 0
	if binaryutil.GUIDEqual(a, b) {
		t.Error("GUIDEqual: different GUIDs reported equal")
	}
}

// ============================================================================
// ReadAtReader tests
// ============================================================================

func makeReader(data []byte) *binaryutil.ReadAtReader {
	return binaryutil.NewReadAtReaderAtStart(bytes.NewReader(data))
}

func TestReadUint8(t *testing.T) {
	r := makeReader([]byte{0xAB, 0xCD})
	v, err := r.ReadUint8()
	if err != nil || v != 0xAB {
		t.Errorf("ReadUint8 = (%02X, %v), want (AB, nil)", v, err)
	}
}

func TestReadUint16BigEndian(t *testing.T) {
	r := makeReader([]byte{0x12, 0x34})
	v, err := r.ReadUint16BigEndian()
	if err != nil || v != 0x1234 {
		t.Errorf("ReadUint16BigEndian = (%04X, %v), want (1234, nil)", v, err)
	}
}

func TestReadUint16LittleEndian(t *testing.T) {
	r := makeReader([]byte{0x34, 0x12})
	v, err := r.ReadUint16LittleEndian()
	if err != nil || v != 0x1234 {
		t.Errorf("ReadUint16LittleEndian = (%04X, %v), want (1234, nil)", v, err)
	}
}

func TestReadUint32BigEndian(t *testing.T) {
	r := makeReader([]byte{0x12, 0x34, 0x56, 0x78})
	v, err := r.ReadUint32BigEndian()
	if err != nil || v != 0x12345678 {
		t.Errorf("ReadUint32BigEndian = (%08X, %v), want (12345678, nil)", v, err)
	}
}

func TestReadUint32LittleEndian(t *testing.T) {
	r := makeReader([]byte{0x78, 0x56, 0x34, 0x12})
	v, err := r.ReadUint32LittleEndian()
	if err != nil || v != 0x12345678 {
		t.Errorf("ReadUint32LittleEndian = (%08X, %v), want (12345678, nil)", v, err)
	}
}

func TestReadUint64BigEndian(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	r := makeReader(data)
	v, err := r.ReadUint64BigEndian()
	if err != nil || v != 0x0102030405060708 {
		t.Errorf("ReadUint64BigEndian = (%016X, %v)", v, err)
	}
}

func TestReadString(t *testing.T) {
	data := append([]byte("hello"), make([]byte, 10)...)
	r := makeReader(data)
	s, err := r.ReadString(10)
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if s != "hello" {
		t.Errorf("ReadString = %q, want %q", s, "hello")
	}
}

func TestSeekToAndOffset(t *testing.T) {
	r := makeReader([]byte{0x01, 0x02, 0x03, 0x04, 0x05})
	r.SeekTo(3)
	if r.Offset() != 3 {
		t.Errorf("Offset after SeekTo(3) = %d, want 3", r.Offset())
	}
	v, _ := r.ReadUint8()
	if v != 0x04 {
		t.Errorf("byte at offset 3 = %02X, want 04", v)
	}
}

func TestSkipBytes(t *testing.T) {
	r := makeReader([]byte{0xAA, 0xBB, 0xCC, 0xDD})
	if err := r.SkipBytes(2); err != nil {
		t.Fatalf("SkipBytes: %v", err)
	}
	v, _ := r.ReadUint8()
	if v != 0xCC {
		t.Errorf("byte after skip 2 = %02X, want CC", v)
	}
}

func TestPeekBytesDoesNotAdvance(t *testing.T) {
	r := makeReader([]byte{0x01, 0x02, 0x03})
	peeked, err := r.PeekBytes(2)
	if err != nil {
		t.Fatalf("PeekBytes: %v", err)
	}
	if len(peeked) != 2 || peeked[0] != 0x01 {
		t.Errorf("PeekBytes = %v, want [01 02]", peeked)
	}
	if r.Offset() != 0 {
		t.Errorf("Offset after PeekBytes = %d, want 0 (should not advance)", r.Offset())
	}
}
