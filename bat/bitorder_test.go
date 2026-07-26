package bat_test

import (
	"bytes"
	"testing"

	"github.com/aoiflux/libvhdi/bat"
)

// TestSectorBitmapBitOrderIsMSBFirst pins the bit ordering of the VHD block
// bitmap. Unlike the all-ones and whole-byte fixtures, this pattern
// distinguishes the two conventions: 0x80 is sector 0 under MSB-first ordering
// and sector 7 under LSB-first ordering.
//
// Getting this backwards silently swaps which sectors of a differencing disk
// resolve to the parent, so it is worth an explicit test.
func TestSectorBitmapBitOrderIsMSBFirst(t *testing.T) {
	data := make([]byte, 512)
	data[0] = 0x80

	sb, err := bat.ReadSectorBitmap(bytes.NewReader(data), 0, 512)
	if err != nil {
		t.Fatalf("ReadSectorBitmap: %v", err)
	}

	if !sb.IsSectorAllocated(0) {
		t.Error("sector 0 should be allocated for bitmap byte 0x80 (MSB-first)")
	}
	for s := uint32(1); s < 8; s++ {
		if sb.IsSectorAllocated(s) {
			t.Errorf("sector %d should NOT be allocated for bitmap byte 0x80", s)
		}
	}
}

// TestSectorBitmapSecondByteOrdering confirms the ordering holds beyond the
// first byte, i.e. that byte indexing and bit indexing are not conflated.
func TestSectorBitmapSecondByteOrdering(t *testing.T) {
	data := make([]byte, 512)
	data[1] = 0x01 // least significant bit of byte 1 == sector 15

	sb, err := bat.ReadSectorBitmap(bytes.NewReader(data), 0, 512)
	if err != nil {
		t.Fatalf("ReadSectorBitmap: %v", err)
	}

	if !sb.IsSectorAllocated(15) {
		t.Error("sector 15 should be allocated for bitmap byte 1 == 0x01 (MSB-first)")
	}
	if sb.IsSectorAllocated(8) {
		t.Error("sector 8 should NOT be allocated for bitmap byte 1 == 0x01")
	}
}
