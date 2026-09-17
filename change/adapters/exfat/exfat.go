// SPDX-License-Identifier: MIT

// Package exfat adapts libxfat to the vhdimap.Filesystem contract.
//
// exFAT shares FAT's identity problem and this package shares the fat package's
// answer to it: identity is the pair of the parent directory's first cluster
// and the entry set's slot index within that directory, packed into one number,
// with the generation left zero because exFAT records no reuse counter.
// Capabilities.StableIdentity is false in consequence, so renames here are
// inference and are reported as such.
//
// An index is built on first use for the same reason it is in the fat package:
// exFAT has no inode table, so resolving an identity back to a directory record
// means having read the directory that holds it.
//
// # Concurrency
//
// libxfat documents its ExFAT value as unsafe for concurrent use, keeping
// parser state on it. This adapter therefore serialises every call that reaches
// the library. vhdimap.Filesystem requires an implementation to be safe on one
// value or to say that it is not, and taking a lock is the way to satisfy that
// rather than push the problem onto the caller.
package exfat

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/aoiflux/libvhdi/change/internal/readonly"
	"github.com/aoiflux/libvhdi/vhdimap"
	"github.com/aoiflux/libxfat"
)

// Volume is an exFAT volume presented as a vhdimap.Filesystem.
type Volume struct {
	base int64

	// mu guards e as well as the index: libxfat keeps parser state on the
	// ExFAT value, so two concurrent directory reads corrupt each other.
	mu      sync.Mutex
	e       *libxfat.ExFAT
	index   map[vhdimap.FileID]libxfat.Entry
	indexed bool
}

var _ vhdimap.Filesystem = (*Volume)(nil)

// Open reads the exFAT volume that begins at base within r.
//
// r is the whole decoded disk and size is its length. Base makes every offset
// libxfat reports disk-absolute, and IgnorePartitionOffset is set because a
// volume inside a virtual disk routinely records a zero PartitionOffset in its
// boot sector: trusting that field would relocate every offset to the start of
// the disk, and the base passed here is the fact that matters.
func Open(r io.ReaderAt, base, size int64) (*Volume, error) {
	if r == nil {
		return nil, fmt.Errorf("exfat: nil reader")
	}
	e, err := libxfat.Open(libxfat.Source{
		Reader:                readonly.Wrap(r),
		Size:                  size,
		Base:                  base,
		IgnorePartitionOffset: true,
	})
	if err != nil {
		return nil, fmt.Errorf("exfat: open at %d: %w", base, err)
	}
	return &Volume{e: e, base: base}, nil
}

// Close releases the volume. libxfat holds no handle of its own, so this is
// here for symmetry with the other adapters.
func (vol *Volume) Close() error { return nil }

// Capabilities reports what this volume can answer.
func (vol *Volume) Capabilities() vhdimap.Capabilities {
	vol.mu.Lock()
	defer vol.mu.Unlock()

	c := vol.e.Capabilities()
	return vhdimap.Capabilities{
		Name:            "exfat",
		StableIdentity:  c.StableFileIdentity,
		HasGeneration:   c.IdentityReuseCounter,
		RecordsCreated:  c.CreationTimes,
		RecordsAccessed: c.AccessTimes,
		RecordsChanged:  c.MetadataChangeTimes,
		HasNamedStreams: false,
		HasJournal:      false,
		BaseOffset:      vol.base,
	}
}

// VolumeIdentity returns the boot sector's volume serial number, which carries
// the same weak guarantee as FAT's: disagreement proves two different volumes,
// agreement is only consistent with one.
func (vol *Volume) VolumeIdentity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	vol.mu.Lock()
	defer vol.mu.Unlock()
	return "exfat-serial:" + strconv.FormatUint(uint64(vol.e.VolumeSerialNumber()), 16), nil
}

