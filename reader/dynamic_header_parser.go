// VHD Dynamic Disk Header Parser
package reader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"time"
	"unicode/utf16"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

// VHDDynamicDiskHeaderParser provides functionality to parse VHD dynamic disk headers.
type VHDDynamicDiskHeaderParser struct {
	reader io.ReaderAt
}

// NewVHDDynamicDiskHeaderParser creates a new VHD dynamic disk header parser.
func NewVHDDynamicDiskHeaderParser(r io.ReaderAt) *VHDDynamicDiskHeaderParser {
	return &VHDDynamicDiskHeaderParser{reader: r}
}

// ReadHeaderAt reads a VHD dynamic disk header from a specific offset.
func (p *VHDDynamicDiskHeaderParser) ReadHeaderAt(offset int64) (*types.ParsedDynamicDiskHeader, error) {
	br := binaryutil.NewReadAtReader(p.reader, offset)

	// Read header structure (1024 bytes, big-endian)
	header := &types.DynamicDiskHeader{}

	// Read signature (8 bytes)
	sig, err := br.ReadBytes(8)
	if err != nil {
		return nil, err
	}
	copy(header.Signature[:], sig)

	// Verify signature
	if !bytes.Equal(header.Signature[:], []byte(types.VHDDynamicDiskSignature)) {
		return nil, errors.New("invalid VHD dynamic disk header signature")
	}

	// Read remaining fields (big-endian)
	header.NextOffset, err = br.ReadUint64BigEndian()
	if err != nil {
		return nil, err
	}

	header.BlockTableOffset, err = br.ReadUint64BigEndian()
	if err != nil {
		return nil, err
	}

	header.FormatVersion, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	header.NumberOfBlocks, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	header.BlockSize, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	header.Checksum, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	// Read parent identifier (16 bytes)
	parentID, err := br.ReadBytes(16)
	if err != nil {
		return nil, err
	}
	copy(header.ParentIdentifier[:], parentID)

	header.ParentModificationTime, err = br.ReadUint32BigEndian()
	if err != nil {
		return nil, err
	}

	// Read reserved1 (4 bytes) — stored for checksum calculation.
	reserved1, err := br.ReadBytes(4)
	if err != nil {
		return nil, err
	}
	copy(header.Reserved1[:], reserved1)

	// Read parent filename (512 bytes, UTF-16 BE)
	parentFilenameRaw, err := br.ReadBytes(512)
	if err != nil {
		return nil, err
	}
	copy(header.ParentFilename[:], parentFilenameRaw)

	// Read parent locator entries (8 × 24 bytes = 192 bytes)
	locatorData, err := br.ReadBytes(8 * 24)
	if err != nil {
		return nil, err
	}
	copy(header.ParentLocatorEntries[:], locatorData)

	// Read reserved2 (256 bytes) — stored for checksum calculation.
	reserved2, err := br.ReadBytes(256)
	if err != nil {
		return nil, err
	}
	copy(header.Reserved2[:], reserved2)

	// Verify CRC-32 checksum.
	if !p.VerifyHeaderChecksum(header) {
		return nil, errors.New("VHD dynamic disk header checksum mismatch")
	}

	// Parse into typed header
	parsed := &types.ParsedDynamicDiskHeader{
		FormatVersion:    header.FormatVersion,
		BlockTableOffset: int64(header.BlockTableOffset),
		NextOffset:       int64(header.NextOffset),
		BlockSize:        header.BlockSize,
		NumberOfBlocks:   header.NumberOfBlocks,
		ParentIdentifier: header.ParentIdentifier,
		ParentModTime:    time.Unix(int64(header.ParentModificationTime), 0),
		ParentFilename:   decodeUTF16BE(header.ParentFilename[:]),
	}

	// Parse parent locator entries
	parsed.ParentLocators, err = p.parseParentLocators(header.ParentLocatorEntries[:])
	if err != nil {
		return nil, err
	}

	// Validate format version (must be 0x00010000 = v1.0).
	if header.FormatVersion != 0x00010000 {
		return nil, errors.New("unsupported VHD dynamic disk header format version")
	}

	// Validate block size: must be > 0 and a multiple of 512.
	if header.BlockSize == 0 || header.BlockSize%512 != 0 {
		return nil, errors.New("invalid VHD dynamic disk block size")
	}

	return parsed, nil
}

