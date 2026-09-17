// SPDX-License-Identifier: MIT

package reader

import (
	"context"
	"fmt"
)

// Block-level change tracking, the first of three tiers.
//
// This tier is useful on its own and is all most consumers need: given a
// differencing chain, it says which ranges of the virtual disk a checkpoint
// wrote. That is enough for delta imaging, targeted acquisition, or driving
// analysis a caller writes itself, and it needs no filesystem knowledge at all
// -- which is why it lives in the core, where the module has no dependencies.
//
// Turning changed *ranges* into changed *files* needs a filesystem parser. That
// is a separate module, and not importing it costs nothing.

// ChangedExtents returns the ranges of the virtual disk that were written by
// the disks nearer the leaf than sinceChainIndex.
//
// Chain indices count outward from the disk this is called on: 0 is that disk,
// 1 its parent, and so on, matching Chain and Extent.ChainIndex. So on a chain
// of leaf, checkpoint, base:
//
//	ChangedExtents(ctx, 1) // what the leaf wrote
//	ChangedExtents(ctx, 2) // what the leaf and the checkpoint wrote together
//
// A returned extent is either ExtentMapped, meaning a disk in that range wrote
// data, or ExtentZeroedByChild, meaning a disk explicitly cleared the region.
// The second kind matters as much as the first: clearing a region is a write,
// and a deletion that zeroes its blocks shows up only this way. Filtering on
// ExtentMapped alone silently under-reports deletions.
//
// Ranges the chain never wrote, and ranges supplied by a disk at or beyond
// sinceChainIndex, are not returned -- they are unchanged by definition.
//
// An unresolved range, one that needs a parent that is not attached, is
// returned as ExtentUnresolved. It is neither changed nor unchanged: the disk
// that would say is missing. Reporting it as unchanged would be a guess in the
// direction that loses evidence.
func (d *VirtualDisk) ChangedExtents(ctx context.Context, sinceChainIndex int) ([]Extent, error) {
	if sinceChainIndex <= 0 {
		return nil, fmt.Errorf("libvhdi: sinceChainIndex must be >= 1, got %d", sinceChainIndex)
	}

	all, err := d.AllExtentsContext(ctx)
	if err != nil {
		return nil, err
	}

	var out []Extent
	for _, e := range all {
		switch {
		case e.Kind == ExtentUnresolved:
			// Only report the gap when it lies within the range being asked
			// about. A parent beyond sinceChainIndex being absent says nothing
			// about what changed since it.
			if e.ChainIndex <= sinceChainIndex {
				out = append(out, e)
			}

		case e.Kind.IsWrite() && e.ChainIndex < sinceChainIndex:
			out = append(out, e)
		}
	}

	return mergeExtents(out), nil
}

// ChangedBytes returns how many bytes the disks nearer the leaf than
// sinceChainIndex wrote.
//
// Zeroed ranges count: clearing a region is a write, and a caller sizing a
// delta acquisition has to account for the fact that those ranges must be
// zeroed at the destination rather than skipped.
//
// Unresolved ranges are excluded and reported separately, because counting them
// either way would be a guess. Use ChangedExtents when the distinction matters.
func (d *VirtualDisk) ChangedBytes(ctx context.Context, sinceChainIndex int) (written, unresolved int64, err error) {
	extents, err := d.ChangedExtents(ctx, sinceChainIndex)
	if err != nil {
		return 0, 0, err
	}

	for _, e := range extents {
		switch {
		case e.Kind == ExtentUnresolved:
			unresolved += e.Length
		case e.Kind.IsWrite():
			written += e.Length
		}
	}
	return written, unresolved, nil
}

// ChangedSince returns the ranges written since the given disk in the chain was
// the leaf.
//
// It is ChangedExtents addressed by path rather than by index, which is how a
// caller holding a checkpoint tree naturally thinks about the question: "what
// changed since this checkpoint?" rather than "what changed since chain index
// two?".
func (d *VirtualDisk) ChangedSince(ctx context.Context, path string) ([]Extent, error) {
	want := normalizePath(path)

	for i, entry := range d.Chain() {
		if entry.Path == "" || normalizePath(entry.Path) != want {
			continue
		}
		if i == 0 {
			// The leaf itself. Nothing has been written since it, by definition.
			return nil, nil
		}
		return d.ChangedExtents(ctx, i)
	}

	return nil, fmt.Errorf("%w: %s is not in this chain", ErrNotFound, path)
}
