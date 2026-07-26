// SPDX-License-Identifier: MIT

package bat_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/aoiflux/libvhdi/bat"
	"github.com/aoiflux/libvhdi/types"
)

// buildVHDBATData builds raw VHD BAT bytes for testing.
// entries: slice of uint32 in big-endian format.
func buildVHDBATData(entries []uint32) []byte {
	buf := make([]byte, len(entries)*4)
	for i, e := range entries {
		binary.BigEndian.PutUint32(buf[i*4:], e)
	}
	return buf
}

// buildVHDXBATData builds raw VHDX BAT bytes for testing.
func buildVHDXBATData(entries []uint64) []byte {
	buf := make([]byte, len(entries)*8)
	for i, e := range entries {
		binary.LittleEndian.PutUint64(buf[i*8:], e)
	}
	return buf
}

// ============================================================================
// VHD BAT tests
// ============================================================================

func TestVHDBATReadAllocated(t *testing.T) {
	// Entry 0 → sector offset 10 (file offset = 10*512 = 5120)
	// Entry 1 → unallocated (0xFFFFFFFF)
	rawEntries := []uint32{10, 0xFFFFFFFF}
	data := buildVHDBATData(rawEntries)

	p := bat.NewVHDBATParser(bytes.NewReader(data))
	b, err := p.ReadBAT(0, 2, 2*1024*1024)
	if err != nil {
		t.Fatalf("ReadBAT: %v", err)
	}

	// Block 0 should be allocated
	alloc0, _ := bat.IsAllocatedVHD(b, 0)
	if !alloc0 {
		t.Error("block 0 should be allocated")
	}

	// Block 1 should be unallocated
	alloc1, _ := bat.IsAllocatedVHD(b, 1)
	if alloc1 {
		t.Error("block 1 should not be allocated")
	}
}

func TestVHDBATBlockOffset(t *testing.T) {
	// Sector offset 10 → file offset = (sectorBitmapSize + 10*512) but in the
	// parser the offset includes the bitmap. For this test we check the sign.
	rawEntries := []uint32{10, 0xFFFFFFFF}
	data := buildVHDBATData(rawEntries)

	p := bat.NewVHDBATParser(bytes.NewReader(data))
	b, err := p.ReadBAT(0, 2, 2*1024*1024)
	if err != nil {
		t.Fatalf("ReadBAT: %v", err)
	}

	off0, err := bat.BlockOffsetVHD(b, 0)
	if err != nil {
		t.Fatalf("BlockOffsetVHD: %v", err)
	}
	if off0 < 0 {
		t.Errorf("BlockOffsetVHD for allocated block = %d, should be >= 0", off0)
	}

	// Unallocated block should return -1
	off1, err := bat.BlockOffsetVHD(b, 1)
	if err != nil {
		t.Fatalf("BlockOffsetVHD(unallocated): %v", err)
	}
	if off1 != -1 {
		t.Errorf("BlockOffsetVHD for unallocated = %d, want -1", off1)
	}
}

func TestVHDBATBlockCount(t *testing.T) {
	rawEntries := []uint32{1, 2, 3}
	data := buildVHDBATData(rawEntries)

	p := bat.NewVHDBATParser(bytes.NewReader(data))
	b, err := p.ReadBAT(0, 3, 2*1024*1024)
	if err != nil {
		t.Fatalf("ReadBAT: %v", err)
	}

	if cnt := bat.BlockCountVHD(b); cnt != 3 {
		t.Errorf("BlockCountVHD = %d, want 3", cnt)
	}
}

// ============================================================================
// VHDX BAT tests
// ============================================================================

