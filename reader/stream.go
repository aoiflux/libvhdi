// SPDX-License-Identifier: MIT

package reader

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ============================================================================
// Context support
// ============================================================================

// ReadAtContext is ReadAt with cancellation.
//
// ReadAt itself deliberately does not take a context: io.ReaderAt is what makes
// a *VirtualDisk composable with the rest of the standard library and with a
// filesystem parser layered on top, and changing its signature would break
// that. This is the cancellable form for callers that need one.
//
// Cancellation is checked before the read begins and does not interrupt one in
// flight. A single ReadAt is bounded by the length of p; the operations worth
// cancelling are the ones that walk the whole disk, and those take a context of
// their own.
func (d *VirtualDisk) ReadAtContext(ctx context.Context, p []byte, offset int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return d.ReadAt(p, offset)
}

// ExtentsContext is Extents with cancellation.
func (d *VirtualDisk) ExtentsContext(ctx context.Context, virtualOffset, length int64) ([]Extent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.Extents(virtualOffset, length)
}

// AllExtentsContext maps the whole disk, checking for cancellation as it goes.
//
// On a large differencing chain this is the expensive call in the library: it
// walks every block of every disk in the chain and, for partially written
// blocks, every sector bitmap. A caller that has walked away should not have to
// wait for it.
func (d *VirtualDisk) AllExtentsContext(ctx context.Context) ([]Extent, error) {
	if d.virtualSize == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Walking in chunks bounds both the work done between cancellation checks
	// and the peak size of the intermediate slice. The chunks are merged at the
	// end, so the result is identical to AllExtents.
	const chunk = int64(1) << 30 // 1 GiB of virtual address space

	var out []Extent
	for at := int64(0); at < int64(d.virtualSize); at += chunk {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		span := chunk
		if remaining := int64(d.virtualSize) - at; span > remaining {
			span = remaining
		}

		part, err := d.Extents(at, span)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}

	// Chunk boundaries can split a run that is really contiguous, so merge
	// across them.
	return mergeExtents(out), nil
}

// MappedBytesContext is MappedBytes with cancellation.
func (d *VirtualDisk) MappedBytesContext(ctx context.Context) (int64, error) {
	extents, err := d.AllExtentsContext(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range extents {
		if e.Kind == ExtentMapped {
			total += e.Length
		}
	}
	return total, nil
}

// ============================================================================
// Streaming
// ============================================================================

// SectionReader returns an io.SectionReader over the whole decoded device.
//
// This is the adapter that lets a *VirtualDisk be handed to code expecting a
// Read/Seek stream -- a hasher, an archiver, a filesystem parser -- without the
// caller tracking an offset. It reads the device densely, including its sparse
// regions, which is the right behaviour for anything that needs every byte and
// the wrong one for acquisition. Use Stream for that.
func (d *VirtualDisk) SectionReader() *io.SectionReader {
	return io.NewSectionReader(d, 0, int64(d.virtualSize))
}

// Run is one contiguous piece of the device as Stream emits it.
type Run struct {
	// Extent is the run's place in the virtual address space and what backs it.
	Extent Extent

	// Data holds the run's bytes. It is valid only until the visit function
	// returns: Stream reuses one buffer across runs so a whole-disk walk does
	// not allocate per run. Copy what you need to keep.
	Data []byte
}

// StreamOptions controls a Stream walk. The zero value is usable.
type StreamOptions struct {
	// BufferSize caps how many bytes are read at once. Zero selects 4 MiB.
	//
	// A mapped extent can be the entire disk, so runs are emitted in pieces no
	// larger than this rather than materialising a whole extent in memory.
	BufferSize int

	// IncludeZero emits sparse regions as runs with a nil Data. The default
	// skips them entirely, which is the point of streaming: a 4 TB device
	// holding 8 GB of data should cost 8 GB of reads, not 4 TB.
	//
	// Turn it on when the consumer needs to account for every byte of the
	// address space rather than only the bytes that exist.
	IncludeZero bool

	// FailOnUnresolved makes an unresolved extent -- one that resolves to a
	// parent that is not attached -- stop the walk with ErrParentRequired.
	//
	// The default emits it as a run with nil Data and lets the visit function
	// decide. Neither choice is safe by default: failing loses a partial
	// acquisition that may be all there is, and continuing risks a consumer
	// treating the gap as zeroes. The kind is on every run so the decision can
	// be made with the facts.
	FailOnUnresolved bool
}

func (o *StreamOptions) bufferSize() int {
	const defaultBufferSize = 4 << 20
	if o == nil || o.BufferSize <= 0 {
		return defaultBufferSize
	}
	return o.BufferSize
}

// Stream walks the device and calls visit for each run, in virtual offset
// order, reading only what is actually backed by data.
//
// This is the sparse-acquisition pattern: on an image whose extent map shows a
// few mapped runs in a large address space, streaming reads those runs and
// nothing else. Reading the device end to end instead would move the whole
// virtual size through memory, most of it zeroes the image never stored.
//
// Runs never span an extent, so every run has one kind, one backing file and
// one chain index -- which is what lets a caller record provenance per run
// rather than per disk. A long mapped extent is split into several runs of at
// most opts.BufferSize bytes.
//
// visit returning an error stops the walk and Stream returns that error. The
// Data slice is reused between calls; copy anything that must outlive the call.
func (d *VirtualDisk) Stream(ctx context.Context, opts *StreamOptions, visit func(Run) error) error {
	if visit == nil {
		return errors.New("libvhdi: Stream needs a visit function")
	}

	extents, err := d.AllExtentsContext(ctx)
	if err != nil {
		return err
	}

	buf := make([]byte, opts.bufferSize())
	includeZero := opts != nil && opts.IncludeZero
	failUnresolved := opts != nil && opts.FailOnUnresolved

	for _, e := range extents {
		if err := ctx.Err(); err != nil {
			return err
		}

		switch e.Kind {
		case ExtentZero, ExtentZeroedByChild:
			// Both read as zeroes. The kind is carried on the run so a consumer
			// can tell an untouched hole from a region a checkpoint cleared,
			// which is the difference between "never written" and "deleted".
			if !includeZero {
				continue
			}
			if err := visit(Run{Extent: e}); err != nil {
				return err
			}
			continue

		case ExtentUnresolved:
			if failUnresolved {
				return fmt.Errorf("%w: virtual range [%d,%d) resolves to an unattached parent",
					ErrParentRequired, e.VirtualOffset, e.End())
			}
			if err := visit(Run{Extent: e}); err != nil {
				return err
			}
			continue
		}

		// A mapped extent can be the whole device, so read it in pieces.
		for at := e.VirtualOffset; at < e.End(); {
			if err := ctx.Err(); err != nil {
				return err
			}

			span := int64(len(buf))
			if remaining := e.End() - at; span > remaining {
				span = remaining
			}

			if _, err := d.ReadAt(buf[:span], at); err != nil {
				return fmt.Errorf("reading virtual range [%d,%d): %w", at, at+span, err)
			}

			piece := e
			piece.VirtualOffset = at
			piece.Length = span
			piece.FileOffset = e.FileOffset + (at - e.VirtualOffset)

			if err := visit(Run{Extent: piece, Data: buf[:span]}); err != nil {
				return err
			}
			at += span
		}
	}

	return nil
}
