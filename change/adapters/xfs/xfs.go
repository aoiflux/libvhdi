// SPDX-License-Identifier: MIT

// Package xfs adapts libxfs to the vhdimap.Filesystem contract.
//
// XFS maintains di_gen precisely so that inode reuse is detectable -- it is
// what NFS file handles rely on -- so identity here is the same shape as ext's:
// an inode number paired with a real reuse counter the filesystem keeps. A file
// carries across two volume states by identity, and a rename is the same
// identity under a different name.
//
// XFS has a log, and libxfs can read it, but the log records block writes
// rather than rename events. That is jbd2's situation and it has jbd2's answer:
// this package does not implement vhdimap.Journal, and renames are reported at
// identity confidence, which is what the evidence supports.
package xfs

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aoiflux/libvhdi/change/internal/readonly"
	"github.com/aoiflux/libvhdi/vhdimap"
	"github.com/aoiflux/libxfs"
)

// Volume is an XFS volume presented as a vhdimap.Filesystem.
type Volume struct {
	v    *libxfs.Volume
	base int64
}

var _ vhdimap.Filesystem = (*Volume)(nil)

// Open reads the XFS volume that begins at base within r.
//
// r is the whole decoded disk and size is its length, so every offset this
// volume reports is disk-absolute, which is what vhdimap.ByteRange requires.
func Open(r io.ReaderAt, base, size int64) (*Volume, error) {
	if r == nil {
		return nil, fmt.Errorf("xfs: nil reader")
	}
	v, err := libxfs.OpenWithOptions(readonly.Wrap(r), libxfs.Options{
		BaseOffset: base,
	})
	if err != nil {
		return nil, fmt.Errorf("xfs: open at %d: %w", base, err)
	}
	return &Volume{v: v, base: base}, nil
}

// Close releases the volume.
func (vol *Volume) Close() error { return vol.v.Close() }

// Volume exposes the underlying libxfs volume.
func (vol *Volume) Volume() *libxfs.Volume { return vol.v }

// Capabilities reports what this volume can answer.
func (vol *Volume) Capabilities() vhdimap.Capabilities {
	c := vol.v.Capabilities()
	return vhdimap.Capabilities{
		Name: "xfs",
		// XFS keeps di_gen for exactly this purpose, so identity is the
		// filesystem's own rather than something synthesised here.
		StableIdentity:  true,
		HasGeneration:   true,
		RecordsCreated:  c.CreationTimes,
		RecordsAccessed: c.AccessTimes,
		RecordsChanged:  c.MetadataChangeTimes,
		HasNamedStreams: false,
		HasJournal:      c.Journaled,
		BaseOffset:      vol.base,
	}
}

// VolumeIdentity returns the superblock UUID, which every v5 metadata checksum
// is computed against and is therefore the canonical identifier of the volume.
func (vol *Volume) VolumeIdentity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sb := vol.v.Superblock()
	return "xfs-uuid:" + strings.ToLower(sb.UUIDString()), nil
}

// WalkFiles calls fn for every entry reachable from the root directory.
//
// The "." and ".." records are skipped, for the reason they are on ext: they
// name directories reported in their own right, and letting them through would
// give every directory a second entry under its own identity.
func (vol *Volume) WalkFiles(ctx context.Context, fn func(vhdimap.FileEntry) error) error {
	err := vol.v.WalkRoot(ctx, func(path string, parentInode uint64, e libxfs.DirectoryEntry) error {
		if e.Name == "." || e.Name == ".." {
			return nil
		}
		return fn(vhdimap.FileEntry{
			ID: vhdimap.FileID{
				Number:     e.InodeNumber,
				Generation: uint64(e.Generation),
			},
			ParentID: vhdimap.FileID{Number: parentInode},
			Name:     e.Name,
			Path:     path,
			// A volume formatted without the ftype feature records no type in
			// its directory entries, so this reads false for every entry
			// there. It qualifies a report rather than driving the diff, which
			// keys on identity.
			IsDir: e.FileType == libxfs.DirEntryFileTypeDirectory,
		})
	})
	if err != nil {
		return fmt.Errorf("xfs: walk: %w", err)
	}
	return nil
}

