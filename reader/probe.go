// SPDX-License-Identifier: MIT

package reader

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoiflux/libvhdi/metadata"
	"github.com/aoiflux/libvhdi/types"
)

// DiskInfo is what a header-only probe can learn about an image.
//
// It exists so that a directory of images can be surveyed without opening any
// of them. A full open parses the block allocation table, which for a large
// disk is megabytes of read and allocation per image -- and discovery needs
// none of it. Everything here comes from structures at fixed offsets near the
// start and end of the file.
type DiskInfo struct {
	// Path is the file this was read from.
	Path string

	// Format and DiskType are what the image says it is.
	Format   types.FileFormat
	DiskType types.DiskType

	// Identifier is the image's own GUID.
	Identifier [16]byte

	// LinkIdentity is the GUID a differencing child of this image would record
	// to name it. It is the image's own identifier for VHD and its data-write
	// identifier for VHDX, which are different fields -- conflating them is what
	// makes a chain fail to link up.
	LinkIdentity [16]byte

	// ParentIdentifier is the GUID this image records for its parent, zero when
	// it has none.
	ParentIdentifier [16]byte

	// ParentFilename is the parent path recorded in this image, and
	// ParentLocators every path candidate it lists.
	ParentFilename string
	ParentLocators []types.ParentLocatorEntry

	// VirtualSize is the device size this image presents.
	VirtualSize uint64

	// FileSize is the size of the file on disk.
	FileSize int64

	// HasLog reports a VHDX carrying journalled writes. Discovery does not
	// replay the log, so a true value here means the image's structures may be
	// stale and a full open is needed to say more.
	HasLog bool

	// FooterSource says which copy of the VHD footer the probe read.
	FooterSource FooterSource
}

// IsDifferencing reports whether this image depends on a parent.
func (i DiskInfo) IsDifferencing() bool {
	return i.DiskType == types.DiskTypeDifferential
}

// Role classifies an image's part in a Hyper-V chain.
type Role uint8

const (
	// RoleUnknown means the image's role could not be determined.
	RoleUnknown Role = iota

	// RoleBase is the disk a chain starts from: a fixed or dynamic image with
	// no parent.
	RoleBase

	// RoleCheckpoint is a differencing image holding the writes made since the
	// checkpoint below it was taken.
	RoleCheckpoint
)

// String names the role.
func (r Role) String() string {
	switch r {
	case RoleBase:
		return "base"
	case RoleCheckpoint:
		return "checkpoint"
	default:
		return "unknown"
	}
}

// Role classifies this image.
//
// Hyper-V names a checkpoint's differencing disk with an .avhdx or .avhd
// extension, but the extension is a convention rather than a format: the file
// is an ordinary differencing image and a renamed one is still a checkpoint.
// The disk type is therefore what decides, and the extension is only a
// corroborating signal.
func (i DiskInfo) Role() Role {
	if i.IsDifferencing() {
		return RoleCheckpoint
	}
	if i.Format == types.FileFormatUnknown {
		return RoleUnknown
	}
	return RoleBase
}

// HasCheckpointExtension reports whether the file uses Hyper-V's naming
// convention for a checkpoint disk.
//
// A differencing image without this extension is still a checkpoint, and an
// image with it that is not differencing is a file that has been renamed or
// converted. Either disagreement is worth reporting rather than resolving.
func (i DiskInfo) HasCheckpointExtension() bool {
	switch strings.ToLower(filepath.Ext(i.Path)) {
	case ".avhdx", ".avhd":
		return true
	}
	return false
}

// ProbeFile reads an image's headers without opening it.
//
// This is what makes scanning a directory of large images cheap: no block
// allocation table is read, nothing is allocated in proportion to the disk's
// size, and no log is replayed. It is enough to build a chain tree and no more.
func ProbeFile(path string) (DiskInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return DiskInfo{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return DiskInfo{}, err
	}

	probed, err := Probe(f, info.Size())
	if err != nil {
		return DiskInfo{}, err
	}
	probed.Path = path
	return probed, nil
}

