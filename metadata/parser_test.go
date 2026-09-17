// SPDX-License-Identifier: MIT

package metadata

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/aoiflux/libvhdi/types"
)

// The metadata region is where a VHDX says how large it is, how it is laid out
// and which parent it belongs to. Every offset and length in it comes straight
// out of the image, so each one is attacker-controlled and each one is used to
// seek and read. An unchecked pair does not fail loudly: it decodes a structure
// out of unrelated bytes and reports the result as metadata.

const testRegionSize = 64 << 10

// metaBuilder assembles a metadata region for a test.
type metaBuilder struct {
	region  []byte
	entries int
}

func newMetaBuilder() *metaBuilder {
	b := &metaBuilder{region: make([]byte, testRegionSize)}
	copy(b.region[0:8], []byte(types.VHDXMetadataSignature))
	return b
}

// addEntry appends a table entry pointing at itemOffset for itemSize bytes.
func (b *metaBuilder) addEntry(id [16]byte, itemOffset, itemSize uint32, required bool) *metaBuilder {
	off := metadataTableHeaderSize + b.entries*metadataTableEntrySize
	copy(b.region[off:off+16], id[:])
	binary.LittleEndian.PutUint32(b.region[off+16:], itemOffset)
	binary.LittleEndian.PutUint32(b.region[off+20:], itemSize)
	var flags uint32
	if required {
		flags |= 1 << 2
	}
	binary.LittleEndian.PutUint32(b.region[off+24:], flags)
	b.entries++
	binary.LittleEndian.PutUint16(b.region[10:12], uint16(b.entries))
	return b
}

// put writes raw item bytes at an offset within the region.
func (b *metaBuilder) put(offset uint32, data []byte) *metaBuilder {
	copy(b.region[offset:], data)
	return b
}

func (b *metaBuilder) parse() (*types.MetadataValues, error) {
	return NewParser(bytes.NewReader(b.region), 0, testRegionSize).Parse()
}

// Item offsets used throughout. All sit well past the table, as a conformant
// producer's do.
const (
	offFileParams = 0x1000
	offDiskSize   = 0x1010
	offSectorSize = 0x1020
	offDiskID     = 0x1030
	offParentLoc  = 0x1100
)

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

// validMeta returns a builder holding a minimal but complete metadata region.
func validMeta() *metaBuilder {
	b := newMetaBuilder()
	b.addEntry(types.MetadataItemFileParameters, offFileParams, 8, true)
	b.addEntry(types.MetadataItemVirtualDiskSize, offDiskSize, 8, true)
	b.addEntry(types.MetadataItemLogicalSectorSize, offSectorSize, 4, true)
	b.addEntry(types.MetadataItemVirtualDiskIdentifier, offDiskID, 16, true)

	b.put(offFileParams, u32(2<<20)) // block size
	b.put(offFileParams+4, u32(0))   // flags: dynamic
	b.put(offDiskSize, u64(64<<20))
	b.put(offSectorSize, u32(512))
	b.put(offDiskID, bytes.Repeat([]byte{0xAB}, 16))
	return b
}

func TestParseDecodesAConformantRegion(t *testing.T) {
	vals, err := validMeta().parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if vals.BlockSize != 2<<20 {
		t.Errorf("BlockSize = %d, want %d", vals.BlockSize, 2<<20)
	}
	if vals.VirtualDiskSize != 64<<20 {
		t.Errorf("VirtualDiskSize = %d, want %d", vals.VirtualDiskSize, 64<<20)
	}
	if vals.LogicalSectorSize != 512 {
		t.Errorf("LogicalSectorSize = %d, want 512", vals.LogicalSectorSize)
	}
	if vals.DiskType != types.DiskTypeDynamic {
		t.Errorf("DiskType = %v, want dynamic", vals.DiskType)
	}
	want := [16]byte{}
	for i := range want {
		want[i] = 0xAB
	}
	if vals.VirtualDiskIdentifier != want {
		t.Errorf("VirtualDiskIdentifier = %x, want %x", vals.VirtualDiskIdentifier, want)
	}
}

