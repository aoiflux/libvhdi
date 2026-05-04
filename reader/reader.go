// Package reader provides high-level APIs for reading VHD/VHDX virtual disk files.
package reader

import (
	"errors"
	"io"
	"os"

	"github.com/aoiflux/libvhdi/bat"
	"github.com/aoiflux/libvhdi/block"
	"github.com/aoiflux/libvhdi/diff"
	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/metadata"
	"github.com/aoiflux/libvhdi/types"
)

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
		// Fixed disk: data starts at offset 0, no BAT needed.
		d.payload = r
		d.blockSize = uint32(footer.MediaSize)

	case types.DiskTypeDynamic, types.DiskTypeDifferential:
		// Read dynamic disk header.
		dhp := NewVHDDynamicDiskHeaderParser(r)
		dynHeader, err := dhp.ReadHeaderAt(footer.NextOffset)
		if err != nil {
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
		d.payload = block.NewVHDBlockReader(r, vhdBat, types.DefaultSectorSize)

	default:
		return nil, errors.New("unsupported VHD disk type")
	}

	return d, nil
}

// OpenVHDX opens a VHDX file.
// fileSize must be the total size of the file in bytes.
func OpenVHDX(r io.ReaderAt, fileSize int64) (*VirtualDisk, error) {
	// Parse image header (primary + backup).
	ihp := NewVHDXImageHeaderParser(r)
	imgHeader, err := ihp.ReadImageHeader()
	if err != nil {
		return nil, err
	}

	// Parse region table. Try primary first (192KB), then secondary (256KB).
	rtp := NewVHDXRegionTableParser(r)

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
	_ = batSize

	// Parse metadata.
	mp := metadata.NewParser(r, metaOffset, metaSize)
	metaVals, err := mp.Parse()
	if err != nil {
		return nil, err
	}

	// Number of blocks = ceil(virtualDiskSize / blockSize)
	blockCount := uint32((metaVals.VirtualDiskSize + uint64(metaVals.BlockSize) - 1) / uint64(metaVals.BlockSize))

	// Parse BAT.
	batParser := bat.NewVHDXBATParser(r)
	vhdxBat, err := batParser.ReadBAT(batOffset, blockCount, metaVals.BlockSize, metaVals.LogicalSectorSize)
	if err != nil {
		return nil, err
	}

	d := &VirtualDisk{
		source:       r,
		fileSize:     fileSize,
		format:       types.FileFormatVHDX,
		diskType:     metaVals.DiskType,
		virtualSize:  metaVals.VirtualDiskSize,
		blockSize:    metaVals.BlockSize,
		sectorSize:   metaVals.LogicalSectorSize,
		imgHeader:    imgHeader,
		metaValues:   metaVals,
		childBATvhdx: vhdxBat,
		payload:      block.NewVHDXBlockReader(r, vhdxBat, metaVals.LogicalSectorSize),
	}

	return d, nil
}

// OpenFile opens a VHD or VHDX file by path, auto-detecting the format.
func OpenFile(path string) (*VirtualDisk, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	fileSize := info.Size()

	// Detect format: peek at offset 0 for VHDX, or try VHD footer.
	sig := make([]byte, 8)
	if _, err := f.ReadAt(sig, 0); err != nil {
		f.Close()
		return nil, err
	}

	var d *VirtualDisk
	if string(sig) == types.VHDXFileSignature {
		d, err = OpenVHDX(f, fileSize)
	} else {
		d, err = OpenVHD(f, fileSize)
	}
	if err != nil {
		f.Close()
		return nil, err
	}

	// Transfer file ownership: VirtualDisk.Close() will close the file.
	d.closer = f
	return d, nil
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

	switch d.format {
	case types.FileFormatVHD:
		if d.childBATvhd == nil {
			return errors.New("child VHD BAT not available")
		}
		d.payload = diff.NewVHDResolver(
			block.NewVHDBlockReader(d.source, d.childBATvhd, d.sectorSize),
			parent,
			d.childBATvhd,
			d.blockSize,
			d.virtualSize,
		)
	case types.FileFormatVHDX:
		if d.childBATvhdx == nil {
			return errors.New("child VHDX BAT not available")
		}
		d.payload = diff.NewVHDXResolver(
			block.NewVHDXBlockReader(d.source, d.childBATvhdx, d.sectorSize),
			parent,
			d.childBATvhdx,
			d.blockSize,
			d.virtualSize,
		)
	default:
		return errors.New("unsupported format for differencing disk")
	}
	return nil
}

// Close releases any resources held by the disk. Should be called when done.
func (d *VirtualDisk) Close() error {
	if d.closer != nil {
		return d.closer.Close()
	}
	return nil
}

// ReadAt reads len(p) bytes from the virtual disk at the given virtual offset.
// Implements io.ReaderAt.
func (d *VirtualDisk) ReadAt(p []byte, offset int64) (int, error) {
	if uint64(offset) >= d.virtualSize {
		return 0, io.EOF
	}
	return d.payload.ReadAt(p, offset)
}

// VirtualToFileOffset resolves a virtual disk byte offset to an absolute byte
// offset in the backing file when a direct mapping exists.
//
// The returned mapped flag is false for sparse/unallocated regions (virtual
// zeros) and for differencing disks where data may come from a parent chain.
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
