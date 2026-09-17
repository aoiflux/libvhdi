// SPDX-License-Identifier: MIT

// Package hfs adapts libhfs to the vhdimap.Filesystem contract, covering HFS+
// and HFSX.
//
// # Identity
//
// HFS+ has a catalog node ID but no generation counter: CNIDs are handed out
// from a counter in the volume header and are reused after a file is deleted.
// A same-CNID match across two states is therefore suggestive and not proof.
//
// libhfs recommends the pair (CNID, creation time) as a composite identity, and
// that is what this package reports: the CNID as the number, and the creation
// time in seconds as the generation. A recycled CNID almost always carries a
// different creation time, so the composite catches the reuse that the CNID
// alone would miss. It is still a synthesised counter and not one the volume
// maintains, so Capabilities.HasGeneration is false and renames are reported as
// inference.
//
// # Forks
//
// A file's resource fork is part of the file. Its ranges are included in
// ExtentsForFile alongside the data fork's, because a change confined to a
// resource fork is a change to the file, and leaving it out would report such a
// file as unchanged.
package hfs

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aoiflux/libhfs"
	"github.com/aoiflux/libvhdi/change/internal/readonly"
	"github.com/aoiflux/libvhdi/vhdimap"
)

// Volume is an HFS+ volume presented as a vhdimap.Filesystem.
type Volume struct {
	v    *libhfs.Volume
	base int64
}

var _ vhdimap.Filesystem = (*Volume)(nil)

// Open reads the HFS+ volume that begins at base within r.
//
// r is the whole decoded disk and size is its length. BaseOffset is what makes
// the reported offsets disk-absolute; it also covers the HFS wrapper case, in
// which the HFS+ volume sits at an offset inside the volume libhfs was pointed
// at and its own base is added on top of this one.
func Open(r io.ReaderAt, base, size int64) (*Volume, error) {
	if r == nil {
		return nil, fmt.Errorf("hfs: nil reader")
	}
	v, err := libhfs.OpenWithConfig(readonly.Wrap(r), libhfs.Config{
		BaseOffset: base,
	})
	if err != nil {
		return nil, fmt.Errorf("hfs: open at %d: %w", base, err)
	}
	return &Volume{v: v, base: base}, nil
}

// Close releases the volume.
func (vol *Volume) Close() error { return vol.v.Close() }

// Volume exposes the underlying libhfs volume.
func (vol *Volume) Volume() *libhfs.Volume { return vol.v }

// Capabilities reports what this volume can answer.
func (vol *Volume) Capabilities() vhdimap.Capabilities {
	c := vol.v.Capabilities()
	return vhdimap.Capabilities{
		Name: strings.ToLower(string(vol.v.Kind())),
		// A CNID paired with a creation time is a composite this package
		// synthesises, not an identity the volume maintains.
		StableIdentity: false,
		HasGeneration:  false,
		// Every HFS+ catalog record carries a creation time; the format has no
		// optional timestamps in the way ext's birth time is optional.
		RecordsCreated:  true,
		RecordsAccessed: c.AccessTimes,
		RecordsChanged:  c.AttrModTimes,
		HasNamedStreams: false,
		HasJournal:      c.Journaled,
		BaseOffset:      vol.base,
	}
}

// VolumeIdentity returns the volume UUID when the volume records one, and falls
// back to the finder-info volume identifier when it does not.
func (vol *Volume) VolumeIdentity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if uuid, err := vol.v.UUID(); err == nil && uuid != "" {
		return "hfs-uuid:" + uuid, nil
	}
	id, err := vol.v.VolumeIdentifier()
	if err != nil {
		return "", fmt.Errorf("hfs: volume identity: %w", err)
	}
	return fmt.Sprintf("hfs-id:%08x%08x", id.High, id.Low), nil
}

// WalkFiles calls fn for every catalog record that names a file or directory.
//
// Thread records are skipped. They exist so a CNID can be resolved back to a
// name and are not files in their own right, so reporting them would add an
// entry per real entry, each with the same identity as the thing it describes.
func (vol *Volume) WalkFiles(ctx context.Context, fn func(vhdimap.FileEntry) error) error {
	err := vol.v.WalkPathsContext(ctx, func(path string, rec libhfs.CatalogRecord) error {
		if rec.Type != libhfs.CatalogRecordFile && rec.Type != libhfs.CatalogRecordFolder {
			return nil
		}
		return fn(entryFrom(path, rec))
	})
	if err != nil {
		return fmt.Errorf("hfs: walk: %w", err)
	}
	return nil
}