func TestPhysicalSectorSizeDefaultsWhenAbsent(t *testing.T) {
	// The item is optional. Defaulting rather than erroring is deliberate, but
	// the default has to be the one the format specifies.
	vals, err := validMeta().parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if vals.PhysicalSectorSize != 512 {
		t.Errorf("PhysicalSectorSize = %d, want the 512 default", vals.PhysicalSectorSize)
	}
}

// ---------------------------------------------------------------------------
// Item offset and length bounds
// ---------------------------------------------------------------------------

func TestItemPastTheRegionIsRejected(t *testing.T) {
	// The decisive case. Without a bounds check the parser reads whatever
	// follows the metadata region -- the BAT, payload blocks, or nothing at all
	// -- and reports it as the virtual disk's size.
	b := validMeta()
	b.addEntry(types.MetadataItemPhysicalSectorSize, testRegionSize-2, 4, false)

	_, err := b.parse()
	if err == nil {
		t.Fatal("accepted a metadata item extending past the end of its region")
	}
	if !strings.Contains(err.Error(), "metadata region") {
		t.Fatalf("error does not name the region bound: %v", err)
	}
}

func TestItemStartingPastTheRegionIsRejected(t *testing.T) {
	b := validMeta()
	b.addEntry(types.MetadataItemPhysicalSectorSize, testRegionSize+4096, 4, false)

	if _, err := b.parse(); err == nil {
		t.Fatal("accepted a metadata item starting past the end of its region")
	}
}

func TestItemOverlappingTheTableIsRejected(t *testing.T) {
	// An item claiming to live inside the metadata table describes its own
	// directory as its payload. No conformant producer emits this, and reading
	// it would decode table entries as a value.
	b := validMeta()
	b.addEntry(types.MetadataItemPhysicalSectorSize, 40, 4, false)

	_, err := b.parse()
	if err == nil {
		t.Fatal("accepted a metadata item overlapping the metadata table")
	}
	if !strings.Contains(err.Error(), "overlaps the metadata table") {
		t.Fatalf("error does not say the item overlaps the table: %v", err)
	}
}

func TestOversizedItemIsRejected(t *testing.T) {
	// A length beyond the format's maximum is the lever that turns a few bytes
	// of table into a huge read.
	b := validMeta()
	b.addEntry(types.MetadataItemPhysicalSectorSize, 0x2000, maxMetadataItemSize+1, false)

	if _, err := b.parse(); err == nil {
		t.Fatal("accepted a metadata item larger than the format permits")
	}
}

func TestZeroLengthItemMustHaveZeroOffset(t *testing.T) {
	// An item with no data must not claim a location either; the pair is
	// meaningless otherwise, and accepting it hides a producer bug.
	b := validMeta()
	b.addEntry(types.MetadataItemPhysicalSectorSize, 0x2000, 0, false)

	if _, err := b.parse(); err == nil {
		t.Fatal("accepted a zero-length item with a non-zero offset")
	}
}

func TestZeroLengthItemWithZeroOffsetIsIgnored(t *testing.T) {
	b := validMeta()
	b.addEntry(types.MetadataItemPhysicalSectorSize, 0, 0, false)

	if _, err := b.parse(); err != nil {
		t.Fatalf("rejected a well-formed empty item: %v", err)
	}
}

func TestEntryCountBeyondTheRegionIsRejected(t *testing.T) {
	// The table header and entries are fixed-size, so the region physically
	// bounds how many entries can exist.
	b := validMeta()
	binary.LittleEndian.PutUint16(b.region[10:12], 0xFFFF)

	if _, err := b.parse(); err == nil {
		t.Fatal("accepted a table declaring more entries than its region can hold")
	}
}

// ---------------------------------------------------------------------------
// Required-item and signature rules
// ---------------------------------------------------------------------------

func TestUnknownRequiredItemIsRejected(t *testing.T) {
	// The same forward-compatibility guard the region table has: an image that
	// marks an item required is saying it cannot be understood without it.
	b := validMeta()
	b.addEntry([16]byte{0xDE, 0xAD}, 0x2000, 8, true)
	b.put(0x2000, u64(1))

	if _, err := b.parse(); err == nil {
		t.Fatal("accepted a metadata table with an unknown required item")
	}
}

