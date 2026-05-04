// Package types defines all data structures, enums, and constants for the libvhdi library.
package types

import (
	"time"
)

// ============================================================================
// ENUMS & TYPE CONSTANTS
// ============================================================================

// FileFormat represents the format of the virtual disk file.
type FileFormat int

const (
	FileFormatUnknown FileFormat = iota
	FileFormatVHD
	FileFormatVHDX
)

// DiskType represents the type of virtual disk.
type DiskType uint32

const (
	DiskTypeFixed        DiskType = 0x00000002
	DiskTypeDynamic      DiskType = 0x00000003
	DiskTypeDifferential DiskType = 0x00000004
)

// AccessFlag represents file access mode.
type AccessFlag int

const (
	AccessFlagRead  AccessFlag = 0x01
	AccessFlagWrite AccessFlag = 0x02
)

// BlockState represents the allocation state of a block (VHDX).
type BlockState uint8

const (
	BlockStateNone               BlockState = 0 // PAYLOAD_BLOCK_NOT_PRESENT
	BlockStateUndefined          BlockState = 1 // PAYLOAD_BLOCK_UNDEFINED
	BlockStateZero               BlockState = 2 // PAYLOAD_BLOCK_ZERO
	BlockStateUnmapped           BlockState = 3 // PAYLOAD_BLOCK_UNMAPPED
	BlockStateFullyAllocated     BlockState = 6 // PAYLOAD_BLOCK_FULLY_PRESENT
	BlockStatePartiallyAllocated BlockState = 7 // PAYLOAD_BLOCK_PARTIALLY_PRESENT
)

// SectorRangeFlag represents flags for sector ranges.
type SectorRangeFlag uint32

const (
	SectorRangeFlagUnallocated SectorRangeFlag = 0x00000001
)

// ============================================================================
// MAGIC VALUES & SIGNATURES
// ============================================================================

const (
	// VHD signatures
	VHDFooterSignature        = "conectix"
	VHDDynamicDiskSignature   = "cxsparse"
	VHDDynamicHeaderSignature = "cxsparse"

	// VHDX signatures
	VHDXFileSignature     = "vhdxfile"
	VHDXHeaderSignature   = "head"
	VHDXRegionSignature   = "regi"
	VHDXMetadataSignature = "metadata"

	// Unallocated block markers
	VHDUnallocatedBlockMarker  uint32 = 0xFFFFFFFF
	VHDXUnallocatedBlockMarker uint64 = 0xFFFFFFFFFFFFFFFF
)

// ============================================================================
// FILE SIZES & OFFSETS
// ============================================================================

const (
	VHDFooterSize        = 512
	VHDDynamicHeaderSize = 1024
	VHDXHeaderSize       = 4096
	VHDXFileInfoSize     = 520
	VHDXRegionTableSize  = 64 * 1024 // 64 KB
	VHDXMetadataSize     = 64 * 1024 // 64 KB

	// Standard VHD offsets
	VHDFooterOffset = -512 // From end of file

	// Standard VHDX offsets (in bytes)
	VHDXFileInfoOffset          = 0
	VHDXFirstHeaderOffset       = 64 * 1024  // 64 KB
	VHDXSecondHeaderOffset      = 128 * 1024 // 128 KB
	VHDXFirstRegionTableOffset  = 192 * 1024 // 192 KB  (= 3 × 64 KB)
	VHDXSecondRegionTableOffset = 256 * 1024 // 256 KB  (= 4 × 64 KB)
	VHDXLogAreaOffset           = 256 * 1024 // legacy alias; use VHDXSecondRegionTableOffset

	// VHD/VHDX block sizes
	DefaultSectorSize = 512

	// Parent locator
	VHDParentLocatorCount = 8
	VHDParentLocatorSize  = 24
	VHDParentFilenameSize = 512
)

// ============================================================================
// CHECKSUM & CRC-32 CONFIGURATION
// ============================================================================

const (
	CRC32Polynomial = 0x82f63b78
	CRC32Initial    = 0xFFFFFFFF
	CRC32FinalXOR   = 0xFFFFFFFF
)

// ============================================================================
// VHD FILE FOOTER (512 bytes, at EOF)
// ============================================================================

