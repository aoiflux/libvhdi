// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// ============================================================================
// Concurrent reads
//
// The documentation states that ReadAt on a Disk is safe for concurrent use
// whenever the underlying io.ReaderAt is. That claim rested on inspection alone:
// the block readers and the resolver hold no mutable state after construction,
// and the log replay overlay is immutable once built. Inspection is not a test,
// and `go test -race` proves nothing unless something actually reads in parallel.
//
// These tests run many goroutines against one Disk and require every read to
// match a single-threaded reference. Under -race they also assert the absence of
// data races on the shared structures.
// ============================================================================

// readerAtFunc adapts a function to io.ReaderAt.
type readerAtFunc func(p []byte, off int64) (int, error)

func (f readerAtFunc) ReadAt(p []byte, off int64) (int, error) { return f(p, off) }

// hammerConcurrently reads the given offsets from d on many goroutines and
// requires every result to equal the reference captured single-threaded first.
func hammerConcurrently(t *testing.T, d *VirtualDisk, offsets []int64, length int) {
	t.Helper()

	// Reference pass, single-threaded.
	want := make([][]byte, len(offsets))
	for i, off := range offsets {
		buf := make([]byte, length)
		n, err := d.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("reference ReadAt(%d): %v", off, err)
		}
		want[i] = buf[:n]
	}

	workers := runtime.GOMAXPROCS(0) * 2
	if workers < 4 {
		workers = 4
	}
	const rounds = 25

	var wg sync.WaitGroup
	errCh := make(chan string, workers*len(offsets))

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			buf := make([]byte, length)

			for r := 0; r < rounds; r++ {
				// Stagger the starting offset per worker so goroutines are not
				// marching in lockstep through the same block.
				for k := range offsets {
					i := (k + worker) % len(offsets)
					n, err := d.ReadAt(buf, offsets[i])
					if err != nil && !errors.Is(err, io.EOF) {
						errCh <- "unexpected error at offset " + itoa(offsets[i]) + ": " + err.Error()
						return
					}
					if n != len(want[i]) || !bytes.Equal(buf[:n], want[i]) {
						errCh <- "concurrent read at offset " + itoa(offsets[i]) +
							" disagrees with the single-threaded reference"
						return
					}
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)

	seen := map[string]bool{}
	for msg := range errCh {
		if !seen[msg] {
			seen[msg] = true
			t.Error(msg)
		}
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestConcurrentReadsDynamicVHD covers the plain block reader path, mixing
// allocated blocks and holes.
func TestConcurrentReadsDynamicVHD(t *testing.T) {
	const (
		blockSize = 2 * 1024 * 1024
		mediaSize = uint64(blockSize * 3)
	)

	img := buildDynamicVHD(types.DiskTypeDynamic, mediaSize, blockSize, []vhdBlock{
		{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x11, blockSize)},
		{allocated: false},
		{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x33, blockSize)},
	}, "", [16]byte{})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	offsets := []int64{
		0, 512, blockSize - 512, blockSize, blockSize + 4096,
		2 * blockSize, 3*blockSize - 512,
	}
	hammerConcurrently(t, d, offsets, 4096)
}

