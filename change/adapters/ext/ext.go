// SPDX-License-Identifier: MIT

// Package ext adapts libext to the vhdimap.Filesystem contract, covering
// ext2, ext3 and ext4.
//
// ext pairs an inode number with a generation counter that the kernel bumps
// whenever a slot is reused, which is exactly the identity vhdimap.FileID asks
// for. A file therefore carries across two volume states by identity rather
// than by name, so a rename is visible as the same identity under a different
// name -- not proven by a journal record, but not guessed at either.
//
// # Why this package does not implement vhdimap.Journal
//
// jbd2 is a block journal, not a change journal. It records which filesystem
// blocks were written, with prior copies of them, and libext exposes both. What
// it does not record is an event saying "this file was renamed": a rename on
// ext edits directory blocks, so recovering it would mean parsing old directory
// blocks out of journal copies and diffing their entries. That is a real
// technique and libext exposes the raw material for it, but it reconstructs
// renames rather than reading them, and calling the result journal-proven would
// overstate what happened. Renames here are reported at identity confidence,
// which is what the evidence supports.
package ext

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aoiflux/libext"
	"github.com/aoiflux/libvhdi/change/internal/readonly"
	"github.com/aoiflux/libvhdi/vhdimap"
)

// Volume is an ext volume presented as a vhdimap.Filesystem.
type Volume struct {
	fs   *libext.FS
	base int64
}

var _ vhdimap.Filesystem = (*Volume)(nil)

// Open reads the ext volume that begins at base within r.
//
// r is the whole decoded disk and size is its length. BaseOffset makes every
// offset libext reports disk-absolute, and ImageSize bounds every read at the
// disk's end rather than leaving the library to probe the reader for a size it
// deliberately cannot see through the read-only wrapper.
func Open(r io.ReaderAt, base, size int64) (*Volume, error) {
	if r == nil {
		return nil, fmt.Errorf("ext: nil reader")
	}
	fs, err := libext.OpenWithOptions(readonly.Wrap(r), libext.Options{
		ImageSize:  uint64(size),
		BaseOffset: base,
	})
	if err != nil {
		return nil, fmt.Errorf("ext: open at %d: %w", base, err)
	}
	return &Volume{fs: fs, base: base}, nil
}

// Close releases the volume.
func (vol *Volume) Close() error { return vol.fs.Close() }

// FS exposes the underlying libext filesystem, for a caller that wants
// something this adapter does not surface -- the journal APIs, in particular.
func (vol *Volume) FS() *libext.FS { return vol.fs }

// Capabilities reports what this volume can answer.
func (vol *Volume) Capabilities() vhdimap.Capabilities {
	c := vol.fs.Capabilities()
	return vhdimap.Capabilities{
		Name:            string(vol.fs.Kind()),
		StableIdentity:  c.StableFileIdentity,
		HasGeneration:   c.IdentityReuseCounter,
		RecordsCreated:  c.CreationTimes,
		RecordsAccessed: c.AccessTimes,
		RecordsChanged:  c.MetadataChangeTimes,
		HasNamedStreams: false,
		HasJournal:      c.Journaled,
		BaseOffset:      vol.base,
	}
}

// VolumeIdentity returns the superblock UUID.
func (vol *Volume) VolumeIdentity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sb := vol.fs.Superblock()
	return "ext-uuid:" + formatUUID(sb.UUID), nil
}

// WalkFiles calls fn for every entry reachable from the root directory.
//
// The "." and ".." records are skipped. They name directories reported in their
// own right, and letting them through would make every directory appear twice
// under two identities -- and a self-link's extents are the directory's own, so
// the duplicate would be counted as a second changed file.
func (vol *Volume) WalkFiles(ctx context.Context, fn func(vhdimap.FileEntry) error) error {
	err := vol.fs.WalkDirContext(ctx, libext.RootInode, func(p string, e libext.DirEntry) error {
		if e.Name == "." || e.Name == ".." {
			return nil
		}
		return fn(entryFrom(p, e))
	})
	if err != nil {
		return fmt.Errorf("ext: walk: %w", err)
	}
	return nil
}