// FileFooter represents a VHD file footer (512 bytes).
type FileFooter struct {
	Signature          [8]byte  // "conectix"
	Features           uint32   // Big-endian
	FormatVersion      uint32   // Big-endian (0x00010000 = v1.0)
	NextOffset         uint64   // Big-endian
	ModificationTime   uint32   // Big-endian (Unix timestamp)
	CreatorApplication uint32   // Big-endian
	CreatorVersion     uint32   // Big-endian
	CreatorOS          uint32   // Big-endian
	DiskSize           uint64   // Big-endian (virtual disk size)
	DataSize           uint64   // Big-endian (physical data size)
	DiskGeometry       uint32   // Big-endian (CHS)
	DiskType           uint32   // Big-endian
	Checksum           uint32   // Big-endian (CRC-32)
	Identifier         [16]byte // GUID
	SavedState         byte
	Reserved           [427]byte
}

// ParsedFileFooter represents a parsed VHD file footer.
type ParsedFileFooter struct {
	FormatVersion uint32
	NextOffset    int64
	MediaSize     uint64
	DiskType      DiskType
	Checksum      uint32
	Identifier    [16]byte
	ModTime       time.Time
	CreatorApp    string
}

// ============================================================================
// VHD DYNAMIC DISK HEADER (1024 bytes)
// ============================================================================

// DynamicDiskHeader represents a VHD dynamic disk header (1024 bytes).
type DynamicDiskHeader struct {
	Signature              [8]byte  // "cxsparse"
	NextOffset             uint64   // Big-endian
	BlockTableOffset       uint64   // Big-endian
	FormatVersion          uint32   // Big-endian
	NumberOfBlocks         uint32   // Big-endian
	BlockSize              uint32   // Big-endian
	Checksum               uint32   // Big-endian (CRC-32)
	ParentIdentifier       [16]byte // GUID
	ParentModificationTime uint32   // Big-endian (Unix timestamp)
	Reserved1              [4]byte
	ParentFilename         [512]byte    // UTF-16 BE
	ParentLocatorEntries   [8 * 24]byte // 8 entries × 24 bytes each
	Reserved2              [256]byte
}

// ParsedDynamicDiskHeader represents a parsed VHD dynamic disk header.
type ParsedDynamicDiskHeader struct {
	FormatVersion    uint32
	BlockTableOffset int64
	NextOffset       int64
	BlockSize        uint32
	NumberOfBlocks   uint32
	ParentIdentifier [16]byte
	ParentModTime    time.Time
	ParentFilename   string
	ParentLocators   []ParentLocatorEntry
}

// ============================================================================
// VHD PARENT LOCATOR STRUCTURES
// ============================================================================

// ParentLocatorHeader represents a parent locator header (4 bytes).
type ParentLocatorHeader struct {
	NumberOfEntries uint32 // Big-endian
}

// ParentLocatorEntry represents a single parent locator entry.
type ParentLocatorEntry struct {
	PlatformCode       uint32 // Big-endian (0=None, 1=Wi2r, 2=Wi2ku, 3=W2ru, 4=W2ku)
	PlatformDataSpace  uint32 // Big-endian (reserved)
	PlatformDataLength uint32 // Big-endian
	Reserved           uint32 // Big-endian
	PlatformDataOffset uint64 // Big-endian
	Key                string
	Value              string
}

// ============================================================================
// VHDX FILE INFORMATION (520 bytes, at offset 0)
// ============================================================================

// FileInformation represents VHDX file information (520 bytes).
type FileInformation struct {
	Signature [8]byte   // "vhdxfile"
	Creator   [512]byte // UTF-8 string
}

// ParsedFileInformation represents parsed VHDX file information.
type ParsedFileInformation struct {
	Signature string
	Creator   string
}

// ============================================================================
// VHDX IMAGE HEADER (4096 bytes, at 64KB and 128KB)
// ============================================================================

