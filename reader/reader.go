// Package reader provides high-level APIs for reading VHD/VHDX virtual disk files.
package reader

import (
	"errors"
	"fmt"
	"io"

	"github.com/aoiflux/libvhdi/bat"
	"github.com/aoiflux/libvhdi/block"
	"github.com/aoiflux/libvhdi/diff"
	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/internal/vhdxlog"
	"github.com/aoiflux/libvhdi/metadata"
	"github.com/aoiflux/libvhdi/types"
)

// ErrParentRequired is returned when a read on a differencing disk resolves to
// a parent that has not been attached. Differencing disks fail closed: silently
// returning zeroes would be indistinguishable from genuine disk contents.
var ErrParentRequired = diff.ErrParentRequired

// VirtualDisk provides a unified read interface for VHD and VHDX virtual disks.
type VirtualDisk struct {
	source      io.ReaderAt
	closer      io.Closer // non-nil when we own the underlying file
	fileSize    int64
	format      types.FileFormat
	diskType    types.DiskType
	virtualSize uint64
	blockSize   uint32
	sectorSize  uint32

	// Format-specific state
	footer     *types.ParsedFileFooter        // VHD only
	dynHeader  *types.ParsedDynamicDiskHeader // VHD dynamic/differential only
	imgHeader  *types.ParsedImageHeader       // VHDX only
	metaValues *types.MetadataValues          // VHDX only

	// Child BAT retained for differencing disk SetParent wiring.
	childBATvhd  *types.VHDBlockAllocationTable
	childBATvhdx *types.VHDXBlockAllocationTable

	// Differencing state. resolver is non-nil for differencing disks and owns
	// the parent link; parent is the attached parent, if any.
	resolver *diff.Resolver
	parent   *VirtualDisk

	// path is the filesystem path this disk was opened from, when known. It
	// seeds the search for parent images.
	path string

	// ownedByChild marks a parent opened by automatic chain resolution, whose
	// lifetime the child controls. Parents attached by hand via SetParent stay
	// the caller's responsibility.
	ownedByChild bool

	// parentErr records why best-effort chain resolution stopped, if it did.
	parentErr error

	// hasLog marks a VHDX image whose active header carries a log GUID, meaning
	// it holds journalled writes.
	hasLog bool

	// replay is non-nil when that log was successfully replayed into an overlay.
	// When hasLog is true and replay is nil, the image was opened un-replayed
	// under Options.AllowDirtyImage and may be stale.
	replay *vhdxlog.Replay

	// The actual io.ReaderAt that handles virtual→physical mapping.
	// For fixed disks this is the raw source; for dynamic/VHDX it's a
	// block reader; for differencing it's a diff.Resolver.
	payload io.ReaderAt
}