func TestUnknownOptionalItemIsIgnored(t *testing.T) {
	b := validMeta()
	b.addEntry([16]byte{0xDE, 0xAD}, 0x2000, 8, false)
	b.put(0x2000, u64(1))

	if _, err := b.parse(); err != nil {
		t.Fatalf("rejected an unknown optional item, which must be skipped: %v", err)
	}
}

func TestBadSignatureIsRejected(t *testing.T) {
	b := validMeta()
	copy(b.region[0:8], []byte("notmeta!"))

	if _, err := b.parse(); err == nil {
		t.Fatal("accepted a region without the metadata signature")
	}
}

// ---------------------------------------------------------------------------
// Value-level validation
// ---------------------------------------------------------------------------

func TestBlockSizeIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name      string
		blockSize uint32
	}{
		{"below the 1 MB minimum", 512 << 10},
		{"above the 256 MB maximum", 512 << 20},
		{"not a multiple of 512", (2 << 20) + 1},
		{"zero", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := validMeta()
			b.put(offFileParams, u32(tc.blockSize))
			if _, err := b.parse(); err == nil {
				t.Fatalf("accepted a block size of %d", tc.blockSize)
			}
		})
	}
}

func TestLogicalSectorSizeIsValidated(t *testing.T) {
	// The format permits exactly two values. Anything else would silently
	// change how every offset in the image is interpreted.
	for _, size := range []uint32{0, 256, 1024, 2048, 8192} {
		b := validMeta()
		b.put(offSectorSize, u32(size))
		if _, err := b.parse(); err == nil {
			t.Errorf("accepted a logical sector size of %d", size)
		}
	}

	for _, size := range []uint32{512, 4096} {
		b := validMeta()
		b.put(offSectorSize, u32(size))
		vals, err := b.parse()
		if err != nil {
			t.Errorf("rejected the permitted logical sector size %d: %v", size, err)
			continue
		}
		if vals.LogicalSectorSize != size {
			t.Errorf("LogicalSectorSize = %d, want %d", vals.LogicalSectorSize, size)
		}
	}
}