// FileByID returns one inode by identity.
//
// XFS records a file's name in its parent directory rather than in the inode,
// and libxfs offers no inode-to-path resolver, so the entry returned here
// carries size and timestamps but no name or path. The engine prefers what a
// walk delivered for precisely that reason; this is the fallback for an
// identity a walk did not reach.
func (vol *Volume) FileByID(ctx context.Context, id vhdimap.FileID) (vhdimap.FileEntry, error) {
	if err := ctx.Err(); err != nil {
		return vhdimap.FileEntry{}, err
	}
	ino, err := vol.v.OpenInode(id.Number)
	if err != nil {
		return vhdimap.FileEntry{}, fmt.Errorf("xfs: inode %d: %w", id.Number, err)
	}
	if uint64(ino.Generation) != id.Generation {
		return vhdimap.FileEntry{}, fmt.Errorf("xfs: inode %d has generation %d, want %d: %w",
			id.Number, ino.Generation, id.Generation, vhdimap.ErrIdentityReused)
	}

	return vhdimap.FileEntry{
		ID:    id,
		Size:  int64(ino.Size),
		IsDir: ino.FileMode&modeFormatMask == modeDirectory,
		Times: vhdimap.Times{
			Created:  nanosToTime(ino.CreationTimeNS),
			Modified: nanosToTime(ino.ModificationTimeNS),
			Accessed: nanosToTime(ino.AccessTimeNS),
			Changed:  nanosToTime(ino.InodeChangeTimeNS),
		},
	}, nil
}

// ExtentsForFile returns the disk ranges backing a file's data and attribute
// forks.
//
// The attribute fork is included because extended attributes are part of the
// file: a change confined to one is a change to the file, and an SELinux label
// or a macOS quarantine flag is exactly the kind of change an examiner wants
// reported rather than silently dropped.
func (vol *Volume) ExtentsForFile(ctx context.Context, id vhdimap.FileID) ([]vhdimap.FileExtent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := vol.v.DataRuns(ctx, id.Number)
	if err != nil {
		return nil, fmt.Errorf("xfs: inode %d data runs: %w", id.Number, err)
	}
	out := convert(data)

	// An inode with no attribute fork is ordinary. Losing the data fork's map
	// because of it would be the wrong trade, so the error is tolerated.
	// The attribute fork's runs carry their own offsets, which restart at zero,
	// and are appended rather than rebased onto the data fork, because a rebased
	// offset would name a position in a file that does not exist.
	if attrs, err := vol.v.AttributeRuns(ctx, id.Number); err == nil {
		out = append(out, convert(attrs)...)
	}
	return out, nil
}

func convert(rs []libxfs.ByteRange) []vhdimap.FileExtent {
	out := make([]vhdimap.FileExtent, 0, len(rs))
	for _, r := range rs {
		if r.LengthBytes == 0 {
			continue
		}
		ext := vhdimap.FileExtent{FileOffset: int64(r.FileOffset)}
		// A hole has no location. A range that is neither sparse nor located
		// failed to resolve against the volume's geometry, and libxfs reports
		// both as a zero offset -- treating that as offset zero would attribute
		// the file to the superblock. Both keep their file position and lose
		// their disk range, so the runs after them stay correctly placed.
		if !r.IsSparse && r.StartOffset != 0 {
			ext.ByteRange = vhdimap.ByteRange{
				Offset: int64(r.StartOffset),
				Length: int64(r.LengthBytes),
			}
		}
		out = append(out, ext)
	}
	return out
}

const (
	modeFormatMask = 0xF000
	modeDirectory  = 0x4000
)

func nanosToTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}