// OpenVHD opens a VHD file (fixed, dynamic, or differencing).
// fileSize must be the total size of the file in bytes.
// For differencing disks, call SetParent before reading.
func OpenVHD(r io.ReaderAt, fileSize int64) (*VirtualDisk, error) {
	fp := NewVHDFooterParser(r)
	footer, err := fp.ReadFooterFromEnd(fileSize)
	if err != nil {
		return nil, err
	}

	d := &VirtualDisk{
		source:      r,
		fileSize:    fileSize,
		format:      types.FileFormatVHD,
		diskType:    footer.DiskType,
		virtualSize: footer.MediaSize,
		sectorSize:  types.DefaultSectorSize,
		footer:      footer,
	}

	switch footer.DiskType {
	case types.DiskTypeFixed:
		if err := validateVHDFixed(footer, fileSize); err != nil {
			return nil, err
		}
		// Fixed disk: data starts at offset 0, no BAT needed.
		d.payload = r
		// A fixed disk has no blocks; report the whole device as one block
		// without truncating a media size larger than 4 GiB.
		if footer.MediaSize > uint64(^uint32(0)) {
			d.blockSize = ^uint32(0)
		} else {
			d.blockSize = uint32(footer.MediaSize)
		}

	case types.DiskTypeDynamic, types.DiskTypeDifferential:
		// Read dynamic disk header.
		dhp := NewVHDDynamicDiskHeaderParser(r)
		dynHeader, err := dhp.ReadHeaderAt(footer.NextOffset)
		if err != nil {
			return nil, err
		}

		// Cross-check the header before it drives any allocation or read.
		if err := validateVHDDynamic(footer, dynHeader, fileSize); err != nil {
			return nil, err
		}

		d.dynHeader = dynHeader
		d.blockSize = dynHeader.BlockSize

		// Parse BAT.
		batParser := bat.NewVHDBATParser(r)
		vhdBat, err := batParser.ReadBAT(dynHeader.BlockTableOffset, dynHeader.NumberOfBlocks, dynHeader.BlockSize)
		if err != nil {
			return nil, err
		}
		d.childBATvhd = vhdBat
		childReader := block.NewVHDBlockReader(r, vhdBat, types.DefaultSectorSize)

		if footer.DiskType == types.DiskTypeDifferential {
			// Install the resolver up front with no parent attached. Reads that
			// resolve to the parent then fail closed with ErrParentRequired
			// until SetParent supplies one.
			res, err := diff.New(diff.Config{
				Source:      r,
				Child:       childReader,
				BlockSize:   dynHeader.BlockSize,
				VirtualSize: footer.MediaSize,
				VHDBAT:      vhdBat,
			})
			if err != nil {
				return nil, err
			}
			d.resolver = res
			d.payload = res
		} else {
			d.payload = childReader
		}

	default:
		return nil, errors.New("unsupported VHD disk type")
	}

	return d, nil
}

// OpenVHDX opens a VHDX file.
// fileSize must be the total size of the file in bytes.
//
// If the image carries a log, it is replayed into an in-memory overlay before
// anything else is parsed, so the disk presents the last committed state. The
// file itself is never modified. An image whose log cannot be replayed — no
// valid entries, or an incomplete sequence — is refused with ErrDirtyImage;
// use Open with Options.AllowDirtyImage to read it as-is anyway.
func OpenVHDX(r io.ReaderAt, fileSize int64) (*VirtualDisk, error) {
	return openVHDX(r, fileSize, false)
}

