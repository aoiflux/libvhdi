// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// Unaligned reads in a PARTIALLY_PRESENT block of a differencing VHDX (PAR-31).
//
// A read whose start fell inside a sector of a state-7 block returned the bytes
// from the start of that sector instead of those at the requested offset, with
// no error. Sector-aligned reads were correct, so the damage showed only where
// something is read at an odd offset: on a real chain, an ext4 root inode 256
// bytes into a sector read as inode 1. These tests read at every offset within
// the first two sectors and compare each result with a byte model of the chain.

// unalignedGeometry is a block and logical sector size to run the tests under.
// Both reuse an existing differencing fixture geometry.
type unalignedGeometry struct {
	name       string
	blockSize  int
	sectorSize int
	virtual    uint64
}

var unalignedGeometries = []unalignedGeometry{
	{"512", vhdxDiffBlockSize, vhdxDiffSectorSize, vhdxDiffVirtual},
	{"4K", vhdx4kBlockSize, vhdx4kSectorSize, vhdx4kVirtual},
}

// unalignedSectors is how many sectors of block 0 the tests populate and model.
const unalignedSectors = 8

// unalignedPresent reports whether sector s of block 0 is present in the child.
// Alternating sectors make every read longer than a sector cross a boundary
// between the child and the parent.
func unalignedPresent(s int) bool { return s%2 == 0 && s < unalignedSectors }

// unalignedChain writes a dynamic parent and a differencing child into one
// directory and returns their paths, with the bytes the chain must produce for
// the first unalignedSectors sectors.
//
// The child's block 0 is PARTIALLY_PRESENT. Its present sectors hold a pattern
// that differs at every byte offset, and its absent sectors hold junk, so a
// read that takes absent bytes from the child shows up as well as one that
// takes the wrong offset. The parent holds a second, different pattern.
func unalignedChain(t *testing.T, g unalignedGeometry) (childPath, parentPath string, want []byte) {
	t.Helper()

	parentPayload := make([]byte, g.blockSize)
	for i := range parentPayload {
		parentPayload[i] = byte(i%253) ^ 0x5A
	}

	childPayload := bytes.Repeat([]byte{0xEE}, g.blockSize)
	var present []int
	for s := range unalignedSectors {
		if unalignedPresent(s) {
			present = append(present, s)
		}
	}

	want = make([]byte, unalignedSectors*g.sectorSize)
	for i := range want {
		if unalignedPresent(i / g.sectorSize) {
			childPayload[i] = byte(i%251 + 1)
			want[i] = childPayload[i]
		} else {
			want[i] = parentPayload[i]
		}
	}

	parent := buildVHDX(vhdxParams{
		blockSize:       uint32(g.blockSize),
		sectorSize:      uint32(g.sectorSize),
		virtualDiskSize: g.virtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         parentPayload,
		dataWriteGUID:   guidRightParent,
		virtualDiskID:   idBase,
	})
	child := buildVHDX(vhdxParams{
		blockSize:       uint32(g.blockSize),
		sectorSize:      uint32(g.sectorSize),
		virtualDiskSize: g.virtual,
		hasParent:       true,
		blockState:      types.BlockStatePartiallyAllocated,
		presentSectors:  present,
		payload:         childPayload,
		parentLinkage:   linkageValue(guidRightParent),
		virtualDiskID:   idTop,
	})

	dir := writeChainDir(t, map[string][]byte{
		"parent.vhdx": parent,
		"child.vhdx":  child,
	})
	return filepath.Join(dir, "child.vhdx"), filepath.Join(dir, "parent.vhdx"), want
}

// openUnalignedChain opens the child of unalignedChain the way a forensic
// consumer does, requiring the whole chain to resolve.
func openUnalignedChain(t *testing.T, g unalignedGeometry) (d *VirtualDisk, childPath, parentPath string, want []byte) {
	t.Helper()

	childPath, parentPath, want = unalignedChain(t, g)
	d, err := OpenFileWith(childPath, &Options{RequireParentChain: true})
	if err != nil {
		t.Fatalf("OpenFileWith: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d, childPath, parentPath, want
}

func TestDifferencingVHDXUnalignedReadsInPartialBlock(t *testing.T) {
	for _, g := range unalignedGeometries {
		t.Run(g.name, func(t *testing.T) {
			d, _, _, want := openUnalignedChain(t, g)
			ss := g.sectorSize

			for off := 0; off < 2*ss; off++ {
				for _, n := range []int{1, ss - 1, ss, 2*ss + 3} {
					got := make([]byte, n)
					if _, err := d.ReadAt(got, int64(off)); err != nil {
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

// TestDifferencingVHDXUnalignedReadReport is the reproduction from the report:
// sixteen bytes read 100 bytes into a present sector.
func TestDifferencingVHDXUnalignedReadReport(t *testing.T) {
	d, _, _, want := openUnalignedChain(t, unalignedGeometries[0])

	got := make([]byte, 16)
	if _, err := d.ReadAt(got, 100); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, want[100:116]) {
		t.Fatalf("ReadAt(16, 100) = % x, want % x", got, want[100:116])
	}
}

// TestVHDXPartialBlockExtentsMatchReadAt holds Extents and ReadAt to the same
// answer for a range that starts mid-sector: the bytes at each mapped extent's
// file offset must be the bytes ReadAt returns for its virtual range.
func TestVHDXPartialBlockExtentsMatchReadAt(t *testing.T) {
	for _, g := range unalignedGeometries {
		t.Run(g.name, func(t *testing.T) {
			d, childPath, parentPath, _ := openUnalignedChain(t, g)

			files := map[int][]byte{}
			for i, p := range []string{childPath, parentPath} {
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("ReadFile: %v", err)
				}
				files[i] = raw
			}

			at, length := int64(100), int64(3*g.sectorSize+50)
			extents, err := d.Extents(at, length)
			if err != nil {
				t.Fatalf("Extents: %v", err)
			}

			next := at
			for _, e := range extents {
				if e.VirtualOffset != next {
					t.Fatalf("extent starts at %d, want %d", e.VirtualOffset, next)
				}
				next = e.End()
				if e.Kind != ExtentMapped {
					t.Fatalf("extent %+v is not mapped", e)
				}
				raw, ok := files[e.ChainIndex]
				if !ok {
					t.Fatalf("extent %+v names an unexpected chain index", e)
				}

				got := make([]byte, e.Length)
				if _, err := d.ReadAt(got, e.VirtualOffset); err != nil {
					t.Fatalf("ReadAt: %v", err)
				}
				fromFile := raw[e.FileOffset : e.FileOffset+e.Length]
				if k := firstDifference(got, fromFile); k >= 0 {
					t.Fatalf("extent %+v: ReadAt byte %d = %#02x, file byte = %#02x",
						e, k, got[k], fromFile[k])
				}
			}
			if next != at+length {
				t.Fatalf("extents end at %d, want %d", next, at+length)
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
