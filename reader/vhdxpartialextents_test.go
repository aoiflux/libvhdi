// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// Extent mapping of PARTIALLY_PRESENT (state 7) blocks.
//
// A state-7 block is allocated but only partly written: the chunk's sector
// bitmap says which sectors actually carry data, and the rest read as zeroes.
// Extent mapping used to report the whole allocated block as ExtentMapped, so a
// sparse-aware imaging tool driven by Extents would seek to the reported file
// offset and copy whatever happened to be sitting there.
//
// The fixture below is what makes that visible: the block is filled with 0xEE on
// disk while only sectors 0, 2 and 4 are marked present. A test whose unwritten
// sectors were zero on disk would pass either way, because the wrong answer and
// the right answer produce identical bytes.
func TestVHDXPartialBlockExtentsExcludeUnwrittenSectors(t *testing.T) {
	const (
		blockSize  = vhdxDiffBlockSize
		sectorSize = vhdxDiffSectorSize
	)
	present := []int{0, 2, 4}

	payload := repeatByte(0xEE, blockSize)

	img := buildVHDX(vhdxParams{
		blockSize:       blockSize,
		sectorSize:      sectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStatePartiallyAllocated,
		presentSectors:  present,
		payload:         payload,
	})

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	// Guard: reads must already be correct. If this fails, the extent
	// expectations below are meaningless.
	buf := make([]byte, sectorSize)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt(sector 0): %v", err)
	}
	if !bytes.Equal(buf, repeatByte(0xEE, sectorSize)) {
		t.Fatalf("present sector 0 did not read its data")
	}
	if _, err := d.ReadAt(buf, sectorSize); err != nil {
		t.Fatalf("ReadAt(sector 1): %v", err)
	}
	if !bytes.Equal(buf, make([]byte, sectorSize)) {
		t.Fatalf("absent sector 1 read %#x..., want zeroes", buf[:8])
	}

	extents, err := d.Extents(0, 6*sectorSize)
	if err != nil {
		t.Fatalf("Extents: %v", err)
	}
	checkExtentsTile(t, extents, 0, 6*sectorSize)
	checkExtentsMatchReads(t, d, extents, map[string][]byte{"": img})

	// Every sector must be classified by its bitmap bit, not by the block's
	// allocation state.
	kindAt := func(off int64) ExtentKind {
		t.Helper()
		for _, e := range extents {
			if off >= e.VirtualOffset && off < e.End() {
				return e.Kind
			}
		}
		t.Fatalf("no extent covers offset %d", off)
		return ExtentZero
	}

	for s := 0; s < 6; s++ {
		off := int64(s) * sectorSize
		got := kindAt(off)
		want := ExtentZero
		for _, p := range present {
			if p == s {
				want = ExtentMapped
			}
		}
		if got != want {
			t.Errorf("sector %d: extent kind %v, want %v", s, got, want)
		}
	}
}

// TestVHDXPartialBlockMappedBytesCountsOnlyWrittenSectors pins the accounting
// that a sparse-aware acquisition uses to size its work.
func TestVHDXPartialBlockMappedBytesCountsOnlyWrittenSectors(t *testing.T) {
	const (
		blockSize  = vhdxDiffBlockSize
		sectorSize = vhdxDiffSectorSize
	)
	present := []int{0, 2, 4}

	img := buildVHDX(vhdxParams{
		blockSize:       blockSize,
		sectorSize:      sectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStatePartiallyAllocated,
		presentSectors:  present,
		payload:         repeatByte(0xEE, blockSize),
	})

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	got, err := d.MappedBytes()
	if err != nil {
		t.Fatalf("MappedBytes: %v", err)
	}
	want := int64(len(present)) * sectorSize
	if got != want {
		t.Errorf("MappedBytes() = %d, want %d (only the %d written sectors)",
			got, want, len(present))
	}
}
