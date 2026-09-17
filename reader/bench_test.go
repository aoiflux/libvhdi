// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// There were no benchmarks, so nothing guarded the BAT walk or the extent map
// against a regression that made them quadratic. These are cheap and pin the
// shape of the cost rather than an absolute number.
//
// The two that matter are AllExtents, which walks every block of every disk in
// a chain, and the whole-device read paths, which is what an acquisition does.

// benchVHDX returns a VHDX with one present block in a larger address space.
func benchVHDX(b *testing.B) []byte {
	b.Helper()
	return buildVHDX(vhdxParams{
		virtualDiskSize: 64 * 1024 * 1024,
		blockSize:       1024 * 1024,
		sectorSize:      512,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xB7, 1024*1024),
	})
}

func BenchmarkAllExtents(b *testing.B) {
	img := benchVHDX(b)
	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		b.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		extents, err := d.AllExtents()
		if err != nil {
			b.Fatalf("AllExtents: %v", err)
		}
		if len(extents) == 0 {
			b.Fatal("no extents")
		}
	}
}

func BenchmarkAllExtentsContext(b *testing.B) {
	// The chunked walk exists so cancellation is checked during the walk. It
	// must not cost meaningfully more than the plain one, or callers will avoid
	// the cancellable form precisely when they most need it.
	img := benchVHDX(b)
	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		b.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.AllExtentsContext(ctx); err != nil {
			b.Fatalf("AllExtentsContext: %v", err)
		}
	}
}

func BenchmarkAllExtentsDifferencingChain(b *testing.B) {
	// A chain multiplies the walk: every unbacked range in the child recurses
	// into the parent. This is where a quadratic regression would show first.
	const size = 16 * 1024 * 1024
	const block = 1024 * 1024

	parentID := [16]byte{0xB1, 0x01}
	parentImg := buildVHD(vhdImageParams{
		diskType:  types.DiskTypeDynamic,
		mediaSize: size,
		blockSize: block,
		blocks:    benchBlocks(16, true),
		selfID:    parentID,
	})
	childImg := buildVHD(vhdImageParams{
		diskType:   types.DiskTypeDifferential,
		mediaSize:  size,
		blockSize:  block,
		blocks:     benchBlocks(16, false),
		parentName: "parent.vhd",
		parentID:   parentID,
		selfID:     [16]byte{0xB2, 0x02},
	})

	parent, err := OpenVHD(bytes.NewReader(parentImg), int64(len(parentImg)))
	if err != nil {
		b.Fatalf("opening parent: %v", err)
	}
	defer parent.Close()

	child, err := OpenVHD(bytes.NewReader(childImg), int64(len(childImg)))
	if err != nil {
		b.Fatalf("opening child: %v", err)
	}
	defer child.Close()

	if err := child.SetParent(parent); err != nil {
		b.Fatalf("SetParent: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := child.AllExtents(); err != nil {
			b.Fatalf("AllExtents: %v", err)
		}
	}
}

// benchBlocks builds n blocks, either all present or all absent.
func benchBlocks(n int, present bool) []vhdBlock {
	out := make([]vhdBlock, n)
	for i := range out {
		if present {
			out[i] = vhdBlock{
				allocated:      true,
				presentSectors: allSectors(1024 * 1024 / 512),
				data:           repeatByte(byte(0xC0+i), 1024*1024),
			}
			continue
		}
		out[i] = vhdBlock{allocated: false}
	}
	return out
}

func BenchmarkSequentialRead(b *testing.B) {
	// Reading the device densely, which is what a hasher or an archiver does.
	img := benchVHDX(b)
	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		b.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	b.SetBytes(int64(d.Size()))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := io.Copy(io.Discard, d.SectionReader())
		if err != nil {
			b.Fatalf("Copy: %v", err)
		}
		if n != int64(d.Size()) {
			b.Fatalf("copied %d bytes, want %d", n, d.Size())
		}
	}
}

func BenchmarkSparseStream(b *testing.B) {
	// The same device read sparsely. This should move a fraction of the bytes
	// BenchmarkSequentialRead does, and the comparison between the two is the
	// point of the streaming API.
	img := benchVHDX(b)
	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		b.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var streamed int64
		err := d.Stream(ctx, nil, func(r Run) error {
			streamed += int64(len(r.Data))
			return nil
		})
		if err != nil {
			b.Fatalf("Stream: %v", err)
		}
		if streamed == 0 {
			b.Fatal("streamed nothing")
		}
	}
}

func BenchmarkRandomSectorReads(b *testing.B) {
	// The access pattern a filesystem parser produces: many small reads at
	// scattered offsets, each one a fresh BAT lookup.
	img := benchVHDX(b)
	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		b.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	buf := make([]byte, 512)
	// Offsets within the one present block, so every read is served rather than
	// short-circuited as a hole.
	const span = 1024 * 1024

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := int64((i * 4096) % (span - 512))
		if _, err := d.ReadAt(buf, off); err != nil {
			b.Fatalf("ReadAt(%d): %v", off, err)
		}
	}
}

func BenchmarkOpenVHDX(b *testing.B) {
	// Opening parses the headers, both region tables, the metadata and the
	// whole BAT. A directory scan that opened every image would pay this per
	// file, which is why discovery probes instead.
	img := benchVHDX(b)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			b.Fatalf("OpenVHDX: %v", err)
		}
		d.Close()
	}
}

func BenchmarkProbeVHDX(b *testing.B) {
	// The same image, headers only.
	//
	// On this fixture the gap between probing and opening is small -- a few
	// percent -- and that is expected rather than disappointing. A probe reads
	// two 4 KB headers, up to two 64 KB region tables and the metadata region,
	// and that cost is fixed. What it skips is the block allocation table,
	// whose size scales with the virtual disk: 64 entries here, but a million
	// for a 1 TB disk with 1 MB blocks. The saving is in the term that grows.
	//
	// This benchmark therefore tracks the fixed cost. That probing skips the
	// BAT at all is proven by TestProbeDoesNotNeedTheBlockAllocationTable,
	// which truncates the table away entirely and confirms a full open fails
	// while the probe still describes the image.
	img := benchVHDX(b)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Probe(bytes.NewReader(img), int64(len(img))); err != nil {
			b.Fatalf("Probe: %v", err)
		}
	}
}