// FileByID returns one inode by identity.
//
// ext stores a file's name in its parent's directory block rather than in the
// inode, so a lookup by number alone cannot name the file. The path resolver
// supplies it, and the parent is taken from that path: this costs a second
// lookup and is why the engine prefers what a walk already delivered.
func (vol *Volume) FileByID(ctx context.Context, id vhdimap.FileID) (vhdimap.FileEntry, error) {
	if err := ctx.Err(); err != nil {
		return vhdimap.FileEntry{}, err
	}
	num, err := inodeNumber(id)
	if err != nil {
		return vhdimap.FileEntry{}, err
	}
	ino, err := vol.fs.ReadInode(num)
	if err != nil {
		return vhdimap.FileEntry{}, fmt.Errorf("ext: inode %d: %w", num, err)
	}
	if uint64(ino.Generation) != id.Generation {
		return vhdimap.FileEntry{}, fmt.Errorf("ext: inode %d has generation %d, want %d: %w",
			num, ino.Generation, id.Generation, vhdimap.ErrIdentityReused)
	}

	entry := vhdimap.FileEntry{
		ID:      id,
		Size:    int64(ino.Size),
		IsDir:   ino.IsDirectory,
		Deleted: ino.Deleted(),
		Times: vhdimap.Times{
			Modified: ino.Mtime,
			Accessed: ino.Atime,
			Changed:  ino.Ctime,
		},
	}
	// An inode too small to hold i_crtime has no birth time to report, and a
	// zero one would read as midnight in 1970 rather than as absent.
	if ino.HasCrtime {
		entry.Times.Created = ino.Crtime
	}
	if p, err := vol.fs.PathForContext(ctx, num); err == nil {
		entry.Path = p
		entry.Name = baseName(p)
		if dir := dirName(p); dir != "" {
			if de, err := vol.fs.LookupPath(dir); err == nil {
				entry.ParentID = vhdimap.FileID{
					Number:     uint64(de.Inode),
					Generation: uint64(de.Generation),
				}
			}
		}
	}
	return entry, nil
}

// ExtentsForFile returns the disk ranges backing a file.
//
// A sparse run keeps its file offset and loses its disk range, so the positions
// of the runs after it stay right. Inline data -- a small file stored inside its
// own inode -- yields nothing at all, for the reason resident NTFS content does
// not: the bytes live in the inode table, which every inode in the same block
// shares.
func (vol *Volume) ExtentsForFile(ctx context.Context, id vhdimap.FileID) ([]vhdimap.FileExtent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	num, err := inodeNumber(id)
	if err != nil {
		return nil, err
	}
	runs, err := vol.fs.DataRuns(num)
	if err != nil {
		return nil, fmt.Errorf("ext: inode %d runs: %w", num, err)
	}

	out := make([]vhdimap.FileExtent, 0, len(runs))
	for _, r := range runs {
		if r.Length <= 0 {
			continue
		}
		ext := vhdimap.FileExtent{FileOffset: r.FileOffset}
		if !r.Sparse {
			ext.ByteRange = vhdimap.ByteRange{Offset: r.DiskOffset, Length: r.Length}
		}
		out = append(out, ext)
	}
	return out, nil
}

func entryFrom(path string, e libext.DirEntry) vhdimap.FileEntry {
	entry := vhdimap.FileEntry{
		ID: vhdimap.FileID{
			Number:     uint64(e.Inode),
			Generation: uint64(e.Generation),
		},
		ParentID: vhdimap.FileID{Number: uint64(e.ParentInode)},
		Name:     e.Name,
		Path:     path,
		Size:     int64(e.Size),
		IsDir:    e.IsDirectory,
		Deleted:  e.Deleted,
		Times: vhdimap.Times{
			Created:  e.Times.Crtime,
			Modified: e.Times.Mtime,
			Accessed: e.Times.Atime,
			Changed:  e.Times.Ctime,
		},
	}
	return entry
}

func inodeNumber(id vhdimap.FileID) (uint32, error) {
	if id.Number == 0 || id.Number > 0xFFFFFFFF {
		return 0, fmt.Errorf("ext: %d is not an inode number", id.Number)
	}
	return uint32(id.Number), nil
}

func formatUUID(u [16]byte) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	for i, c := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			b.WriteByte('-')
		}
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0F])
	}
	return b.String()
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func dirName(p string) string {
	p = strings.TrimRight(p, "/")
	i := strings.LastIndexByte(p, '/')
	switch {
	case i < 0:
		return ""
	case i == 0:
		return "/"
	default:
		return p[:i]
	}
}