// parseParentLocators parses the 8 parent locator entries (24 bytes each).
func (p *VHDDynamicDiskHeaderParser) parseParentLocators(data []byte) ([]types.ParentLocatorEntry, error) {
	var entries []types.ParentLocatorEntry

	// Each parent locator entry is 24 bytes
	for i := 0; i < 8; i++ {
		offset := i * 24
		if offset+24 > len(data) {
			break
		}

		entryData := data[offset : offset+24]
		br := binaryutil.NewReadAtReader(bytes.NewReader(entryData), 0)

		// Read 6 uint32 fields (big-endian)
		platformCode, _ := br.ReadUint32BigEndian()
		platformDataSpace, _ := br.ReadUint32BigEndian()
		platformDataLength, _ := br.ReadUint32BigEndian()
		reserved, _ := br.ReadUint32BigEndian()
		platformDataOffsetHigh, _ := br.ReadUint32BigEndian()
		platformDataOffsetLow, _ := br.ReadUint32BigEndian()

		// Platform code 0 means no entry
		if platformCode == 0 {
			continue
		}

		platformDataOffset := (uint64(platformDataOffsetHigh) << 32) | uint64(platformDataOffsetLow)

		entries = append(entries, types.ParentLocatorEntry{
			PlatformCode:       platformCode,
			PlatformDataSpace:  platformDataSpace,
			PlatformDataLength: platformDataLength,
			Reserved:           reserved,
			PlatformDataOffset: platformDataOffset,
		})
	}

	return entries, nil
}

// VerifyHeaderChecksum verifies the header's CRC-32 checksum.
func (p *VHDDynamicDiskHeaderParser) VerifyHeaderChecksum(header *types.DynamicDiskHeader) bool {
	// Checksum is calculated with the checksum field itself set to zero
	headerCopy := *header
	headerCopy.Checksum = 0

	// Serialize header to bytes
	buf := headerToBytes(&headerCopy)

	// Compute CRC-32
	computed := binaryutil.CRC32(buf)

	return computed == header.Checksum
}

// headerToBytes serializes a DynamicDiskHeader to bytes.
func headerToBytes(header *types.DynamicDiskHeader) []byte {
	buf := new(bytes.Buffer)

	// Write all fields in big-endian order
	buf.Write(header.Signature[:])
	writeBE64(buf, header.NextOffset)
	writeBE64(buf, header.BlockTableOffset)
	writeBE32(buf, header.FormatVersion)
	writeBE32(buf, header.NumberOfBlocks)
	writeBE32(buf, header.BlockSize)
	writeBE32(buf, header.Checksum)
	buf.Write(header.ParentIdentifier[:])
	writeBE32(buf, header.ParentModificationTime)
	buf.Write(header.Reserved1[:])
	buf.Write(header.ParentFilename[:])
	buf.Write(header.ParentLocatorEntries[:])
	buf.Write(header.Reserved2[:])

	return buf.Bytes()
}

// decodeUTF16BE decodes a UTF-16 big-endian encoded string with null termination.
func decodeUTF16BE(data []byte) string {
	// Convert byte array to uint16 array (big-endian)
	if len(data)%2 != 0 {
		return ""
	}

	utf16Data := make([]uint16, 0, len(data)/2)
	for i := 0; i < len(data); i += 2 {
		val := binary.BigEndian.Uint16(data[i : i+2])
		if val == 0 {
			break
		}
		utf16Data = append(utf16Data, val)
	}

	// Decode UTF-16 to string
	if len(utf16Data) == 0 {
		return ""
	}

	// Handle UTF-16 decoding
	runes := utf16.Decode(utf16Data)
	return string(runes)
}
