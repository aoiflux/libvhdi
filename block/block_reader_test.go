// SPDX-License-Identifier: MIT

package block_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/aoiflux/libvhdi/block"
	"github.com/aoiflux/libvhdi/types"
)

// A read that starts partway into a sector of a PARTIALLY_PRESENT block must
// return the bytes at the requested offset (PAR-31). The per-sector loop placed
// the first sector's bytes at the clamped start of the destination but still
// read them from the start of the sector, so an unaligned read came back shifted
// by its offset within the sector. Sector-aligned reads were unaffected, which
// is why a test reading whole sectors could not see it.

// partialBlockSectors is how many sectors of the block the fixture populates.
const partialBlockSectors = 8

// partialBlockPresent reports whether a sector of the fixture block is present.
// Alternating sectors make every read longer than a sector cross a boundary
// between present and absent.
func partialBlockPresent(sector int) bool {
	return sector%2 == 0 && sector < partialBlockSectors
}

// partialBlockImage lays out one state-7 block: its chunk's sector bitmap at
// file offset 0, then the block data at 4096. Present sectors hold a pattern
// that differs at every byte offset; absent sectors hold junk the reader must
// never return. It also returns the bytes ReadAt must produce for the populated
// sectors: the pattern where present, zero where absent.
func partialBlockImage(sectorSize int) (img []byte, b *types.VHDXBlockAllocationTable, want []byte) {
	const (
		blockSize = 1 << 20
		dataOff   = 4096
	)

	img = make([]byte, dataOff+partialBlockSectors*sectorSize)
	want = make([]byte, partialBlockSectors*sectorSize)
	for s := range partialBlockSectors {
		if partialBlockPresent(s) {
			img[s/8] |= 1 << uint(s%8) // VHDX sector bitmaps are LSB-first
		}
	}
	for i := range want {
		if partialBlockPresent(i / sectorSize) {
			img[dataOff+i] = byte(i%251 + 1)
			want[i] = img[dataOff+i]
		} else {
			img[dataOff+i] = 0xEE
		}
	}

	b = &types.VHDXBlockAllocationTable{
		NumberOfEntries:     1,
		BlockSize:           blockSize,
		ChunkRatio:          uint32((uint64(1) << 23) * uint64(sectorSize) / blockSize),
		SectorSize:          uint32(sectorSize),
		SectorBitmapOffsets: []int64{0},
		Entries: []types.BlockAllocationEntry{{
			IsAllocated: true,
			FileOffset:  dataOff,
			BlockState:  types.BlockStatePartiallyAllocated,
		}},
	}
	return img, b, want
}

func TestVHDXPartialBlockUnalignedReads(t *testing.T) {
	for _, sectorSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("sector%d", sectorSize), func(t *testing.T) {
			img, b, want := partialBlockImage(sectorSize)
			br := block.NewVHDXBlockReader(bytes.NewReader(img), b, uint32(sectorSize))

			// Every start in the first two sectors, so starts in a present and
			// in an absent sector are both covered, at lengths that stay inside
			// a sector, end exactly on a boundary, and span several.
			for off := 0; off < 2*sectorSize; off++ {
				for _, n := range []int{1, sectorSize - 1, sectorSize, 2*sectorSize + 3} {
					got := make([]byte, n)
					if _, err := br.ReadAt(got, int64(off)); err != nil {
						t.Fatalf("ReadAt(len %d, off %d): %v", n, off, err)
					}
					if k := firstDifference(got, want[off:off+n]); k >= 0 {
						t.Fatalf("ReadAt(len %d, off %d): byte %d = %#02x, want %#02x",
							n, off, k, got[k], want[off+k])
					}
				}
			}
		})
	}
}

// firstDifference returns the index of the first byte where a and b differ, or
// -1 if they are equal.
func firstDifference(a, b []byte) int {
	if bytes.Equal(a, b) {
		return -1
	}
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}
	return len(a)
}
