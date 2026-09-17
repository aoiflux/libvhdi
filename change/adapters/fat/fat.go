// SPDX-License-Identifier: MIT

// Package fat adapts libfat to the vhdimap.Filesystem contract, covering
// FAT12, FAT16 and FAT32.
//
// # Identity on a format that has none
//
// FAT has no inode number and no reuse counter. What it has is a directory
// record at a position inside a parent directory, and libfat's FileID is that
// pair: the parent directory's first cluster, and the entry's 32-byte slot
// index within it. This package packs the pair into the single number
// vhdimap.FileID carries, and leaves the generation zero, because there is
// nothing on the volume that could fill it.
//
// The consequence is stated rather than hidden. Capabilities.StableIdentity is
// false, so a rename on FAT is reported as inference and never as proof. Two
// specific things can go wrong: a file deleted and a different one created in
// the same slot looks like a modification, and a file whose parent directory is
// rearranged looks like a delete plus an add. Neither is detectable from the
// volume, so the honest move is to say what the evidence supports and let the
// report carry the caveat.
//
// # Why this package holds an index
//
// vhdimap.ExtentsForFile is keyed on identity, and FAT cannot resolve an
// identity back to a directory record without reading the directory that holds
// it. There is no inode table to seek into. So the first call that needs one
// walks the tree and keeps the records, which is a real cost of the format
// rather than a shortcut taken here.
package fat

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/aoiflux/libfat"
	"github.com/aoiflux/libvhdi/change/internal/readonly"
	"github.com/aoiflux/libvhdi/vhdimap"
)

// Volume is a FAT volume presented as a vhdimap.Filesystem.
type Volume struct {
	v    *libfat.Volume
	base int64

	mu      sync.Mutex
	index   map[vhdimap.FileID]libfat.DirEntry
	indexed bool
}

var _ vhdimap.Filesystem = (*Volume)(nil)

// Open reads the FAT volume that begins at base within r.
//
// r is the whole decoded disk and size is its length, so every offset this
// volume reports is disk-absolute, which is what vhdimap.ByteRange requires.
func Open(r io.ReaderAt, base, size int64) (*Volume, error) {
	if r == nil {
		return nil, fmt.Errorf("fat: nil reader")
	}
	v, err := libfat.OpenWithOptions(readonly.Wrap(r), libfat.OpenOptions{
		BaseOffset: base,
	})
	if err != nil {
		return nil, fmt.Errorf("fat: open at %d: %w", base, err)
	}
	return &Volume{v: v, base: base}, nil
}

// Close releases the volume.
func (vol *Volume) Close() error { return vol.v.Close() }

// Volume exposes the underlying libfat volume.
func (vol *Volume) Volume() *libfat.Volume { return vol.v }