// WalkFiles calls fn for every entry in the reachable directory tree.
//
// fn runs outside the library lock, so a callback that itself calls back into
// this volume does not deadlock. The entries are collected first for that
// reason; on a volume large enough for that to matter, exFAT's absence of an
// inode table means an index has to be held anyway.
func (vol *Volume) WalkFiles(ctx context.Context, fn func(vhdimap.FileEntry) error) error {
	if err := vol.buildIndex(ctx); err != nil {
		return err
	}

	vol.mu.Lock()
	entries := make([]vhdimap.FileEntry, 0, len(vol.index))
	for id, e := range vol.index {
		entries = append(entries, entryFrom(id, e))
	}
	vol.mu.Unlock()

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(entry); err != nil {
			return err
		}
	}
	return nil
}

// FileByID returns one directory record by identity, from the index.
func (vol *Volume) FileByID(ctx context.Context, id vhdimap.FileID) (vhdimap.FileEntry, error) {
	e, err := vol.lookup(ctx, id)
	if err != nil {
		return vhdimap.FileEntry{}, err
	}
	return entryFrom(id, e), nil
}

// ExtentsForFile returns the disk ranges backing a file.
func (vol *Volume) ExtentsForFile(ctx context.Context, id vhdimap.FileID) ([]vhdimap.FileExtent, error) {
	e, err := vol.lookup(ctx, id)
	if err != nil {
		return nil, err
	}

	vol.mu.Lock()
	ranges, err := vol.e.FragmentOffsets(e)
	vol.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("exfat: fragments for %q: %w", e.Name(), err)
	}

	out := make([]vhdimap.FileExtent, 0, len(ranges))
	for _, r := range ranges {
		if r.Length <= 0 {
			continue
		}
		out = append(out, vhdimap.FileExtent{
			ByteRange:  vhdimap.ByteRange{Offset: r.StartByte, Length: r.Length},
			FileOffset: r.FileOffset,
		})
	}
	return out, nil
}

func entryFrom(id vhdimap.FileID, e libxfat.Entry) vhdimap.FileEntry {
	return vhdimap.FileEntry{
		ID:       id,
		ParentID: vhdimap.FileID{Number: uint64(e.ParentFirstCluster()) << 32},
		Name:     e.Name(),
		Size:     int64(e.Size()),
		IsDir:    e.IsDir(),
		Deleted:  e.IsDeleted(),
		Times: vhdimap.Times{
			Created:  e.CreatedTime(),
			Modified: e.ModifiedTime(),
			Accessed: e.AccessedTime(),
		},
	}
}

func (vol *Volume) lookup(ctx context.Context, id vhdimap.FileID) (libxfat.Entry, error) {
	if err := vol.buildIndex(ctx); err != nil {
		return libxfat.Entry{}, err
	}
	vol.mu.Lock()
	e, ok := vol.index[id]
	vol.mu.Unlock()
	if !ok {
		return libxfat.Entry{}, fmt.Errorf("exfat: no entry at slot %d of directory cluster %d",
			uint32(id.Number), uint32(id.Number>>32))
	}
	return e, nil
}

func (vol *Volume) buildIndex(ctx context.Context) error {
	vol.mu.Lock()
	defer vol.mu.Unlock()

	if vol.indexed {
		return nil
	}

	index := make(map[vhdimap.FileID]libxfat.Entry)
	err := vol.e.Walk(ctx, func(path string, parentFirstCluster uint32, entry libxfat.Entry) error {
		fid, ok := vol.e.FileID(entry)
		if !ok {
			return nil
		}
		index[pack(fid.ParentFirstCluster, fid.EntrySlotIndex)] = entry
		return nil
	})
	if err != nil {
		// indexed stays false, so a cancelled walk is retried rather than
		// leaving a partial index to be mistaken for the whole volume.
		return fmt.Errorf("exfat: index: %w", err)
	}

	vol.index = index
	vol.indexed = true
	return nil
}

func pack(parentFirstCluster, entrySlotIndex uint32) vhdimap.FileID {
	return vhdimap.FileID{Number: uint64(parentFirstCluster)<<32 | uint64(entrySlotIndex)}
}
