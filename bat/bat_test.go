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

// TestVHDXBATEntryCount pins the two counts of MS-VHDX 2.5. A differencing
// BAT holds whole chunks, so it reaches the last chunk's sector bitmap entry;
// a fixed or dynamic BAT ends at the last payload entry.
func TestVHDXBATEntryCount(t *testing.T) {
	for _, c := range []struct {
		name             string
		blocks, ratio    uint64
		differencing, fd uint64
	}{
		// Hyper-V's default 127 GiB disk: 2 MiB blocks, 512-byte sectors.
		{"127 GiB", 65024, 2048, 32 * 2049, 65024 + 31},
		{"10 GiB", 5120, 2048, 3 * 2049, 5120 + 2},
		{"exact chunk multiple", 4096, 2048, 2 * 2049, 4096 + 1},
		{"one block", 1, 16, 17, 1},
		{"one block into the second chunk", 17, 16, 34, 18},
		{"no blocks", 0, 16, 0, 0},
		{"zero chunk ratio", 17, 0, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := bat.VHDXBATEntryCount(c.blocks, c.ratio, types.DiskTypeDifferential); got != c.differencing {
				t.Errorf("differencing = %d, want %d", got, c.differencing)
			}
			for _, dt := range []types.DiskType{types.DiskTypeDynamic, types.DiskTypeFixed} {
				if got := bat.VHDXBATEntryCount(c.blocks, c.ratio, dt); got != c.fd {
					t.Errorf("disk type %d = %d, want %d", dt, got, c.fd)
				}
			}
		})
	}
}

// TestVHDXBATForDiskTypeLastPartialChunk reads a differencing BAT that ends one
// block into its second chunk. 256 MiB blocks and 512-byte sectors give a chunk
// ratio of 16, so 17 blocks span two chunks and the BAT holds 2 x 17 entries.
// The chunk-1 sector bitmap entry is the last of them, at index 33.
func TestVHDXBATForDiskTypeLastPartialChunk(t *testing.T) {
	const (
		blockSize = 256 << 20
		blocks    = 17
		ratio     = 16
	)

	raw := make([]uint64, 2*(ratio+1))
	raw[ratio] = 8<<20 | 6                                                 // chunk-0 sector bitmap, present
	raw[16+16/ratio] = 10<<20 | uint64(types.BlockStatePartiallyAllocated) // block 16
	raw[1*(ratio+1)+ratio] = 9<<20 | 6                                     // chunk-1 sector bitmap, present
	data := buildVHDXBATData(raw)

	p := bat.NewVHDXBATParser(bytes.NewReader(data))
	b, err := p.ReadBATForDiskType(0, blocks, blockSize, 512, types.DiskTypeDifferential)
	if err != nil {
		t.Fatalf("ReadBATForDiskType: %v", err)
	}
	if b.ChunkRatio != ratio {
		t.Fatalf("ChunkRatio = %d, want %d", b.ChunkRatio, ratio)
	}
	want := []int64{8 << 20, 9 << 20}
	if len(b.SectorBitmapOffsets) != len(want) {
		t.Fatalf("SectorBitmapOffsets = %v, want %v", b.SectorBitmapOffsets, want)
	}
	for i := range want {
		if b.SectorBitmapOffsets[i] != want[i] {
			t.Errorf("SectorBitmapOffsets[%d] = %d, want %d", i, b.SectorBitmapOffsets[i], want[i])
		}
	}
	if e := b.Entries[16]; e.BlockState != types.BlockStatePartiallyAllocated || e.FileOffset != 10<<20 {
		t.Errorf("block 16 = %+v, want PARTIALLY_PRESENT at %d", e, 10<<20)
	}

	// ReadBAT keeps its v0.4.1 count, which stops short of the chunk-1 entry.
	// This is why it is deprecated.
	legacy, err := p.ReadBAT(0, blocks, blockSize, 512)
	if err != nil {
		t.Fatalf("ReadBAT: %v", err)
	}
	if got := legacy.SectorBitmapOffsets[1]; got != -1 {
		t.Errorf("ReadBAT SectorBitmapOffsets[1] = %d, want -1 (unchanged legacy behaviour)", got)
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