// Capabilities reports what this volume can answer.
func (vol *Volume) Capabilities() vhdimap.Capabilities {
	c := vol.v.Capabilities()
	return vhdimap.Capabilities{
		Name:            strings.ToLower(vol.v.FATType()),
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

// VolumeIdentity returns the boot sector's volume ID.
//
// It is 32 bits chosen at format time and nothing maintains it, so it confirms
// far less than an NTFS serial or an ext UUID does. It is still worth checking:
// two states that disagree on it are certainly different volumes, even though
// agreement is weak evidence that they are the same one.
func (vol *Volume) VolumeIdentity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	bs := vol.v.GetBootSector()
	if bs == nil {
		return "", fmt.Errorf("fat: no boot sector")
	}
	return "fat-volumeid:" + strconv.FormatUint(uint64(bs.VolumeID), 16), nil
}

// WalkFiles calls fn for every entry in the reachable directory tree.
func (vol *Volume) WalkFiles(ctx context.Context, fn func(vhdimap.FileEntry) error) error {
	err := vol.v.Walk(ctx, func(path string, parentFirstCluster uint32, e libfat.DirEntry) error {
		entry, ok := vol.entryFrom(path, parentFirstCluster, e)
		if !ok {
			// No identity could be formed, so nothing downstream could refer to
			// this record. Skipping it is the only option that does not invent
			// one.
			return nil
		}
		return fn(entry)
	})
	if err != nil {
		return fmt.Errorf("fat: walk: %w", err)
	}
	return nil
}

// FileByID returns one directory record by identity, from the index.
func (vol *Volume) FileByID(ctx context.Context, id vhdimap.FileID) (vhdimap.FileEntry, error) {
	e, err := vol.lookup(ctx, id)
	if err != nil {
		return vhdimap.FileEntry{}, err
	}
	entry, _ := vol.entryFrom(e.Path, e.ParentFirstCluster, e)
	return entry, nil
}

// ExtentsForFile returns the disk ranges backing a file.
//
// FAT has no sparse allocation: every run is backed by real clusters, so
// nothing is dropped here beyond empty runs.
func (vol *Volume) ExtentsForFile(ctx context.Context, id vhdimap.FileID) ([]vhdimap.FileExtent, error) {
	e, err := vol.lookup(ctx, id)
	if err != nil {
		return nil, err
	}
	ranges, err := vol.v.FragmentOffsets(e)
	if err != nil {
		return nil, fmt.Errorf("fat: fragments for %s: %w", e.Path, err)
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

func (vol *Volume) entryFrom(path string, parentFirstCluster uint32, e libfat.DirEntry) (vhdimap.FileEntry, bool) {
	fid, ok := vol.v.FileID(e)
	if !ok {
		return vhdimap.FileEntry{}, false
	}
	return vhdimap.FileEntry{
		ID:       pack(fid.ParentFirstCluster, fid.EntrySlotIndex),
		ParentID: vhdimap.FileID{Number: uint64(parentFirstCluster) << 32},
		Name:     e.Name,
		Path:     path,
		Size:     int64(e.Size),
		IsDir:    e.IsDirectory,
		Deleted:  e.Deleted,
		Times: vhdimap.Times{
			Created:  e.CreatedAt,
			Modified: e.ModifiedAt,
			Accessed: e.AccessedAt,
		},
	}, true
}

// lookup finds the directory record behind an identity, building the index on
// first use.
func (vol *Volume) lookup(ctx context.Context, id vhdimap.FileID) (libfat.DirEntry, error) {
	if err := vol.buildIndex(ctx); err != nil {
		return libfat.DirEntry{}, err
	}
	vol.mu.Lock()
	e, ok := vol.index[id]
	vol.mu.Unlock()
	if !ok {
		parent, slot := unpack(id)
		return libfat.DirEntry{}, fmt.Errorf("fat: no entry at slot %d of directory cluster %d", slot, parent)
	}
	return e, nil
}

func (vol *Volume) buildIndex(ctx context.Context) error {
	vol.mu.Lock()
	if vol.indexed {
		vol.mu.Unlock()
		return nil
	}
	vol.mu.Unlock()

	// The walk runs outside the lock: it is the long operation, and holding the
	// lock across it would serialise every concurrent reader behind it for no
	// benefit. Two goroutines racing here both walk and the second overwrites
	// an identical map, which costs a duplicate pass and never a wrong answer.
	index := make(map[vhdimap.FileID]libfat.DirEntry)
	err := vol.v.Walk(ctx, func(path string, parentFirstCluster uint32, e libfat.DirEntry) error {
		fid, ok := vol.v.FileID(e)
		if !ok {
			return nil
		}
		e.Path = path
		index[pack(fid.ParentFirstCluster, fid.EntrySlotIndex)] = e
		return nil
	})
	if err != nil {
		// A cancelled or failed walk leaves indexed false, so the next call
		// retries rather than inheriting a half-built index as though it were
		// the whole volume.
		return fmt.Errorf("fat: index: %w", err)
	}

	vol.mu.Lock()
	vol.index = index
	vol.indexed = true
	vol.mu.Unlock()
	return nil
}

// pack folds FAT's composite identity into the single number vhdimap.FileID
// carries. The parent's first cluster takes the high 32 bits and the slot index
// the low 32, which are the natural widths of both.
func pack(parentFirstCluster, entrySlotIndex uint32) vhdimap.FileID {
	return vhdimap.FileID{Number: uint64(parentFirstCluster)<<32 | uint64(entrySlotIndex)}
}

func unpack(id vhdimap.FileID) (parentFirstCluster, entrySlotIndex uint32) {
	return uint32(id.Number >> 32), uint32(id.Number)
}