func openVHDX(r io.ReaderAt, fileSize int64, allowDirty bool) (*VirtualDisk, error) {
	// Parse image header (primary + backup) from the file itself. The headers
	// live outside the log's reach, so they are read before any replay.
	ihp := NewVHDXImageHeaderParser(r)
	imgHeader, err := ihp.ReadImageHeader()
	if err != nil {
		return nil, err
	}

	// A non-zero log GUID means the log holds writes that may not have reached
	// their final locations, so the structures below could be stale.
	hasLog := imgHeader.LogIdentifier != ([16]byte{})

	// src is what every later parse reads through: the file, or the file with
	// the replayed sectors layered over it.
	src := r
	var replay *vhdxlog.Replay

	if hasLog {
		replay, err = vhdxlog.Analyze(r, imgHeader.LogOffset, imgHeader.LogSize, imgHeader.LogIdentifier)
		switch {
		case err == nil:
			src = replay.Overlay(r)

		case allowDirty:
			// Proceed on the un-replayed file. Contents may predate the last
			// committed state; IsDirty reports this.
			replay = nil

		default:
			return nil, fmt.Errorf("%w: %v (log guid %s at offset %d, size %d)",
				ErrDirtyImage, err,
				binaryutil.GUIDToString(imgHeader.LogIdentifier),
				imgHeader.LogOffset, imgHeader.LogSize)
		}
	}

	// Parse region table. Try primary first (192KB), then secondary (256KB).
	rtp := NewVHDXRegionTableParser(src)

	locateRegions := func(regions []types.ParsedRegionTableEntry) (int64, int64, int64, uint32, error) {
		var batOffset, batSize int64
		var metaOffset int64
		var metaSize uint32
		for _, region := range regions {
			switch region.TypeIdentifier {
			case types.RegionTypeBAT:
				batOffset = region.DataOffset
				batSize = int64(region.DataSize)
			case types.RegionTypeMetadata:
				metaOffset = region.DataOffset
				metaSize = region.DataSize
			default:
				// Per spec: unknown entries with IsRequired=1 must cause failure.
				if region.IsRequired {
					return 0, 0, 0, 0, errors.New("VHDX region table contains unknown required entry")
				}
			}
		}
		return batOffset, batSize, metaOffset, metaSize, nil
	}

	var batOffset, batSize int64
	var metaOffset int64
	var metaSize uint32

	regions, err := rtp.ReadRegionTableAt(types.VHDXFirstRegionTableOffset)
	if err == nil {
		// Ignore locateRegions errors from the primary table; the secondary
		// table will be tried if BAT/metadata offsets are still zero.
		batOffset, batSize, metaOffset, metaSize, _ = locateRegions(regions)
	}

	if batOffset == 0 || metaOffset == 0 {
		regions2, err2 := rtp.ReadRegionTableAt(types.VHDXSecondRegionTableOffset)
		if err2 == nil {
			var err3 error
			batOffset, batSize, metaOffset, metaSize, err3 = locateRegions(regions2)
			if err3 != nil {
				return nil, err3
			}
		}
	}

	if batOffset == 0 {
		return nil, errors.New("VHDX BAT region not found")
	}
	if metaOffset == 0 {
		return nil, errors.New("VHDX metadata region not found")
	}

	// Parse metadata.
	mp := metadata.NewParser(src, metaOffset, metaSize)
	metaVals, err := mp.Parse()
	if err != nil {
		return nil, err
	}

	// Validate the geometry before it sizes the BAT allocation. batSize is the
	// region table's declared size for the BAT, used here as an upper bound.
	if err := validateVHDXGeometry(metaVals, batOffset, batSize, fileSize); err != nil {
		return nil, err
	}

	// Number of blocks = ceil(virtualDiskSize / blockSize). Bounded above by
	// validateVHDXGeometry, so this conversion cannot truncate.
	blockCount := uint32((metaVals.VirtualDiskSize + uint64(metaVals.BlockSize) - 1) / uint64(metaVals.BlockSize))

	// Parse BAT.
	batParser := bat.NewVHDXBATParser(src)
	vhdxBat, err := batParser.ReadBAT(batOffset, blockCount, metaVals.BlockSize, metaVals.LogicalSectorSize)
	if err != nil {
		return nil, err
	}

	// Payload reads also go through src, so replayed sectors are visible in the
	// data stream and not just in the metadata.
	childReader := block.NewVHDXBlockReader(src, vhdxBat, metaVals.LogicalSectorSize)

	d := &VirtualDisk{
		source:       src,
		fileSize:     fileSize,
		format:       types.FileFormatVHDX,
		diskType:     metaVals.DiskType,
		virtualSize:  metaVals.VirtualDiskSize,
		blockSize:    metaVals.BlockSize,
		sectorSize:   metaVals.LogicalSectorSize,
		imgHeader:    imgHeader,
		metaValues:   metaVals,
		childBATvhdx: vhdxBat,
		payload:      childReader,
		hasLog:       hasLog,
		replay:       replay,
	}

	if metaVals.DiskType == types.DiskTypeDifferential {
		// Install the resolver up front with no parent attached. Reads that
		// resolve to the parent then fail closed with ErrParentRequired until
		// SetParent supplies one.
		res, err := diff.New(diff.Config{
			Source:      src,
			Child:       childReader,
			BlockSize:   metaVals.BlockSize,
			SectorSize:  metaVals.LogicalSectorSize,
			VirtualSize: metaVals.VirtualDiskSize,
			VHDXBAT:     vhdxBat,
		})
		if err != nil {
			return nil, err
		}
		d.resolver = res
		d.payload = res
	}

	return d, nil
}

// OpenFile opens a VHD or VHDX file by path, auto-detecting the format.
//
// For a differencing disk, parents are resolved automatically from the image's
// own directory, so the returned disk presents a correct contiguous device
// without manual SetParent wiring. Resolution is best-effort: if the chain
// cannot be completed the disk still opens, ParentResolveError explains why, and
// reads that need the missing parent return ErrParentRequired rather than
// zeroes.
//
// Use OpenFileWith for control over parent resolution and the other options.
func OpenFile(path string) (*VirtualDisk, error) {
	return OpenFileWith(path, nil)
}