// FileByID returns one catalog record by identity.
//
// When the identity carries a generation -- a creation time, as this package
// synthesises it -- it is checked against the record's own, so a recycled CNID
// is reported as reuse rather than returned as the original file.
func (vol *Volume) FileByID(ctx context.Context, id vhdimap.FileID) (vhdimap.FileEntry, error) {
	if err := ctx.Err(); err != nil {
		return vhdimap.FileEntry{}, err
	}
	cnid, err := cnidOf(id)
	if err != nil {
		return vhdimap.FileEntry{}, err
	}
	rec, err := vol.v.OpenCNID(cnid)
	if err != nil {
		return vhdimap.FileEntry{}, fmt.Errorf("hfs: cnid %d: %w", cnid, err)
	}
	if got := generationOf(rec); id.Generation != 0 && got != id.Generation {
		return vhdimap.FileEntry{}, fmt.Errorf("hfs: cnid %d was created at %d, want %d: %w",
			cnid, got, id.Generation, vhdimap.ErrIdentityReused)
	}

	path, _ := vol.v.PathForCNID(cnid)
	return entryFrom(path, rec), nil
}

// ExtentsForFile returns the disk ranges backing both forks of a file.
func (vol *Volume) ExtentsForFile(ctx context.Context, id vhdimap.FileID) ([]vhdimap.FileExtent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cnid, err := cnidOf(id)
	if err != nil {
		return nil, err
	}

	data, err := vol.v.DataForkRanges(cnid)
	if err != nil {
		return nil, fmt.Errorf("hfs: cnid %d data fork: %w", cnid, err)
	}
	out := convert(data)

	// A file with no resource fork is the common case and reports an error
	// rather than an empty list, so the failure is tolerated: losing the data
	// fork's map because a fork that does not exist could not be read would be
	// the wrong trade.
	// The resource fork's runs carry their own fork offsets, which restart at
	// zero. They are appended rather than rebased onto the data fork, because a
	// rebased offset would name a position in a file that does not exist.
	if rsrc, err := vol.v.ResourceForkRanges(cnid); err == nil {
		out = append(out, convert(rsrc)...)
	}
	return out, nil
}

func convert(rs []libhfs.ByteRange) []vhdimap.FileExtent {
	out := make([]vhdimap.FileExtent, 0, len(rs))
	for _, r := range rs {
		if r.Length <= 0 {
			continue
		}
		out = append(out, vhdimap.FileExtent{
			ByteRange:  vhdimap.ByteRange{Offset: r.DiskOffset, Length: r.Length},
			FileOffset: r.ForkOffset,
		})
	}
	return out
}

func entryFrom(path string, rec libhfs.CatalogRecord) vhdimap.FileEntry {
	return vhdimap.FileEntry{
		ID: vhdimap.FileID{
			Number:     uint64(rec.CNID),
			Generation: generationOf(rec),
		},
		ParentID: vhdimap.FileID{Number: uint64(rec.ParentCNID)},
		Name:     rec.Name,
		Path:     path,
		Size:     int64(rec.DataFork.LogicalSize),
		IsDir:    rec.Type == libhfs.CatalogRecordFolder,
		Times: vhdimap.Times{
			Created:  rec.Times.Created,
			Modified: rec.Times.ContentModified,
			Accessed: rec.Times.Accessed,
			Changed:  rec.Times.AttrModified,
		},
	}
}

// generationOf synthesises a reuse counter from the record's creation time.
//
// A zero creation time yields a zero generation, which reads as "no composite
// available" and makes FileByID fall back to matching on the CNID alone. That
// is weaker, and it is better than inventing an epoch date and comparing two
// files on it.
func generationOf(rec libhfs.CatalogRecord) uint64 {
	if rec.Times.Created.IsZero() {
		return 0
	}
	sec := rec.Times.Created.Unix()
	if sec < 0 {
		return 0
	}
	return uint64(sec)
}

func cnidOf(id vhdimap.FileID) (uint32, error) {
	if id.Number == 0 || id.Number > 0xFFFFFFFF {
		return 0, fmt.Errorf("hfs: %d is not a CNID", id.Number)
	}
	return uint32(id.Number), nil
}