// Probe reads an image's headers from r without opening it.
func Probe(r io.ReaderAt, fileSize int64) (DiskInfo, error) {
	format, err := detectFormat(r)
	if err != nil {
		return DiskInfo{}, err
	}

	if format == types.FileFormatVHDX {
		return probeVHDX(r, fileSize)
	}
	return probeVHD(r, fileSize)
}

// probeVHD reads a VHD footer, and the dynamic disk header when there is one.
func probeVHD(r io.ReaderAt, fileSize int64) (DiskInfo, error) {
	fp := NewVHDFooterParser(r)
	footer, source, _, err := fp.readFooterWithRecovery(fileSize)
	if err != nil {
		return DiskInfo{}, err
	}

	info := DiskInfo{
		Format:       types.FileFormatVHD,
		DiskType:     footer.DiskType,
		Identifier:   footer.Identifier,
		LinkIdentity: footer.Identifier,
		VirtualSize:  footer.MediaSize,
		FileSize:     fileSize,
		FooterSource: source,
	}

	if footer.DiskType != types.DiskTypeDynamic && footer.DiskType != types.DiskTypeDifferential {
		return info, nil
	}

	dhp := NewVHDDynamicDiskHeaderParser(r)
	header, err := dhp.ReadHeaderAt(footer.NextOffset)
	if err != nil {
		return DiskInfo{}, err
	}

	info.ParentIdentifier = header.ParentIdentifier
	info.ParentFilename = header.ParentFilename
	info.ParentLocators = header.ParentLocators
	return info, nil
}

// probeVHDX reads a VHDX header and its metadata, stopping short of the BAT.
//
// The log is deliberately not replayed. Replay is the expensive part of opening
// a VHDX and it cannot change any field this returns: the headers sit outside
// the log's reach, and a chain's shape is not something a journalled write
// alters. HasLog records that the image has one so a caller can decide whether
// to open it properly.
func probeVHDX(r io.ReaderAt, fileSize int64) (DiskInfo, error) {
	ihp := NewVHDXImageHeaderParser(r)
	imgHeader, err := ihp.ReadImageHeader()
	if err != nil {
		return DiskInfo{}, err
	}

	info := DiskInfo{
		Format:       types.FileFormatVHDX,
		LinkIdentity: imgHeader.DataWriteIdentifier,
		FileSize:     fileSize,
		HasLog:       imgHeader.LogIdentifier != ([16]byte{}),
	}

	rtp := NewVHDXRegionTableParser(r)
	var metaOffset int64
	var metaSize uint32

	for _, tableOffset := range []int64{types.VHDXFirstRegionTableOffset, types.VHDXSecondRegionTableOffset} {
		regions, err := rtp.ReadRegionTableAt(tableOffset)
		if err != nil {
			continue
		}
		if err := validateVHDXRegionTable(regions, fileSize); err != nil {
			continue
		}
		for _, region := range regions {
			if region.TypeIdentifier == types.RegionTypeMetadata {
				metaOffset = region.DataOffset
				metaSize = region.DataSize
			}
		}
		if metaOffset != 0 {
			break
		}
	}

	if metaOffset == 0 {
		return DiskInfo{}, types.Corruptf("probe VHDX", types.FileFormatVHDX, types.NoOffset,
			"Metadata Region", "no metadata region in either region table")
	}

	vals, err := metadata.NewParser(r, metaOffset, metaSize).Parse()
	if err != nil {
		return DiskInfo{}, err
	}

	info.DiskType = vals.DiskType
	info.Identifier = vals.VirtualDiskIdentifier
	info.ParentIdentifier = vals.ParentIdentifier
	info.ParentFilename = vals.ParentFilename
	info.ParentLocators = vals.ParentLocators
	info.VirtualSize = vals.VirtualDiskSize
	return info, nil
}