func TestVHDXBATPayloadBlockFullyAllocated(t *testing.T) {
	// A VHDX BAT payload entry: bits 0-2 = state, bits 20-63 = file_offset / MB
	// State 6 (FullyAllocated), offset = 2MB → bits 20-63 = 2
	// Entry: state=6, fileOffsetMB=2 → (2 << 20) | 6
	entry := uint64(2<<20) | uint64(types.BlockStateFullyAllocated)
	data := buildVHDXBATData([]uint64{entry, 0 /* sector bitmap placeholder */})

	p := bat.NewVHDXBATParser(bytes.NewReader(data))
	b, err := p.ReadBAT(0, 1, 2*1024*1024, 512)
	if err != nil {
		t.Fatalf("ReadBAT: %v", err)
	}

	alloc, _ := bat.IsAllocatedVHDX(b, 0)
	if !alloc {
		t.Error("block 0 should be allocated (state=FullyAllocated)")
	}

	off, err := bat.BlockOffsetVHDX(b, 0)
	if err != nil {
		t.Fatalf("BlockOffsetVHDX: %v", err)
	}
	if off != 2*1024*1024 {
		t.Errorf("BlockOffsetVHDX = %d, want %d", off, 2*1024*1024)
	}
}

func TestVHDXBATPayloadBlockNotPresent(t *testing.T) {
	// State 0 = NotPresent/unallocated
	entry := uint64(types.BlockStateNone)
	data := buildVHDXBATData([]uint64{entry, 0})

	p := bat.NewVHDXBATParser(bytes.NewReader(data))
	b, err := p.ReadBAT(0, 1, 2*1024*1024, 512)
	if err != nil {
		t.Fatalf("ReadBAT: %v", err)
	}

	alloc, _ := bat.IsAllocatedVHDX(b, 0)
	if alloc {
		t.Error("block 0 should NOT be allocated (state=NotPresent)")
	}

	off, _ := bat.BlockOffsetVHDX(b, 0)
	if off != -1 {
		t.Errorf("BlockOffsetVHDX for unallocated = %d, want -1", off)
	}
}

// ============================================================================
// Sector bitmap tests
// ============================================================================

func TestSectorBitmapAllAllocated(t *testing.T) {
	// A 512-byte bitmap with all bits set → all sectors allocated.
	data := bytes.Repeat([]byte{0xFF}, 512)
	sb, err := bat.ReadSectorBitmap(bytes.NewReader(data), 0, 512)
	if err != nil {
		t.Fatalf("ReadSectorBitmap: %v", err)
	}

	// Sector 0 and sector 4095 should both be allocated.
	if !sb.IsSectorAllocated(0) {
		t.Error("sector 0 should be allocated in all-ones bitmap")
	}
	if !sb.IsSectorAllocated(4095) {
		t.Error("sector 4095 should be allocated in all-ones bitmap")
	}
}

func TestSectorBitmapNoneAllocated(t *testing.T) {
	data := make([]byte, 512)
	sb, err := bat.ReadSectorBitmap(bytes.NewReader(data), 0, 512)
	if err != nil {
		t.Fatalf("ReadSectorBitmap: %v", err)
	}

	if sb.IsSectorAllocated(0) {
		t.Error("sector 0 should NOT be allocated in all-zeros bitmap")
	}

	ranges := sb.GetAllocatedSectorRanges()
	if len(ranges) != 0 {
		t.Errorf("GetAllocatedSectorRanges on empty bitmap = %v, want []", ranges)
	}
}

func TestSectorBitmapPartial(t *testing.T) {
	// Set only the first byte (sectors 0-7) to 0xFF.
	data := make([]byte, 512)
	data[0] = 0xFF
	sb, err := bat.ReadSectorBitmap(bytes.NewReader(data), 0, 512)
	if err != nil {
		t.Fatalf("ReadSectorBitmap: %v", err)
	}

	for i := uint32(0); i < 8; i++ {
		if !sb.IsSectorAllocated(i) {
			t.Errorf("sector %d should be allocated", i)
		}
	}
	if sb.IsSectorAllocated(8) {
		t.Error("sector 8 should NOT be allocated")
	}

	ranges := sb.GetAllocatedSectorRanges()
	if len(ranges) != 1 {
		t.Errorf("expected 1 range, got %d", len(ranges))
	}
}