// ============================================================================
// VirtualDisk methods
// ============================================================================

// SetParent wires a parent disk for this differencing disk. After calling
// SetParent, reads on unallocated blocks are transparently satisfied by the
// parent disk. Returns an error if the disk is not a differencing disk or if
// the parent's virtual size doesn't match.
func (d *VirtualDisk) SetParent(parent *VirtualDisk) error {
	if !d.IsDifferencing() {
		return errors.New("disk is not a differencing disk")
	}
	if parent == nil {
		return errors.New("parent must not be nil")
	}
	if parent.virtualSize != d.virtualSize {
		return errors.New("parent virtual size does not match child")
	}

	if d.resolver == nil {
		return errors.New("differencing resolver not initialised")
	}

	d.resolver.SetParent(parent)
	d.parent = parent
	return nil
}

// NeedsParent reports whether this disk is a differencing disk whose parent has
// not been attached yet. Reads that resolve to the parent fail with
// ErrParentRequired while this is true.
func (d *VirtualDisk) NeedsParent() bool {
	return d.IsDifferencing() && d.parent == nil
}

// Parent returns the attached parent disk, or nil if none is attached.
func (d *VirtualDisk) Parent() *VirtualDisk { return d.parent }

// ChainDepth returns the number of disks backing this one, including itself.
// A fixed or dynamic disk has depth 1.
func (d *VirtualDisk) ChainDepth() int {
	n := 1
	for p := d.parent; p != nil; p = p.parent {
		n++
	}
	return n
}

