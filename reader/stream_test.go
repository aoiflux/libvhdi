// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// The README described sparse acquisition in prose and the library gave callers
// no way to do it: the only whole-device read was ReadAt in a loop, which moves
// the entire virtual size through memory including the zeroes the image never
// stored. Stream reads what is backed and skips the rest.

// sparseVHDX returns an image whose first block is present and whose remaining
// three are not: 4 MB of address space holding 1 MB of data.
func sparseVHDX(t *testing.T, fill byte) []byte {
	t.Helper()
	return buildVHDX(vhdxParams{
		virtualDiskSize: 4 * 1024 * 1024,
		blockSize:       1024 * 1024,
		sectorSize:      512,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(fill, 1024*1024),
	})
}

func TestStreamSkipsSparseRegions(t *testing.T) {
	img := sparseVHDX(t, 0xA7)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	var streamed int64
	var kinds []ExtentKind
	err = d.Stream(context.Background(), nil, func(r Run) error {
		kinds = append(kinds, r.Extent.Kind)
		streamed += int64(len(r.Data))
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	for _, k := range kinds {
		if k != ExtentMapped {
			t.Fatalf("Stream emitted a %v run by default; only mapped runs should appear", k)
		}
	}

	// The whole point: a 4 MB device holding 1 MB of data costs 1 MB of reads.
	if streamed != 1024*1024 {
		t.Fatalf("streamed %d bytes, want the %d that are actually backed", streamed, 1024*1024)
	}
	if want := int64(d.Size()); streamed >= want {
		t.Fatalf("streamed %d bytes of a %d byte device; the sparse regions were not skipped",
			streamed, want)
	}
}

func TestStreamReproducesTheDeviceExactly(t *testing.T) {
	// Skipping sparse regions is only correct if the runs that are emitted,
	// placed at their virtual offsets with zeroes elsewhere, reconstruct the
	// device byte for byte.
	img := sparseVHDX(t, 0x5C)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	rebuilt := make([]byte, d.Size())
	err = d.Stream(context.Background(), nil, func(r Run) error {
		copy(rebuilt[r.Extent.VirtualOffset:], r.Data)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	direct := make([]byte, d.Size())
	if _, err := io.ReadFull(d.SectionReader(), direct); err != nil {
		t.Fatalf("reading the device directly: %v", err)
	}

	if !bytes.Equal(rebuilt, direct) {
		t.Fatal("the streamed runs do not reconstruct the device")
	}
}

func TestStreamRunsNeverSpanAnExtent(t *testing.T) {
	// Every run must carry one kind, one backing file and one chain index, or a
	// caller cannot record provenance per run.
	img := sparseVHDX(t, 0x11)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	extents, err := d.AllExtents()
	if err != nil {
		t.Fatalf("AllExtents: %v", err)
	}

	err = d.Stream(context.Background(), &StreamOptions{IncludeZero: true}, func(r Run) error {
		for _, e := range extents {
			if r.Extent.VirtualOffset >= e.VirtualOffset && r.Extent.End() <= e.End() {
				if r.Extent.Kind != e.Kind {
					t.Errorf("run at %d has kind %v, but the extent containing it is %v",
						r.Extent.VirtualOffset, r.Extent.Kind, e.Kind)
				}
				return nil
			}
		}
		t.Errorf("run [%d,%d) is not contained in any extent", r.Extent.VirtualOffset, r.Extent.End())
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
}

func TestStreamSplitsLongExtentsAtTheBufferSize(t *testing.T) {
	// A mapped extent can be the whole device, so it must be emitted in pieces
	// rather than materialised in memory.
	img := sparseVHDX(t, 0x22)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	const bufSize = 64 * 1024
	runs := 0
	err = d.Stream(context.Background(), &StreamOptions{BufferSize: bufSize}, func(r Run) error {
		runs++
		if len(r.Data) > bufSize {
			t.Fatalf("run of %d bytes exceeds the %d byte buffer", len(r.Data), bufSize)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if want := (1024 * 1024) / bufSize; runs != want {
		t.Fatalf("emitted %d runs for 1 MB at a %d byte buffer, want %d", runs, bufSize, want)
	}
}

func TestStreamFileOffsetTracksTheSplit(t *testing.T) {
	// When a mapped extent is split, each piece's FileOffset has to advance
	// with it. A piece carrying the parent extent's offset would point a
	// sparse-aware copier at the wrong bytes.
	img := sparseVHDX(t, 0x33)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	var prev Extent
	first := true
	err = d.Stream(context.Background(), &StreamOptions{BufferSize: 64 * 1024}, func(r Run) error {
		if r.Extent.Kind != ExtentMapped {
			return nil
		}
		if !first && r.Extent.VirtualOffset == prev.End() {
			if got, want := r.Extent.FileOffset, prev.FileOffset+prev.Length; got != want {
				t.Fatalf("run at virtual %d has file offset %d, want %d",
					r.Extent.VirtualOffset, got, want)
			}
			// And the file offset must actually name the run's bytes.
			check := make([]byte, 8)
			if _, err := bytes.NewReader(img).ReadAt(check, r.Extent.FileOffset); err != nil {
				t.Fatalf("reading at the reported file offset: %v", err)
			}
			if !bytes.Equal(check, r.Data[:8]) {
				t.Fatalf("file offset %d does not hold the run's bytes", r.Extent.FileOffset)
			}
		}
		prev, first = r.Extent, false
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
}

func TestStreamIncludeZeroEmitsSparseRuns(t *testing.T) {
	// A consumer that must account for every byte of the address space needs
	// the holes too, with no data behind them.
	img := sparseVHDX(t, 0x44)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	var covered int64
	sawZero := false
	err = d.Stream(context.Background(), &StreamOptions{IncludeZero: true}, func(r Run) error {
		covered += r.Extent.Length
		if r.Extent.Kind == ExtentZero {
			sawZero = true
			if r.Data != nil {
				t.Error("a sparse run carries data")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if !sawZero {
		t.Fatal("IncludeZero emitted no sparse runs")
	}
	if covered != int64(d.Size()) {
		t.Fatalf("runs cover %d bytes of a %d byte device", covered, d.Size())
	}
}

func TestStreamStopsOnVisitError(t *testing.T) {
	img := sparseVHDX(t, 0x55)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	sentinel := errors.New("stop here")
	calls := 0
	err = d.Stream(context.Background(), &StreamOptions{BufferSize: 64 * 1024}, func(Run) error {
		calls++
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("Stream returned %v, want the visit function's error", err)
	}
	if calls != 1 {
		t.Fatalf("visit called %d times after returning an error, want 1", calls)
	}
}

func TestStreamIsCancellable(t *testing.T) {
	img := sparseVHDX(t, 0x66)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = d.Stream(ctx, nil, func(Run) error {
		t.Fatal("visit called on a cancelled context")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream returned %v, want context.Canceled", err)
	}
}

func TestStreamFailOnUnresolvedStops(t *testing.T) {
	// A differencing disk with no parent attached. Neither continuing nor
	// failing is safe by default, so the caller chooses -- but the choice has to
	// be available.
	img := buildVHDX(vhdxParams{
		virtualDiskSize: 2 * 1024 * 1024,
		blockSize:       1024 * 1024,
		sectorSize:      512,
		hasParent:       true,
		blockState:      types.BlockStateNone,
	})

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	err = d.Stream(context.Background(), &StreamOptions{FailOnUnresolved: true}, func(Run) error {
		return nil
	})
	if !errors.Is(err, ErrParentRequired) {
		t.Fatalf("Stream returned %v, want ErrParentRequired", err)
	}

	// And by default the run is surfaced rather than the walk failing, so a
	// caller can record which parts of the chain are missing.
	sawUnresolved := false
	err = d.Stream(context.Background(), nil, func(r Run) error {
		if r.Extent.Kind == ExtentUnresolved {
			sawUnresolved = true
			if r.Data != nil {
				t.Error("an unresolved run carries data, which would be indistinguishable from real contents")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !sawUnresolved {
		t.Fatal("no unresolved run was surfaced")
	}
}

func TestSectionReaderReadsTheWholeDevice(t *testing.T) {
	img := sparseVHDX(t, 0x77)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	sr := d.SectionReader()
	if got := sr.Size(); got != int64(d.Size()) {
		t.Fatalf("SectionReader().Size() = %d, want %d", got, d.Size())
	}

	// Unlike Stream, this reads densely: the sparse tail comes back as zeroes.
	all, err := io.ReadAll(sr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if int64(len(all)) != int64(d.Size()) {
		t.Fatalf("read %d bytes, want %d", len(all), d.Size())
	}
	if all[0] != 0x77 {
		t.Fatalf("first byte is %#x, want %#x", all[0], 0x77)
	}
	for i := 1024 * 1024; i < len(all); i++ {
		if all[i] != 0 {
			t.Fatalf("byte %d of the sparse region is %#x, want 0", i, all[i])
		}
	}
}

func TestAllExtentsContextMatchesAllExtents(t *testing.T) {
	// The chunked walk exists to bound work between cancellation checks. It
	// must not change the answer, including across chunk boundaries.
	img := sparseVHDX(t, 0x88)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	plain, err := d.AllExtents()
	if err != nil {
		t.Fatalf("AllExtents: %v", err)
	}
	chunked, err := d.AllExtentsContext(context.Background())
	if err != nil {
		t.Fatalf("AllExtentsContext: %v", err)
	}

	if len(plain) != len(chunked) {
		t.Fatalf("AllExtents returned %d extents, AllExtentsContext %d", len(plain), len(chunked))
	}
	for i := range plain {
		if plain[i] != chunked[i] {
			t.Fatalf("extent %d differs:\n plain   %+v\n chunked %+v", i, plain[i], chunked[i])
		}
	}
}

func TestContextAwareReadsRespectCancellation(t *testing.T) {
	img := sparseVHDX(t, 0x99)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	buf := make([]byte, 512)
	if _, err := d.ReadAtContext(ctx, buf, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("ReadAtContext returned %v, want context.Canceled", err)
	}
	if _, err := d.ExtentsContext(ctx, 0, 512); !errors.Is(err, context.Canceled) {
		t.Errorf("ExtentsContext returned %v, want context.Canceled", err)
	}
	if _, err := d.AllExtentsContext(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("AllExtentsContext returned %v, want context.Canceled", err)
	}
	if _, err := d.MappedBytesContext(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("MappedBytesContext returned %v, want context.Canceled", err)
	}

	// And ReadAt itself must stay an io.ReaderAt, which is what makes a disk
	// composable with the standard library and with a filesystem parser.
	var _ io.ReaderAt = d
}