// TestConcurrentReadsDifferencingChain is the important one: reads cross the
// resolver, which reads sector bitmaps and dispatches to either the child or the
// parent. The bitmap window has to be per-call for this to be safe.
func TestConcurrentReadsDifferencingChain(t *testing.T) {
	parentImg, childImg, _ := diffVHDPair("parent.vhd")
	dir := writeChainDir(t, map[string][]byte{
		"parent.vhd": parentImg,
		"child.vhd":  childImg,
	})

	d, err := OpenFile(filepath.Join(dir, "child.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()
	if d.NeedsParent() {
		t.Fatalf("chain incomplete: %v", d.ParentResolveError())
	}

	// Offsets spanning the alternating child/parent sectors, which forces the
	// resolver down the per-sector path on every read.
	var offsets []int64
	for s := int64(0); s < 8; s++ {
		offsets = append(offsets, s*512, s*512+128)
	}
	hammerConcurrently(t, d, offsets, 2048)
}

// TestConcurrentReadsThreeDeepChain adds a second resolver layer, so a single
// read can traverse two resolvers and a block reader.
func TestConcurrentReadsThreeDeepChain(t *testing.T) {
	const (
		blockSize = chainBlockSize
		mediaSize = chainMediaSize
	)
	sectors := blockSize / 512

	base := buildVHD(vhdImageParams{
		diskType: types.DiskTypeDynamic, mediaSize: mediaSize, blockSize: blockSize,
		blocks: []vhdBlock{{allocated: true, presentSectors: allSectors(sectors), data: repeatByte(0xA0, blockSize)}},
		selfID: idBase,
	})

	midData := make([]byte, blockSize)
	copy(midData[0:1024], repeatByte(0xB0, 1024))
	mid := buildVHD(vhdImageParams{
		diskType: types.DiskTypeDifferential, mediaSize: mediaSize, blockSize: blockSize,
		blocks:     []vhdBlock{{allocated: true, presentSectors: []int{0, 1}, data: midData}},
		parentName: "base.vhd", parentID: idBase, selfID: idMid,
	})

	topData := make([]byte, blockSize)
	copy(topData[0:512], repeatByte(0xC0, 512))
	top := buildVHD(vhdImageParams{
		diskType: types.DiskTypeDifferential, mediaSize: mediaSize, blockSize: blockSize,
		blocks:     []vhdBlock{{allocated: true, presentSectors: []int{0}, data: topData}},
		parentName: "mid.vhd", parentID: idMid, selfID: idTop,
	})

	dir := writeChainDir(t, map[string][]byte{
		"base.vhd": base, "mid.vhd": mid, "top.vhd": top,
	})

	d, err := OpenFile(filepath.Join(dir, "top.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()
	if d.NeedsParent() {
		t.Fatalf("chain incomplete: %v", d.ParentResolveError())
	}

	offsets := []int64{0, 256, 512, 768, 1024, 1536, 4096}
	hammerConcurrently(t, d, offsets, 1536)
}

// TestConcurrentReadsVHDXPartialBlocks exercises the VHDX state-7 path, whose
// bitmap handling is separate code from the VHD path.
func TestConcurrentReadsVHDXPartialBlocks(t *testing.T) {
	parentImg, childImg := vhdxDiffFixtures(t, []int{0, 2, 4, 6})

	parent, err := OpenVHDX(bytes.NewReader(parentImg), int64(len(parentImg)))
	if err != nil {
		t.Fatalf("OpenVHDX(parent): %v", err)
	}
	defer parent.Close()
	child, err := OpenVHDX(bytes.NewReader(childImg), int64(len(childImg)))
	if err != nil {
		t.Fatalf("OpenVHDX(child): %v", err)
	}
	defer child.Close()
	if err := child.SetParent(parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}

	var offsets []int64
	for s := int64(0); s < 8; s++ {
		offsets = append(offsets, s*vhdxDiffSectorSize)
	}
	hammerConcurrently(t, child, offsets, 3*vhdxDiffSectorSize)
}

// TestConcurrentReadsLogReplayOverlay covers the replay overlay, whose sector map
// is shared across readers. It is immutable after Analyze, so concurrent map
// reads are safe — this test is what holds that property in place.
func TestConcurrentReadsLogReplayOverlay(t *testing.T) {
	p := vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateNone,
		payload:         repeatByte(0xD7, vhdxDiffBlockSize),
	}
	p.logWrites = []logWrite{{fileOffset: vhdxBATRegionOff, data: batSectorWithBlock0(p)}}
	img := buildVHDX(p)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()
	if !d.LogReplayed() {
		t.Fatal("LogReplayed() = false; the overlay is not in play")
	}

	offsets := []int64{0, 512, 4096, 65536, vhdxDiffBlockSize - 512}
	hammerConcurrently(t, d, offsets, 4096)
}

// TestConcurrentExtentsAndReads runs extent mapping alongside reads. Extents
// walks the same BATs and sector bitmaps the read path uses, so the two must not
// interfere.
func TestConcurrentExtentsAndReads(t *testing.T) {
	parentImg, childImg, _ := diffVHDPair("parent.vhd")
	dir := writeChainDir(t, map[string][]byte{
		"parent.vhd": parentImg,
		"child.vhd":  childImg,
	})

	d, err := OpenFile(filepath.Join(dir, "child.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()
	if d.NeedsParent() {
		t.Fatalf("chain incomplete: %v", d.ParentResolveError())
	}

	reference, err := d.Extents(0, 8*512)
	if err != nil {
		t.Fatalf("Extents: %v", err)
	}

	workers := runtime.GOMAXPROCS(0) * 2
	if workers < 4 {
		workers = 4
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			buf := make([]byte, 2048)

			for r := 0; r < 20; r++ {
				if worker%2 == 0 {
					got, err := d.Extents(0, 8*512)
					if err != nil {
						mu.Lock()
						failures = append(failures, "Extents: "+err.Error())
						mu.Unlock()
						return
					}
					if len(got) != len(reference) {
						mu.Lock()
						failures = append(failures, "concurrent Extents returned a different extent count")
						mu.Unlock()
						return
					}
					for i := range got {
						if got[i] != reference[i] {
							mu.Lock()
							failures = append(failures, "concurrent Extents disagrees with the reference")
							mu.Unlock()
							return
						}
					}
					continue
				}

				if _, err := d.ReadAt(buf, int64(r%8)*512); err != nil && !errors.Is(err, io.EOF) {
					mu.Lock()
					failures = append(failures, "ReadAt: "+err.Error())
					mu.Unlock()
					return
				}
			}
		}(w)
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, f := range failures {
		if !seen[f] {
			seen[f] = true
			t.Error(f)
		}
	}
}

// TestConcurrentReadsRespectUnderlyingReader documents the boundary of the
// guarantee: safety is inherited from the backing io.ReaderAt. A reader that is
// itself unsafe makes the Disk unsafe, so callers supplying their own must ensure
// it tolerates concurrent ReadAt.
func TestConcurrentReadsRespectUnderlyingReader(t *testing.T) {
	const size = 8192
	img := buildFixedVHD(size, func(p []byte) {
		for i := range p {
			p[i] = byte(i % 251)
		}
	})

	var calls int64
	var mu sync.Mutex
	base := bytes.NewReader(img)

	// A counting wrapper, itself safe, so the test verifies the Disk adds no
	// unsynchronised state of its own.
	counted := readerAtFunc(func(p []byte, off int64) (int, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return base.ReadAt(p, off)
	})

	d, err := Open(counted, &Options{Size: int64(len(img))})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	hammerConcurrently(t, d, []int64{0, 1024, 4096, size - 512}, 1024)

	mu.Lock()
	defer mu.Unlock()
	if calls == 0 {
		t.Error("the underlying reader was never called")
	}
}