// ImageHeader represents a VHDX image header (4096 bytes).
type ImageHeader struct {
	Signature           [4]byte  // "head"
	Checksum            uint32   // Little-endian (CRC-32)
	SequenceNumber      uint64   // Little-endian
	FileWriteIdentifier [16]byte // GUID
	DataWriteIdentifier [16]byte // GUID
	LogIdentifier       [16]byte // GUID
	LogFormatVersion    uint16   // Little-endian
	FormatVersion       uint16   // Little-endian (0x0001 = v1.0)
	LogSize             uint32   // Little-endian
	LogOffset           uint64   // Little-endian
	Reserved            [4016]byte
}

// ParsedImageHeader represents a parsed VHDX image header.
type ParsedImageHeader struct {
	Checksum            uint32
	SequenceNumber      uint64
	FormatVersion       uint16
	LogFormatVersion    uint16
	LogSize             uint32
	LogOffset           int64
	FileWriteIdentifier [16]byte
	DataWriteIdentifier [16]byte
	LogIdentifier       [16]byte
}

// ============================================================================
// VHDX REGION TABLE
// ============================================================================

// RegionTableHeader represents a VHDX region table header (16 bytes).
type RegionTableHeader struct {
	Signature       [4]byte // "regi"
	Checksum        uint32  // Little-endian (CRC-32)
	NumberOfEntries uint32  // Little-endian
	Reserved        uint32  // Little-endian
}

// RegionTableEntry represents a single region table entry (32 bytes).
type RegionTableEntry struct {
	TypeIdentifier [16]byte // GUID
	DataOffset     uint64   // Little-endian
	DataSize       uint32   // Little-endian
	IsRequired     uint32   // Little-endian (0 or 1)
}

// ParsedRegionTableEntry represents a parsed region table entry.
type ParsedRegionTableEntry struct {
	TypeIdentifier [16]byte
	DataOffset     int64
	DataSize       uint32
	IsRequired     bool
}

// ============================================================================
// VHDX METADATA TABLE
// ============================================================================

// MetadataTableHeader represents a VHDX metadata table header (32 bytes).
type MetadataTableHeader struct {
	Signature       [8]byte // "metadata"
	Reserved1       [2]byte
	NumberOfEntries uint16 // Little-endian
	Reserved2       [20]byte
}

// MetadataTableEntry represents a single metadata table entry (32 bytes).
type MetadataTableEntry struct {
	ItemIdentifier [16]byte // GUID
	ItemOffset     uint32   // Little-endian
	ItemSize       uint32   // Little-endian
	Reserved       [8]byte
}

// ParsedMetadataTableEntry represents a parsed metadata table entry.
type ParsedMetadataTableEntry struct {
	ItemIdentifier [16]byte
	ItemOffset     uint32
	ItemSize       uint32
	IsRequired     bool // bit 2 of the flags word
}

// ============================================================================
// VHDX BLOCK ALLOCATION TABLE
// ============================================================================

// BlockAllocationEntry represents a single BAT entry.
// For VHD: 4 bytes (big-endian uint32)
// For VHDX: 8 bytes (little-endian uint64)
type BlockAllocationEntry struct {
	IsAllocated bool       // Derived from entry value
	FileOffset  int64      // Calculated offset in file
	BlockState  BlockState // For VHDX
}

// VHDBlockAllocationTable represents VHD BAT information.
type VHDBlockAllocationTable struct {
	NumberOfEntries  uint32
	TableOffset      int64
	BlockSize        uint32
	SectorBitmapSize uint32
	Entries          []BlockAllocationEntry
}

// VHDXBlockAllocationTable represents VHDX BAT information.
type VHDXBlockAllocationTable struct {
	NumberOfEntries     uint32
	TableOffset         int64
	BlockSize           uint32
	SectorBitmapSize    uint32
	ChunkRatio          uint32  // data blocks per sector bitmap entry
	SectorSize          uint32  // logical sector size in bytes
	SectorBitmapOffsets []int64 // per-chunk sector bitmap file offsets; -1 = not present
	Entries             []BlockAllocationEntry
}

// ============================================================================
// BLOCK DESCRIPTORS & SECTOR RANGES
// ============================================================================

// BlockDescriptor represents information about a single block.
type BlockDescriptor struct {
	FileOffset   int64
	BlockState   BlockState
	SectorRanges []SectorRangeDescriptor
}