// Close releases any resources held by the disk, including parents opened by
// automatic chain resolution. Parents attached by hand with SetParent are left
// alone; their lifetime stays with whoever opened them.
//
// The first error encountered is returned, but every link is still closed.
func (d *VirtualDisk) Close() error {
	var firstErr error

	if d.closer != nil {
		if err := d.closer.Close(); err != nil {
			firstErr = err
		}
		d.closer = nil
	}

	if d.parent != nil && d.parent.ownedByChild {
		if err := d.parent.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// ParentResolveError reports why automatic parent chain resolution stopped
// short, or nil if the chain is complete or no resolution was attempted.
//
// Resolution is best-effort by default, so a disk can open successfully with an
// incomplete chain. Reads that need the missing parent fail with
// ErrParentRequired rather than returning zeroes.
func (d *VirtualDisk) ParentResolveError() error { return d.parentErr }

// ParentLocators returns the parent locator entries recorded in a differencing
// disk, which may carry candidate paths to the parent image.
func (d *VirtualDisk) ParentLocators() []types.ParentLocatorEntry {
	if d.dynHeader != nil {
		return d.dynHeader.ParentLocators
	}
	if d.metaValues != nil {
		return d.metaValues.ParentLocators
	}
	return nil
}

// Path returns the filesystem path this disk was opened from, or "" when it was
// opened from a bare io.ReaderAt.
func (d *VirtualDisk) Path() string { return d.path }

// HasLog reports whether this VHDX image's active header carries a log GUID,
// meaning it holds journalled writes. This is true whether or not the log was
// replayable, so it is the right signal for recording that an image was captured
// mid-write.
func (d *VirtualDisk) HasLog() bool { return d.hasLog }

// LogReplayed reports whether the image's log was replayed into an in-memory
// overlay, in which case reads present the last committed state. The file itself
// is never modified.
func (d *VirtualDisk) LogReplayed() bool { return d.replay != nil }

// IsDirty reports whether this image carries a log that was not replayed, in
// which case its block allocation table and metadata may be stale and the
// decoded contents may not reflect the last committed state.
//
// Such an image only opens when Options.AllowDirtyImage is set, since either the
// log held no valid entries or its sequence was incomplete.
func (d *VirtualDisk) IsDirty() bool { return d.hasLog && d.replay == nil }

// LogReplayStats describes what a log replay applied.
type LogReplayStats struct {
	// Entries is the number of log entries replayed.
	Entries int

	// Descriptors is the number of descriptors applied across those entries.
	Descriptors int

	// Sectors is the number of distinct 4 KB file sectors the replay overrides.
	Sectors int

	// FirstSequence and LastSequence bound the replayed sequence numbers.
	FirstSequence, LastSequence uint64
}

// LogReplayStats returns what the log replay applied, and false if no log was
// replayed.
//
// The sector count is a useful evidentiary signal: it measures how much of the
// image was still in flight when the file was captured.
func (d *VirtualDisk) LogReplayStats() (LogReplayStats, bool) {
	if d.replay == nil {
		return LogReplayStats{}, false
	}
	first, last := d.replay.SequenceRange()
	return LogReplayStats{
		Entries:       d.replay.EntryCount(),
		Descriptors:   d.replay.DescriptorCount(),
		Sectors:       d.replay.SectorCount(),
		FirstSequence: first,
		LastSequence:  last,
	}, true
}

// ReadAt reads len(p) bytes from the virtual disk at the given virtual offset.
// Implements io.ReaderAt.
//
// Reads are clamped to the virtual disk size: a request straddling the end of
// the device returns the available bytes and io.EOF. Without clamping, callers
// would receive trailing block padding — and, for fixed VHDs, the file footer —
// as if it were disk content.
func (d *VirtualDisk) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("virtual offset must be >= 0")
	}
	if uint64(offset) >= d.virtualSize {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	clamped := false
	if remaining := int64(d.virtualSize) - offset; int64(len(p)) > remaining {
		p = p[:remaining]
		clamped = true
	}

	n, err := d.payload.ReadAt(p, offset)
	if err == nil && clamped {
		return n, io.EOF
	}
	return n, err
}

// VirtualToFileOffset resolves a virtual disk byte offset to an absolute byte
// offset in the backing file when a direct mapping exists.
//
// The returned mapped flag is false for sparse/unallocated regions (virtual
// zeros) and for differencing disks, where a single file offset cannot express
// the answer because different byte ranges come from different files in the
// chain. Use Extents for those: it resolves per sector and reports which disk in
// the chain backs each run.
//
// Offsets returned by this method are absolute from the start of the backing
// VHD/VHDX file, not relative to BAT/region/block structures.
func (d *VirtualDisk) VirtualToFileOffset(virtualOffset int64) (fileOffset int64, mapped bool, err error) {
	if virtualOffset < 0 {
		return 0, false, errors.New("virtual offset must be >= 0")
	}
	if uint64(virtualOffset) >= d.virtualSize {
		return 0, false, io.EOF
	}

	if d.IsDifferencing() {
		// With a parent chain, the source can be child, parent, or higher ancestor.
		// Return unresolved instead of an ambiguous/incorrect single file offset.
		return 0, false, nil
	}

	switch d.format {
	case types.FileFormatVHD:
		if d.diskType == types.DiskTypeFixed {
			return virtualOffset, true, nil
		}
		if d.childBATvhd == nil {
			return 0, false, errors.New("VHD BAT not available")
		}
		blockSize := int64(d.blockSize)
		blockIndex := uint32(virtualOffset / blockSize)
		offsetInBlock := virtualOffset % blockSize
		if blockIndex >= d.childBATvhd.NumberOfEntries {
			return 0, false, io.EOF
		}
		entry := d.childBATvhd.Entries[blockIndex]
		if !entry.IsAllocated || entry.FileOffset < 0 {
			return 0, false, nil
		}
		return entry.FileOffset + offsetInBlock, true, nil

	case types.FileFormatVHDX:
		if d.childBATvhdx == nil {
			return 0, false, errors.New("VHDX BAT not available")
		}
		blockSize := int64(d.blockSize)
		blockIndex := uint32(virtualOffset / blockSize)
		offsetInBlock := virtualOffset % blockSize
		if blockIndex >= d.childBATvhdx.NumberOfEntries {
			return 0, false, io.EOF
		}
		entry := d.childBATvhdx.Entries[blockIndex]
		if !entry.IsAllocated || entry.FileOffset < 0 {
			return 0, false, nil
		}

		switch entry.BlockState {
		case types.BlockStateFullyAllocated:
			return entry.FileOffset + offsetInBlock, true, nil
		case types.BlockStatePartiallyAllocated:
			// For state-7 blocks, sector bitmap determines if this sector is backed.
			sectorSize := int64(d.sectorSize)
			chunkRatio := uint64(d.childBATvhdx.ChunkRatio)
			if chunkRatio == 0 || sectorSize <= 0 {
				return 0, false, errors.New("invalid VHDX BAT sector mapping")
			}
			chunkIndex := uint64(blockIndex) / chunkRatio
			if int(chunkIndex) >= len(d.childBATvhdx.SectorBitmapOffsets) {
				return 0, false, nil
			}
			bitmapOffset := d.childBATvhdx.SectorBitmapOffsets[chunkIndex]
			if bitmapOffset < 0 {
				return 0, false, nil
			}

			sectorsPerBlock := uint64(d.blockSize) / uint64(d.sectorSize)
			blockInChunk := uint64(blockIndex) % chunkRatio
			firstSectorInChunk := blockInChunk * sectorsPerBlock
			sectorInBlock := uint64(offsetInBlock) / uint64(d.sectorSize)
			bitPos := firstSectorInChunk + sectorInBlock
			bytePos := bitPos / 8
			bitOff := bitPos % 8

			var b [1]byte
			if _, readErr := d.source.ReadAt(b[:], bitmapOffset+int64(bytePos)); readErr != nil {
				return 0, false, readErr
			}
			allocated := ((b[0] >> bitOff) & 1) == 1
			if !allocated {
				return 0, false, nil
			}
			return entry.FileOffset + offsetInBlock, true, nil
		default:
			// Zero/unmapped/undefined states do not have direct payload bytes.
			return 0, false, nil
		}
	default:
		return 0, false, errors.New("unknown disk format")
	}
}

// Size returns the total virtual disk size in bytes.
func (d *VirtualDisk) Size() uint64 { return d.virtualSize }

// Format returns the file format.
func (d *VirtualDisk) Format() types.FileFormat { return d.format }

// DiskType returns the disk type (fixed, dynamic, differencing).
func (d *VirtualDisk) DiskType() types.DiskType { return d.diskType }

// BlockSize returns the block size in bytes.
func (d *VirtualDisk) BlockSize() uint32 { return d.blockSize }

// SectorSize returns the logical sector size in bytes.
func (d *VirtualDisk) SectorSize() uint32 { return d.sectorSize }

// IsDifferencing reports whether this is a differencing (child) disk.
func (d *VirtualDisk) IsDifferencing() bool { return d.diskType == types.DiskTypeDifferential }

// ParentIdentifier returns the parent disk GUID for a VHD differencing disk.
// Returns a zero GUID if this is not a differencing VHD.
func (d *VirtualDisk) ParentIdentifier() [16]byte {
	if d.dynHeader != nil {
		return d.dynHeader.ParentIdentifier
	}
	if d.metaValues != nil {
		return d.metaValues.ParentIdentifier
	}
	return [16]byte{}
}

// ParentFilename returns the parent disk path for a differencing disk.
func (d *VirtualDisk) ParentFilename() string {
	if d.dynHeader != nil {
		return d.dynHeader.ParentFilename
	}
	if d.metaValues != nil {
		return d.metaValues.ParentFilename
	}
	return ""
}

// Identifier returns the disk's unique identifier GUID.
func (d *VirtualDisk) Identifier() [16]byte {
	if d.footer != nil {
		return d.footer.Identifier
	}
	if d.metaValues != nil {
		return d.metaValues.VirtualDiskIdentifier
	}
	return [16]byte{}
}

// GUIDString returns the disk identifier as a formatted GUID string.
func (d *VirtualDisk) GUIDString() string {
	return binaryutil.GUIDToString(d.Identifier())
}
