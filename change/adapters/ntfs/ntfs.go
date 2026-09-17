// SPDX-License-Identifier: MIT

// Package ntfs adapts libntfs to the vhdimap.Filesystem contract.
//
// NTFS is the only filesystem in the set with a change journal, so it is the
// only one where a rename is read from a record of the rename having happened
// rather than inferred from a file keeping its identity across two states. That
// is what vhdimap.Journal is for, and why this package implements it and the
// others do not.
package ntfs

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/aoiflux/libntfs"
	"github.com/aoiflux/libvhdi/change/internal/readonly"
	"github.com/aoiflux/libvhdi/vhdimap"
)

// Volume is an NTFS volume presented as a vhdimap.Filesystem.
type Volume struct {
	v    *libntfs.Volume
	base int64
}

var (
	_ vhdimap.Filesystem = (*Volume)(nil)
	_ vhdimap.Journal    = (*Volume)(nil)
)

// Open reads the NTFS volume that begins at base within r.
//
// r is the whole decoded disk and size is its length, so every offset this
// volume reports is disk-absolute, which is what vhdimap.ByteRange requires.
// The volume is opened read-only and the reader is additionally wrapped so that
// no write can reach it even if libntfs later changes how it treats a reader
// that happens to implement io.WriterAt.
func Open(r io.ReaderAt, base, size int64) (*Volume, error) {
	if r == nil {
		return nil, fmt.Errorf("ntfs: nil reader")
	}
	v, err := libntfs.OpenWithOptions(readonly.Wrap(r), libntfs.Options{
		ReadOnly:   true,
		BaseOffset: base,
	})
	if err != nil {
		return nil, fmt.Errorf("ntfs: open at %d: %w", base, err)
	}
	if v.IsWritable() {
		// Unreachable as libntfs stands. If it is ever reached, the value in
		// hand is a write handle on evidence, and closing it is the only
		// acceptable response.
		v.Close()
		return nil, fmt.Errorf("ntfs: volume at %d opened writable despite ReadOnly", base)
	}
	return &Volume{v: v, base: base}, nil
}

// Close releases the volume.
func (vol *Volume) Close() error { return vol.v.Close() }

// Volume exposes the underlying libntfs volume, for a caller that wants
// something this adapter does not surface.
func (vol *Volume) Volume() *libntfs.Volume { return vol.v }

// Capabilities reports what this volume can answer.
func (vol *Volume) Capabilities() vhdimap.Capabilities {
	c := vol.v.Capabilities()
	return vhdimap.Capabilities{
		Name:            "ntfs",
		StableIdentity:  c.StableFileIdentity,
		HasGeneration:   c.IdentityReuseCounter,
		RecordsCreated:  c.CreationTimes,
		RecordsAccessed: c.AccessTimes,
		RecordsChanged:  c.MetadataChangeTimes,
		HasNamedStreams: c.AlternateDataStreams,
		HasJournal:      c.ChangeJournal,
		BaseOffset:      vol.base,
	}
}

// VolumeIdentity returns the boot sector's volume serial number.
func (vol *Volume) VolumeIdentity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "ntfs-serial:" + strconv.FormatUint(vol.v.VolumeSerialNumber(), 16), nil
}

// WalkFiles calls fn for every MFT record that parses.
//
// One entry is reported per file, never one per data stream. A change confined
// to an alternate data stream is reported as a change to the file that owns it,
// because ExtentsForFile is keyed on identity alone and NTFS gives a stream no
// identity of its own. Capabilities.HasNamedStreams says such streams exist, so
// a consumer can qualify that attribution rather than be misled by it.
func (vol *Volume) WalkFiles(ctx context.Context, fn func(vhdimap.FileEntry) error) error {
	err := vol.v.WalkFiles(ctx, libntfs.WalkConfig{}, func(path string, parentRef uint64, parentSeq uint16, e *libntfs.MFTEntry) error {
		entry := vhdimap.FileEntry{
			ID: vhdimap.FileID{
				Number:     uint64(e.RecordNumber),
				Generation: uint64(e.SequenceNum),
			},
			ParentID: vhdimap.FileID{
				Number:     libntfs.FileReferenceEntry(parentRef),
				Generation: uint64(parentSeq),
			},
			Path:    path,
			Name:    baseName(path),
			IsDir:   e.IsDirectory(),
			Deleted: !e.IsInUse(),
		}

		// A record with no readable $FILE_NAME still has an identity and still
		// occupies clusters. It is reported rather than dropped: an unnamed
		// record whose clusters changed is evidence, and silence about it is
		// indistinguishable from it not having changed.
		if fname, err := e.GetFileName(); err == nil && fname != nil {
			if entry.Name == "" {
				entry.Name = fname.Name
			}
			entry.Size = int64(fname.RealSize)
		}
		if si, err := e.GetStandardInformation(); err == nil && si != nil {
			entry.Times = vhdimap.Times{
				Created:  si.CreateTime,
				Modified: si.ModifyTime,
				Accessed: si.AccessTime,
				Changed:  si.MFTChangeTime,
			}
		}
		return fn(entry)
	})
	if err != nil {
		return fmt.Errorf("ntfs: walk: %w", err)
	}
	return nil
}

