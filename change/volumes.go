// SPDX-License-Identifier: MIT

package change

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aoiflux/libtable"
	"github.com/aoiflux/libvhdi/change/adapters/exfat"
	"github.com/aoiflux/libvhdi/change/adapters/ext"
	"github.com/aoiflux/libvhdi/change/adapters/fat"
	"github.com/aoiflux/libvhdi/change/adapters/hfs"
	"github.com/aoiflux/libvhdi/change/adapters/ntfs"
	"github.com/aoiflux/libvhdi/change/adapters/xfs"
	"github.com/aoiflux/libvhdi/vhdimap"
)

// ErrUnknownFilesystem is returned when nothing at a given offset is
// recognised.
//
// It is deliberately distinct from a parse failure. One says no adapter here
// knows this format, which is a reason to reach for another tool; the other
// says the structure is damaged, which is a reason to look at the image. An
// examiner sent looking for damage that is not there has been misled.
var ErrUnknownFilesystem = errors.New("change: no supported filesystem found")

// Volume is one filesystem located on a disk.
type Volume struct {
	// Filesystem is the volume, ready to be compared.
	Filesystem vhdimap.Filesystem

	// Base is the volume's byte offset within the disk, and Length its extent.
	// Length is zero for a volume found outside a partition table.
	Base   int64
	Length int64

	// PartitionIndex and PartitionName describe the partition the volume was
	// found in. Index is -1 when there was no partition table.
	PartitionIndex int
	PartitionName  string

	closer func() error
}

// Close releases the volume.
func (v *Volume) Close() error {
	if v == nil || v.closer == nil {
		return nil
	}
	return v.closer()
}

// Span reports the volume's range within the disk, for intersecting a disk's
// changed ranges with the volume that holds them.
//
// A volume with no recorded length spans from its base to the end of the disk,
// since nothing says otherwise and assuming a shorter span would silently drop
// changed ranges past the guess.
func (v *Volume) Span(diskSize int64) vhdimap.ByteRange {
	length := v.Length
	if length <= 0 {
		length = diskSize - v.Base
	}
	if length < 0 {
		length = 0
	}
	return vhdimap.ByteRange{Offset: v.Base, Length: length}
}

// OpenVolumes finds every filesystem on a disk and opens the ones it can read.
//
// r is the whole decoded disk and size its length. Partitions are read first;
// a disk with no partition table is then tried at offset zero, which is how a
// formatted virtual disk attached as a data volume is usually laid out.
//
// Partitions that hold nothing recognisable are reported in errs rather than
// failing the call: one unsupported filesystem on a disk must not cost the
// analysis of the others, and an examiner needs to be told which volume was
// skipped rather than left to notice its absence.
func OpenVolumes(ctx context.Context, r io.ReaderAt, size int64) (vols []*Volume, errs []error) {
	if r == nil {
		return nil, []error{fmt.Errorf("change: nil reader")}
	}

	table, err := libtable.Parse(r, uint64(size), libtable.Options{})
	if err != nil || table == nil || len(table.Partitions) == 0 {
		// No usable table. The disk may still be formatted end to end.
		v, verr := OpenVolumeAt(ctx, r, 0, size, size)
		if verr != nil {
			if err != nil {
				return nil, []error{fmt.Errorf("change: no partition table (%v) and no filesystem at offset 0: %w", err, verr)}
			}
			return nil, []error{verr}
		}
		v.PartitionIndex = -1
		return []*Volume{v}, nil
	}

	blockSize := int64(table.BlockSize)
	if blockSize <= 0 {
		blockSize = 512
	}

	for _, p := range table.Partitions {
		if err := ctx.Err(); err != nil {
			return vols, append(errs, err)
		}

		base := int64(p.StartLBA) * blockSize
		length := int64(p.LengthLBA) * blockSize
		if base <= 0 || length <= 0 || base >= size {
			// A zero-start or zero-length entry is a table artefact -- a GPT
			// header pseudo-entry, an unused slot -- and not a volume.
			continue
		}

		v, err := OpenVolumeAt(ctx, r, base, length, size)
		if err != nil {
			errs = append(errs, fmt.Errorf("partition %d (%s) at %d: %w", p.Index, p.TypeName, base, err))
			continue
		}
		v.PartitionIndex = p.Index
		v.PartitionName = partitionName(p)
		vols = append(vols, v)
	}

	return vols, errs
}

