// SPDX-License-Identifier: MIT

// Package libvhdi provides a pure-Go implementation of the VHD and VHDX virtual disk format support.
//
// This package implements read support for the VHD (Virtual Hard Disk) and VHDX
// (Virtual Hard Disk v2) formats, written against the Microsoft VHD Image Format
// Specification and the VHDX Format Specification. It is an independent
// implementation and shares no code with any C library.
//
// Supported features:
//   - VHD fixed, dynamic and differencing disks
//   - VHDX fixed, dynamic and differencing disks
//   - Sector-granular differencing resolution across a parent chain
//   - Automatic parent chain resolution, verified against each child's metadata
//   - VHDX log replay into a read-only in-memory overlay
//   - Sparse-aware extent mapping with per-link provenance
//   - Per-format checksum verification
//
// Reads are read-only throughout: no code path writes to an image, and log
// replay is applied to an overlay so a forensic copy stays byte-identical.
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
	"context"
	"io"
	"io/fs"

	"github.com/aoiflux/libvhdi/internal/buildinfo"
	"github.com/aoiflux/libvhdi/reader"
	"github.com/aoiflux/libvhdi/types"
)

// ModulePath is this library's Go module path. It is the key under which the
// module appears in a consuming binary's build information.
const ModulePath = buildinfo.ModulePath

// Author identifies the project the library belongs to.
const Author = "aoiflux"

// Version reports the libvhdi module version recorded in the calling binary's
// build information, for example "v0.3.0".
//
// The value is read from the metadata the Go toolchain embeds at link time
// rather than from a constant maintained by hand, so it cannot drift away from
// the tag consumers actually resolve. A hand-maintained constant did drift
// exactly that way before v0.3.0, which is why this is a function.
//
// Three results are possible:
//
//   - The module version, when libvhdi is a dependency of the running binary.
//   - "(devel)", when libvhdi is the main module, which is the case while
//     running its own tests and examples.
//   - "unknown", when build information is unavailable at all. This happens in
//     binaries built without module information, and it is the reason the
//     result is a string rather than a parsed version: a forensic report must
//     be able to record "the producing tool could not name its own version"
//     without the report generator failing.
//
// The returned value is suitable for recording in a report's provenance. It is
// not suitable for a version comparison without parsing, and callers should not
// attempt one against "(devel)" or "unknown".
func Version() string {
	return buildinfo.Version()
}

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

// NoParentResolution suppresses automatic parent chain resolution, so a caller
// can attach parents by hand with SetParent.
//
// It exists so that a nil Options.ParentResolver can mean "the default" without
// leaving a caller who genuinely wants no resolution unable to say so.
var NoParentResolution = reader.NoParentResolution

// DirParentResolver returns a ParentResolver that searches the child's own
// directory first, then each supplied directory.
func DirParentResolver(dirs ...string) ParentResolver {
	return reader.DirParentResolver(dirs...)
}

// FSParentResolver returns a ParentResolver that searches an fs.FS.
func FSParentResolver(fsys fs.FS) ParentResolver {
	return reader.FSParentResolver(fsys)
}

// GUIDString renders a 16-byte GUID in the canonical RFC 4122 form,
// "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx".
//
// This is not a plain hex dump. Both formats store the first three fields
// little-endian and the rest big-endian, so printing the bytes in order
// produces a string that looks right and names a different GUID -- which is why
// this is exported rather than left for each caller to re-derive.
func GUIDString(guid [16]byte) string {
	return reader.GUIDString(guid)
}

// ParseGUID parses a GUID in the canonical form GUIDString produces. It is the
// inverse of GUIDString, so a value can survive a round trip through a report.
func ParseGUID(s string) ([16]byte, error) {
	return reader.ParseGUID(s)
}

// StructuralError says what was being parsed, where in the image, and what was
// wrong with it, so an error becomes a starting point for examining the file by
// hand rather than only a statement that something failed.
//
// It wraps ErrCorruptImage or ErrUnsupportedFeature, so errors.Is still answers
// the coarse question.
type StructuralError = reader.StructuralError

// Geometry describes the physical shape an image reports for itself: its
// virtual size, block and sector sizes, and, for VHD, its CHS geometry.
type Geometry = reader.Geometry

// Provenance describes what produced an image, when, and how much of the read
// can be trusted. It is what makes a forensic report self-describing.
type Provenance = reader.Provenance

// DefaultMaxChainDepth bounds how many disks a differencing chain may contain.
const DefaultMaxChainDepth = reader.DefaultMaxChainDepth

// ============================================================================
// Checkpoints
// ============================================================================

// DiscoverChain scans a directory of images and builds the parent-to-children
// tree they form, reading only headers.
//
// Chain walks upward from one image to its ancestors, which is the wrong
// direction for Hyper-V checkpoints: checkpoints branch, and from a leaf a
// sibling branch is invisible. This sees the whole shape.
func DiscoverChain(ctx context.Context, dir string, opts *DiscoverOptions) (*Tree, error) {
	return reader.DiscoverChain(ctx, dir, opts)
}

// ProbeFile reads an image's headers without opening it, which is what makes
// surveying a directory of terabyte-scale images cheap.
func ProbeFile(path string) (DiskInfo, error) { return reader.ProbeFile(path) }

