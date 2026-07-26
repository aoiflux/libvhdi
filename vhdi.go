// SPDX-License-Identifier: MIT

// Package libvhdi provides a pure-Go implementation of the VHD and VHDX virtual disk format support.
//
// This package implements read support for the VHD (Virtual Hard Disk) and VHDX
// (Virtual Hard Disk v2) formats, written against the Microsoft VHD Image Format
// Specification and the VHDX Format Specification. It is an independent
// implementation and shares no code with any C library.
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
	"io/fs"

	"github.com/aoiflux/libvhdi/reader"
	"github.com/aoiflux/libvhdi/types"
)

// Version information
const (
	// Version is the current library version
	Version = "0.6.0"

	// Author information
	Author = "aoiflux"
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

// Open opens a VHD or VHDX image from r, detecting the format automatically and
// deriving the image size from the reader when possible.
//
// Readers exposing Size, Stat or Seek are all handled, covering *os.File,
// *bytes.Reader, *strings.Reader, io.SectionReader and fs.File. A bare
// io.ReaderAt with none of those must set Options.Size for VHD images; VHDX
// locates every structure from fixed offsets and needs no size.
//
// When opts.ParentResolver is set, differencing parent chains are resolved
// automatically, so the caller gets a correct contiguous device with no manual
// SetParent wiring. opts may be nil.
func Open(r io.ReaderAt, opts *Options) (*Disk, error) {
	return reader.Open(r, opts)
}

// OpenFileWith opens a VHD or VHDX file by path with explicit options.
//
// When opts.ParentResolver is nil it defaults to searching the image's own
// directory, which is where a differencing chain's parents conventionally live.
func OpenFileWith(path string, opts *Options) (*Disk, error) {
	return reader.OpenFileWith(path, opts)
}

// Options controls how a disk is opened. The zero value is safe: parent GUIDs
// are verified, and differencing disks with no parent fail closed on read.
type Options = reader.Options

// Parent resolution types. A ParentResolver locates the parent image of a
// differencing disk; DirParentResolver and FSParentResolver cover the common
// cases.
type (
	ParentResolver     = reader.ParentResolver
	ParentResolverFunc = reader.ParentResolverFunc
	ParentRequest      = reader.ParentRequest
	ParentSource       = reader.ParentSource
)

// DirParentResolver returns a ParentResolver that searches the child's own
// directory first, then each supplied directory.
func DirParentResolver(dirs ...string) ParentResolver {
	return reader.DirParentResolver(dirs...)
}

// FSParentResolver returns a ParentResolver that searches an fs.FS.
func FSParentResolver(fsys fs.FS) ParentResolver {
	return reader.FSParentResolver(fsys)
}

// DefaultMaxChainDepth bounds how many disks a differencing chain may contain.
const DefaultMaxChainDepth = reader.DefaultMaxChainDepth

// Extent describes a contiguous run of the virtual disk and what backs it, so a
// caller can read only what is mapped and skip sparse regions. For a
// differencing chain each mapped extent also identifies which disk supplies it.
type Extent = reader.Extent

// ExtentKind says what backs an Extent.
type ExtentKind = reader.ExtentKind

// Extent kinds.
const (
	// ExtentZero has no backing bytes anywhere in the chain and reads as zeroes.
	ExtentZero = reader.ExtentZero

	// ExtentMapped is backed by a contiguous run of bytes in one of the files.
	ExtentMapped = reader.ExtentMapped

	// ExtentUnresolved resolves to a parent that is not attached.
	ExtentUnresolved = reader.ExtentUnresolved
)

// LogReplayStats describes what a VHDX log replay applied.
type LogReplayStats = reader.LogReplayStats

// ChainEntry describes one disk in a differencing chain: the set of files that
// together constitute the device, which is what an evidence record must name.
type ChainEntry = reader.ChainEntry

// Errors reported when opening or reading a disk.
var (
	// ErrParentRequired is returned when a read on a differencing disk resolves
	// to a parent that has not been attached. Differencing disks fail closed:
	// silently returning zeroes would be indistinguishable from genuine disk
	// contents.
	ErrParentRequired = reader.ErrParentRequired

	// ErrParentNotFound is returned when a resolver exhausted its search
	// without locating the parent image.
	ErrParentNotFound = reader.ErrParentNotFound

	// ErrParentMismatch is returned when a located image does not match the
	// parent identity recorded in the child.
	ErrParentMismatch = reader.ErrParentMismatch

	// ErrChainTooDeep is returned when a parent chain exceeds the configured
	// maximum depth.
	ErrChainTooDeep = reader.ErrChainTooDeep

	// ErrChainCycle is returned when a parent chain refers back to a disk
	// already in the chain.
	ErrChainCycle = reader.ErrChainCycle

	// ErrSizeUnknown is returned when an image's size can neither be derived
	// from the reader nor was supplied via Options.Size.
	ErrSizeUnknown = reader.ErrSizeUnknown

	// ErrCorruptImage is returned when an image's headers are inconsistent or
	// describe structures that do not fit the file.
	ErrCorruptImage = reader.ErrCorruptImage

	// ErrDirtyImage is returned when a VHDX log cannot be replayed, so the
	// image may not reflect its last committed state. Set
	// Options.AllowDirtyImage to read it anyway.
	ErrDirtyImage = reader.ErrDirtyImage
)