// OpenVolumeAt opens the filesystem that begins at base within r.
//
// length is the volume's extent and may be zero when it is unknown; size is the
// whole disk's length, which bounds every read. The returned volume reports
// disk-absolute offsets, so its extents can be intersected with libvhdi's
// changed ranges without any adjustment.
func OpenVolumeAt(ctx context.Context, r io.ReaderAt, base, length, size int64) (*Volume, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	kind, err := detect(r, base)
	if err != nil {
		return nil, err
	}

	v := &Volume{Base: base, Length: length, PartitionIndex: -1}
	switch kind {
	case fsNTFS:
		fs, err := ntfs.Open(r, base, size)
		if err != nil {
			return nil, err
		}
		v.Filesystem, v.closer = fs, fs.Close
	case fsExt:
		fs, err := ext.Open(r, base, size)
		if err != nil {
			return nil, err
		}
		v.Filesystem, v.closer = fs, fs.Close
	case fsFAT:
		fs, err := fat.Open(r, base, size)
		if err != nil {
			return nil, err
		}
		v.Filesystem, v.closer = fs, fs.Close
	case fsExFAT:
		fs, err := exfat.Open(r, base, size)
		if err != nil {
			return nil, err
		}
		v.Filesystem, v.closer = fs, fs.Close
	case fsHFS:
		fs, err := hfs.Open(r, base, size)
		if err != nil {
			return nil, err
		}
		v.Filesystem, v.closer = fs, fs.Close
	case fsXFS:
		fs, err := xfs.Open(r, base, size)
		if err != nil {
			return nil, err
		}
		v.Filesystem, v.closer = fs, fs.Close
	default:
		return nil, ErrUnknownFilesystem
	}
	return v, nil
}

type fsKind int

const (
	fsUnknown fsKind = iota
	fsNTFS
	fsExt
	fsFAT
	fsExFAT
	fsHFS
	fsXFS
)

// detect identifies the filesystem at base from its on-disk signatures.
//
// Signatures are checked rather than each adapter being tried in turn. Opening
// six parsers against an unknown volume to see which one does not error is slow
// and, worse, unsafe: a parser handed a structure it does not understand can
// succeed on plausible-looking rubbish, and the first one to do so wins.
func detect(r io.ReaderAt, base int64) (fsKind, error) {
	// The first two kibibytes cover every signature checked here: the boot
	// sector for the FAT family and NTFS, and the 1024-byte superblock offset
	// that ext and HFS+ both use.
	buf := make([]byte, 2048)
	n, err := r.ReadAt(buf, base)
	if n < 1024 {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return fsUnknown, fmt.Errorf("change: reading signatures at %d: %w", base, err)
	}
	buf = buf[:n]

	switch {
	case hasAt(buf, 3, "NTFS    "):
		return fsNTFS, nil
	case hasAt(buf, 3, "EXFAT   "):
		return fsExFAT, nil
	case hasAt(buf, 0, "XFSB"):
		return fsXFS, nil
	case hasAt(buf, 82, "FAT32   "),
		hasAt(buf, 54, "FAT12   "),
		hasAt(buf, 54, "FAT16   "),
		hasAt(buf, 54, "FAT     "):
		return fsFAT, nil
	}

	if len(buf) >= 1026 {
		// HFS+ and HFSX put their signature at the top of the volume header,
		// which sits 1024 bytes in. "BD" is a classic HFS volume, which may be
		// a wrapper around an embedded HFS+ one -- libhfs resolves that itself.
		switch string(buf[1024:1026]) {
		case "H+", "HX", "BD":
			return fsHFS, nil
		}
	}
	if len(buf) >= 1082 {
		// s_magic sits 56 bytes into the ext superblock, little-endian 0xEF53.
		if buf[1080] == 0x53 && buf[1081] == 0xEF {
			return fsExt, nil
		}
	}

	return fsUnknown, ErrUnknownFilesystem
}

func hasAt(buf []byte, off int, want string) bool {
	if off+len(want) > len(buf) {
		return false
	}
	return string(buf[off:off+len(want)]) == want
}

func partitionName(p libtable.Partition) string {
	if p.Name != "" {
		return p.Name
	}
	return p.TypeName
}