// Probe reads an image's headers from r without opening it.
func Probe(r io.ReaderAt, fileSize int64) (DiskInfo, error) { return reader.Probe(r, fileSize) }

// Checkpoint discovery types.
type (
	// Tree is the parent-to-children graph of a set of images.
	Tree = reader.Tree

	// Node is one image in a chain tree, readable or not.
	Node = reader.Node

	// Lineage is one path through a tree, root first: the set of files that
	// together constitute one device.
	Lineage = reader.Lineage

	// DiskInfo is what a header-only probe can learn about an image.
	DiskInfo = reader.DiskInfo

	// DiscoverOptions controls a directory scan. The zero value is usable.
	DiscoverOptions = reader.DiscoverOptions

	// Role classifies an image's part in a chain.
	Role = reader.Role
)

// Image roles within a chain.
const (
	// RoleUnknown means the role could not be determined, which is the case for
	// an image that could not be read.
	RoleUnknown = reader.RoleUnknown

	// RoleBase is the disk a chain starts from.
	RoleBase = reader.RoleBase

	// RoleCheckpoint is a differencing image holding the writes made since the
	// checkpoint below it was taken.
	RoleCheckpoint = reader.RoleCheckpoint
)

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

	// ExtentZeroedByChild was explicitly cleared by a differencing disk,
	// overriding whatever its parent held there. It reads as zeroes like
	// ExtentZero, but it is a write rather than an absence -- which is how a
	// deletion shows up at block level. VHDX only; VHD cannot express it.
	ExtentZeroedByChild = reader.ExtentZeroedByChild
)

// FooterSource identifies which copy of the VHD footer an image was opened
// from, so a report can distinguish an intact read from a recovered one.
type FooterSource = reader.FooterSource

// VHD footer copies, in the order the open path tries them.
const (
	// FooterSourceUnknown means no footer was read, which is the case for every
	// VHDX image.
	FooterSourceUnknown = reader.FooterSourceUnknown

	// FooterSourceTrailing is the conformant 512-byte footer in the final sector.
	FooterSourceTrailing = reader.FooterSourceTrailing

	// FooterSourceTrailingLegacy is the 511-byte footer written by Microsoft
	// Virtual PC before the format was documented.
	FooterSourceTrailingLegacy = reader.FooterSourceTrailingLegacy

	// FooterSourceMirror is the copy at offset 0, which dynamic and differencing
	// disks carry so a damaged tail stays recoverable.
	FooterSourceMirror = reader.FooterSourceMirror
)

// Warning records something suspicious about an image or its parent chain that
// does not prevent it from being read, such as having been recovered from a
// fallback footer or having accepted a parent whose identity could not be
// verified.
type Warning = reader.Warning

// WarningKind names a class of warning.
type WarningKind = reader.WarningKind

// Warning kinds.
const (
	// WarningFooterRecovered means a VHD was opened from a fallback footer copy,
	// so the image is damaged even though it opened.
	WarningFooterRecovered = reader.WarningFooterRecovered

	// WarningParentTimestampMismatch means a differencing child records a
	// different modification time for its parent than the file found carries,
	// which can mean the parent has been written to since the child was made.
	WarningParentTimestampMismatch = reader.WarningParentTimestampMismatch

	// WarningParentIdentityUnverifiable means the child records no parent
	// identifier, so its parent was matched on virtual size alone.
	WarningParentIdentityUnverifiable = reader.WarningParentIdentityUnverifiable

	// WarningParentIdentityUnchecked means identity verification was disabled by
	// Options.AllowParentGUIDMismatch.
	WarningParentIdentityUnchecked = reader.WarningParentIdentityUnchecked
)

// Run is one contiguous piece of a device as Disk.Stream emits it: an extent
// and, for a mapped extent, its bytes.
type Run = reader.Run

// StreamOptions controls a Disk.Stream walk. The zero value is usable.
type StreamOptions = reader.StreamOptions

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

	// ErrCorruptImage is returned when an image's headers are inconsistent,
	// fail a checksum, or describe structures that do not fit the file.
	ErrCorruptImage = reader.ErrCorruptImage

	// ErrUnsupportedFeature is returned when an image is well-formed but uses
	// something this library does not implement.
	//
	// It is deliberately distinct from ErrCorruptImage. One says the file is
	// broken; the other says to try a different tool. Conflating them sends an
	// examiner looking for damage that is not there.
	ErrUnsupportedFeature = reader.ErrUnsupportedFeature

	// ErrDirtyImage is returned when a VHDX log cannot be replayed, so the
	// image may not reflect its last committed state. Set
	// Options.AllowDirtyImage to read it anyway.
	ErrDirtyImage = reader.ErrDirtyImage

	// ErrSplitImage is returned for a file that looks like one piece of a split
	// or segmented disk.
	//
	// Neither specification defines such a layout: a multi-file disk in VHD or
	// VHDX is always a differencing chain, and split images are a VMDK and VDI
	// concept. Such a file is a different format, not a VHD this library is
	// failing to read.
	ErrSplitImage = reader.ErrSplitImage

	// ErrNotFound is returned when a disk named by identity or path is not in a
	// chain tree.
	ErrNotFound = reader.ErrNotFound
)