// SectorRangeDescriptor represents an allocated sector range within a block.
type SectorRangeDescriptor struct {
	StartOffset uint64 // Start of allocated region
	EndOffset   uint64 // End of allocated region
	Flags       SectorRangeFlag
}

// ============================================================================
// METADATA VALUES (VHDX)
// ============================================================================

// MetadataValues represents extracted VHDX metadata values.
type MetadataValues struct {
	BlockSize             uint32
	DiskType              DiskType
	LogicalSectorSize     uint32
	ParentIdentifier      [16]byte
	ParentFilename        string
	PhysicalSectorSize    uint32
	VirtualDiskIdentifier [16]byte
	VirtualDiskSize       uint64
	LeaveBitsUnallocated  bool
}

// ============================================================================
// IO HANDLE (Internal state)
// ============================================================================

// IOHandle represents the internal I/O state for an open file.
type IOHandle struct {
	FileType       FileFormat
	DiskType       DiskType
	MediaSize      uint64 // Virtual disk size
	BytesPerSector uint32
	BlockSize      uint32
	Abort          bool
}

// ============================================================================
// REGION TYPE IDENTIFIERS (GUIDs for VHDX regions)
// ============================================================================

// Regional type identifiers (as byte arrays, little-endian stored)
var (
	// BAT region identifier
	RegionTypeBAT = [16]byte{0x66, 0x77, 0xc2, 0x2d, 0x23, 0xf6, 0x00, 0x42, 0x9d, 0x64, 0x11, 0x5e, 0x9b, 0xfd, 0x4a, 0x08}

	// Metadata region identifier
	RegionTypeMetadata = [16]byte{0x06, 0xa2, 0x7c, 0x8b, 0x90, 0x47, 0x9a, 0x4b, 0xb8, 0xfe, 0x57, 0x5f, 0x05, 0x0f, 0x88, 0x6e}
)

// ============================================================================
// METADATA ITEM IDENTIFIERS (GUIDs for VHDX metadata items)
// ============================================================================

// Metadata item identifiers (as byte arrays, little-endian stored)
var (
	// File parameters
	MetadataItemFileParameters = [16]byte{0x37, 0x67, 0xa1, 0xca, 0x36, 0xfa, 0x43, 0x4d, 0xb3, 0xb6, 0x33, 0xf0, 0xaa, 0x44, 0xe7, 0x6b}

	// Logical sector size
	MetadataItemLogicalSectorSize = [16]byte{0x1d, 0xbf, 0x41, 0x81, 0x6f, 0xa9, 0x09, 0x47, 0xba, 0x47, 0xf2, 0x33, 0xa8, 0xfa, 0xab, 0x5f}

	// Parent locator
	MetadataItemParentLocator = [16]byte{0x2d, 0x5f, 0xd3, 0xa8, 0x0b, 0xb3, 0x4d, 0x45, 0xab, 0xf7, 0xd3, 0xd8, 0x48, 0x34, 0xab, 0x0c}

	// Physical sector size
	MetadataItemPhysicalSectorSize = [16]byte{0xc7, 0x48, 0xa3, 0xcd, 0x5d, 0x44, 0x71, 0x44, 0x9c, 0xc9, 0xe9, 0x88, 0x52, 0x51, 0xc5, 0x56}

	// Virtual disk identifier
	MetadataItemVirtualDiskIdentifier = [16]byte{0xab, 0x12, 0xca, 0xbe, 0xe6, 0xb2, 0x23, 0x45, 0x93, 0xef, 0xc3, 0x09, 0xe0, 0x00, 0xc7, 0x46}

	// Virtual disk size
	MetadataItemVirtualDiskSize = [16]byte{0x24, 0x42, 0xa5, 0x2f, 0x1b, 0xcd, 0x76, 0x48, 0xb2, 0x11, 0x5d, 0xbe, 0xd8, 0x3b, 0xf4, 0xb8}
)

// ============================================================================
// VERSION NUMBERS
// ============================================================================

const (
	VHDFormatVersion  = 0x00010000 // VHD v1.0
	VHDXFormatVersion = 0x0001     // VHDX v1.0
)

// ============================================================================
// CACHE SETTINGS
// ============================================================================

const (
	MaxCacheEntriesBlockDescriptors = 8
)
