// Package libvhdi provides a pure-Go implementation of the VHD and VHDX virtual disk format support.
//
// This package implements full support for reading VHD (Virtual Hard Disk) and VHDX
// (Virtual Hard Disk v2) formats with feature parity to the libvhdi C library.
//
// Supported features:
//   - VHD fixed and dynamic disk parsing
//   - VHDX disk parsing with metadata
//   - Block Allocation Table (BAT) lookup and resolution
//   - Differencing (child) disk support
//   - Checksum verification
//   - Multi-sector reading
//
// Example usage:
//
//	disk, err := libvhdi.OpenFile("disk.vhd")
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer disk.Close()
//
//	buf := make([]byte, 512)
//	n, err := disk.ReadAt(buf, 0)
//	if err != nil {
//		log.Fatal(err)
//	}
package libvhdi

import (
	"io"

	"github.com/aoiflux/libvhdi/reader"
	"github.com/aoiflux/libvhdi/types"
)

// Version information
const (
	// Version is the current library version
	Version = "0.1.0"

	// Author information
	Author = "libewf contributors"
)

// Disk represents an open VHD or VHDX virtual disk.
// Implements io.ReaderAt.
type Disk = reader.VirtualDisk

// FileFormat constants (re-exported for convenience).
const (
	FormatUnknown = types.FileFormatUnknown
	FormatVHD     = types.FileFormatVHD
	FormatVHDX    = types.FileFormatVHDX
)

// DiskType constants (re-exported for convenience).
const (
	DiskTypeFixed        = types.DiskTypeFixed
	DiskTypeDynamic      = types.DiskTypeDynamic
	DiskTypeDifferential = types.DiskTypeDifferential
)

// OpenFile opens a VHD or VHDX virtual disk file by path.
// Format detection is automatic. The caller must call disk.Close() when done.
func OpenFile(path string) (*Disk, error) {
	return reader.OpenFile(path)
}

// OpenVHD opens a VHD disk from an io.ReaderAt of the given total file size.
func OpenVHD(r io.ReaderAt, fileSize int64) (*Disk, error) {
	return reader.OpenVHD(r, fileSize)
}

// OpenVHDX opens a VHDX disk from an io.ReaderAt of the given total file size.
func OpenVHDX(r io.ReaderAt, fileSize int64) (*Disk, error) {
	return reader.OpenVHDX(r, fileSize)
}