func TestFileParameterFlagsSelectTheDiskType(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags uint32
		want  types.DiskType
	}{
		{"neither flag set is dynamic", 0, types.DiskTypeDynamic},
		{"LeaveBlocksAllocated alone is fixed", 0x1, types.DiskTypeFixed},
		{"HasParent is differencing", 0x2, types.DiskTypeDifferential},
		{"HasParent wins over LeaveBlocksAllocated", 0x3, types.DiskTypeDifferential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := validMeta()
			b.put(offFileParams+4, u32(tc.flags))
			vals, err := b.parse()
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if vals.DiskType != tc.want {
				t.Fatalf("DiskType = %v, want %v", vals.DiskType, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Parent locator
// ---------------------------------------------------------------------------

// utf16LE encodes a string the way the parent locator stores keys and values.
func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(out[i*2:], u)
	}
	return out
}

// putParentLocator writes a parent locator item holding the given key/value
// pairs, and returns its total length.
func (b *metaBuilder) putParentLocator(offset uint32, locatorType [16]byte, pairs ...[2]string) uint32 {
	copy(b.region[offset:offset+16], locatorType[:])
	binary.LittleEndian.PutUint16(b.region[offset+18:], uint16(len(pairs)))

	// Descriptors follow the header; string data follows the descriptors.
	dataOff := uint32(parentLocatorHeaderSize + len(pairs)*parentLocatorEntrySize)
	for i, kv := range pairs {
		key, val := utf16LE(kv[0]), utf16LE(kv[1])
		desc := offset + uint32(parentLocatorHeaderSize) + uint32(i*parentLocatorEntrySize)

		binary.LittleEndian.PutUint32(b.region[desc:], dataOff)
		copy(b.region[offset+dataOff:], key)
		dataOff += uint32(len(key))

		binary.LittleEndian.PutUint32(b.region[desc+4:], dataOff)
		copy(b.region[offset+dataOff:], val)
		dataOff += uint32(len(val))

		binary.LittleEndian.PutUint16(b.region[desc+8:], uint16(len(key)))
		binary.LittleEndian.PutUint16(b.region[desc+10:], uint16(len(val)))
	}
	return dataOff
}

func TestParentLocatorYieldsPathsAndIdentity(t *testing.T) {
	// parent_linkage is what makes a VHDX parent verifiable. Without it a
	// parent can only be checked by virtual size, which any same-sized image
	// satisfies -- which is how a wrong reconstruction of a disk gets built.
	const guid = "{6b9f4b11-8e2a-4c3d-9f10-1122334455aa}"

	b := validMeta()
	size := b.putParentLocator(offParentLoc, types.ParentLocatorTypeVHDX,
		[2]string{"relative_path", `..\base.vhdx`},
		[2]string{"absolute_win32_path", `C:\vm\base.vhdx`},
		[2]string{"parent_linkage", guid},
	)
	b.addEntry(types.MetadataItemParentLocator, offParentLoc, size, false)

	vals, err := b.parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if vals.ParentFilename != `..\base.vhdx` {
		t.Errorf("ParentFilename = %q, want the first path listed", vals.ParentFilename)
	}
	if len(vals.ParentLocators) != 2 {
		t.Fatalf("ParentLocators has %d entries, want 2 path candidates", len(vals.ParentLocators))
	}
	// parent_linkage is identity, not a path. Listing it among the candidates
	// would send a resolver looking for a file named after a GUID.
	for _, loc := range vals.ParentLocators {
		if loc.Key == "parent_linkage" {
			t.Error("parent_linkage was recorded as a path candidate")
		}
	}

	if vals.ParentIdentifier == ([16]byte{}) {
		t.Fatal("ParentIdentifier is the zero GUID; parent_linkage was not parsed")
	}
}

func TestUnsupportedParentLocatorTypeIsRejected(t *testing.T) {
	// The keys are only meaningful under the one locator type the format
	// defines. Reading another type's entries as paths would invent a parent.
	b := validMeta()
	size := b.putParentLocator(offParentLoc, [16]byte{0x99},
		[2]string{"relative_path", `..\base.vhdx`},
	)
	b.addEntry(types.MetadataItemParentLocator, offParentLoc, size, false)

	_, err := b.parse()
	if err == nil {
		t.Fatal("accepted a parent locator of an unrecognised type")
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("error does not name the locator type: %v", err)
	}
}

func TestMalformedParentLinkageIsRejected(t *testing.T) {
	// A parent_linkage that does not parse as a GUID must fail rather than
	// leave ParentIdentifier zeroed, which is indistinguishable from the key
	// being absent and would silently disable verification.
	b := validMeta()
	size := b.putParentLocator(offParentLoc, types.ParentLocatorTypeVHDX,
		[2]string{"parent_linkage", "not-a-guid"},
	)
	b.addEntry(types.MetadataItemParentLocator, offParentLoc, size, false)

	if _, err := b.parse(); err == nil {
		t.Fatal("accepted a malformed parent_linkage value")
	}
}

func TestParentLocatorEntryCountIsBounded(t *testing.T) {
	// A crafted count of 65535 with maximal key and value lengths would drive
	// gigabytes of reads and allocations out of a few bytes of input.
	b := validMeta()
	copy(b.region[offParentLoc:offParentLoc+16], types.ParentLocatorTypeVHDX[:])
	binary.LittleEndian.PutUint16(b.region[offParentLoc+18:], 0xFFFF)
	b.addEntry(types.MetadataItemParentLocator, offParentLoc, 64, false)

	if _, err := b.parse(); err == nil {
		t.Fatal("accepted a parent locator declaring more entries than its item can hold")
	}
}

func TestParentLocatorStringsMustLieInsideTheItem(t *testing.T) {
	b := validMeta()
	copy(b.region[offParentLoc:offParentLoc+16], types.ParentLocatorTypeVHDX[:])
	binary.LittleEndian.PutUint16(b.region[offParentLoc+18:], 1)

	desc := offParentLoc + parentLocatorHeaderSize
	binary.LittleEndian.PutUint32(b.region[desc:], 0x10000) // key offset, far outside
	binary.LittleEndian.PutUint32(b.region[desc+4:], 0x10000)
	binary.LittleEndian.PutUint16(b.region[desc+8:], 16)
	binary.LittleEndian.PutUint16(b.region[desc+10:], 16)

	b.addEntry(types.MetadataItemParentLocator, offParentLoc, 128, false)

	if _, err := b.parse(); err == nil {
		t.Fatal("accepted a parent locator entry pointing outside its own item")
	}
}