// FileByID returns one record by MFT reference.
//
// The generation is checked. A record whose sequence number has moved on holds
// a different file in the same slot, and returning it is the precise mistake
// the reuse counter exists to prevent, so that case is ErrIdentityReused rather
// than a successful lookup of the wrong file.
func (vol *Volume) FileByID(ctx context.Context, id vhdimap.FileID) (vhdimap.FileEntry, error) {
	if err := ctx.Err(); err != nil {
		return vhdimap.FileEntry{}, err
	}
	e, err := vol.v.GetMFTEntry(id.Number)
	if err != nil {
		return vhdimap.FileEntry{}, fmt.Errorf("ntfs: entry %d: %w", id.Number, err)
	}
	if uint64(e.SequenceNum) != id.Generation {
		return vhdimap.FileEntry{}, fmt.Errorf("ntfs: entry %d has sequence %d, want %d: %w",
			id.Number, e.SequenceNum, id.Generation, vhdimap.ErrIdentityReused)
	}

	entry := vhdimap.FileEntry{
		ID:      id,
		IsDir:   e.IsDirectory(),
		Deleted: !e.IsInUse(),
	}
	if p, err := vol.v.PathFor(id.Number); err == nil {
		entry.Path = p
		entry.Name = baseName(p)
	}
	if fname, err := e.GetFileName(); err == nil && fname != nil {
		if entry.Name == "" {
			entry.Name = fname.Name
		}
		entry.Size = int64(fname.RealSize)
		entry.ParentID = vhdimap.FileID{
			Number:     libntfs.FileReferenceEntry(fname.ParentDirectory),
			Generation: uint64(fname.ParentSeqNum),
		}
	}
	if si, err := e.GetStandardInformation(); err == nil && si != nil {
		entry.Times = vhdimap.Times{
			Created:  si.CreateTime,
			Modified: si.ModifyTime,
			Accessed: si.AccessTime,
			Changed:  si.MFTChangeTime,
		}
	}
	return entry, nil
}

// ExtentsForFile returns the disk ranges backing every data stream of a file.
//
// Named streams are included. A file whose only change is inside an alternate
// stream would otherwise intersect nothing and be reported as unchanged, which
// is how a tool misses data deliberately hidden in one.
//
// Resident content -- a small file living inside its own MFT record -- yields
// no range. Its bytes sit in the MFT's clusters, which any other record in the
// same cluster also occupies, so reporting them would attribute one file's
// change to every file that happens to share the cluster.
func (vol *Volume) ExtentsForFile(ctx context.Context, id vhdimap.FileID) ([]vhdimap.FileExtent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e, err := vol.v.GetMFTEntry(id.Number)
	if err != nil {
		return nil, fmt.Errorf("ntfs: entry %d: %w", id.Number, err)
	}
	if uint64(e.SequenceNum) != id.Generation {
		return nil, fmt.Errorf("ntfs: entry %d: %w", id.Number, vhdimap.ErrIdentityReused)
	}

	var out []vhdimap.FileExtent
	for _, attr := range e.DataStreams() {
		frags, err := vol.v.AttributeFragments(attr)
		if err != nil {
			// One unreadable stream must not cost the rest of the file's map.
			continue
		}
		for _, f := range frags {
			if f.Resident || f.Length <= 0 {
				continue
			}
			// A hole keeps its position in the stream and loses its location,
			// so the runs after it stay correctly placed.
			ext := vhdimap.FileExtent{FileOffset: f.FileOffset}
			if !f.Sparse {
				ext.ByteRange = vhdimap.ByteRange{Offset: f.StartOffset, Length: f.Length}
			}
			out = append(out, ext)
		}
	}
	return out, nil
}

// RenamesBetween returns the renames the USN journal recorded between two
// markers produced by VolumeState.
//
// NTFS writes a rename as a pair of records, the old name and then the new one,
// both against the same file reference. The pair is matched on that identity
// rather than on adjacency, so other records interleaved between them do not
// break the match.
func (vol *Volume) RenamesBetween(ctx context.Context, from, to string) ([]vhdimap.RenameEvidence, error) {
	lo, err := parseMarker(from)
	if err != nil {
		return nil, err
	}
	hi, err := parseMarker(to)
	if err != nil {
		return nil, err
	}

	type pending struct {
		name   string
		parent vhdimap.FileID
	}
	olds := make(map[vhdimap.FileID]pending)
	var out []vhdimap.RenameEvidence

	err = vol.v.EachUSNRecordContext(ctx, func(rec *libntfs.USNRecord) error {
		if rec.USN < lo || (hi > 0 && rec.USN > hi) {
			return nil
		}
		id := vhdimap.FileID{
			Number:     libntfs.FileReferenceEntry(rec.FileReference),
			Generation: uint64(rec.FileSequence),
		}
		parent := vhdimap.FileID{
			Number:     libntfs.FileReferenceEntry(rec.ParentReference),
			Generation: uint64(rec.ParentSequence),
		}
		switch {
		case rec.Reason&libntfs.USNReasonRenameOldName != 0:
			olds[id] = pending{name: rec.Name, parent: parent}
		case rec.Reason&libntfs.USNReasonRenameNewName != 0:
			// A missing old-name record is ordinary on a busy volume: the
			// journal is circular and the first half may have aged out. The new
			// name still proves a rename happened, so it is reported with an
			// unknown origin rather than discarded.
			old := olds[id]
			delete(olds, id)
			out = append(out, vhdimap.RenameEvidence{
				ID:        id,
				OldName:   old.name,
				NewName:   rec.Name,
				OldParent: old.parent,
				NewParent: parent,
				When:      rec.Timestamp,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ntfs: usn journal: %w", err)
	}
	return out, nil
}

// VolumeState returns the highest USN present on this volume, as an opaque
// marker for RenamesBetween.
func (vol *Volume) VolumeState(ctx context.Context) (string, error) {
	var highest int64
	err := vol.v.EachUSNRecordContext(ctx, func(rec *libntfs.USNRecord) error {
		if rec.USN > highest {
			highest = rec.USN
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("ntfs: usn journal: %w", err)
	}
	return "usn:" + strconv.FormatInt(highest, 10), nil
}

func parseMarker(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(strings.TrimPrefix(s, "usn:"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ntfs: bad journal marker %q: %w", s, err)
	}
	return v, nil
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
