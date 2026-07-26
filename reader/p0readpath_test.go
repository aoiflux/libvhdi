// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// ============================================================================
// P0-3: reads must not run past the end of the virtual disk
// ============================================================================

// TestFixedVHD_ReadAtDoesNotLeakFooter checks that a read straddling the end of
// the virtual disk is clamped to the disk size. A fixed VHD stores its 512-byte
// footer immediately after the payload, so an unclamped read hands the caller
// footer bytes as if they were disk contents.
func TestFixedVHD_ReadAtDoesNotLeakFooter(t *testing.T) {
	const mediaSize = 4096

	img := buildFixedVHD(mediaSize, func(payload []byte) {
		copy(payload, repeatByte(0xCC, mediaSize))
	})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	// Read 1024 bytes starting 512 bytes before the end of the virtual disk.
	// Only the first 512 may be returned.
	buf := make([]byte, 1024)
	n, err := d.ReadAt(buf, mediaSize-512)

	if n != 512 {
		t.Errorf("ReadAt returned n = %d, want 512 (clamped to disk size)", n)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt error = %v, want io.EOF at end of disk", err)
	}
	if bytes.Contains(buf, []byte(types.VHDFooterSignature)) {
		t.Errorf("ReadAt leaked the VHD footer signature into the data stream")
	}
	if !bytes.Equal(buf[:512], repeatByte(0xCC, 512)) {
		t.Errorf("payload bytes corrupted before the boundary")
	}
	for i, b := range buf[512:] {
		if b != 0 {
			t.Fatalf("byte %d past end of disk = %#x, want untouched/zero", 512+i, b)
			break
		}
	}
}

// TestDynamicVHD_ReadAtClampsUnalignedTail covers a virtual size that is not a
// whole multiple of the block size, where the final block extends past the end
// of the device.
func TestDynamicVHD_ReadAtClampsUnalignedTail(t *testing.T) {
	const (
		blockSize = 2 * 1024 * 1024
		// One and a half blocks: the tail of block 1 is beyond the device.
		mediaSize = uint64(blockSize + blockSize/2)
	)

	img := buildDynamicVHD(types.DiskTypeDynamic, mediaSize, blockSize, []vhdBlock{
		{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x11, blockSize)},
		{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x22, blockSize)},
	}, "", [16]byte{})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	if d.Size() != mediaSize {
		t.Fatalf("Size() = %d, want %d", d.Size(), mediaSize)
	}

	// Straddle the end of the device by one sector.
	buf := make([]byte, 1024)
	n, err := d.ReadAt(buf, int64(mediaSize)-512)
	if n != 512 {
		t.Errorf("ReadAt returned n = %d, want 512", n)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt error = %v, want io.EOF", err)
	}
}

// ============================================================================
// P0-2: a differencing disk with no parent wired must fail closed
// ============================================================================

// TestDifferencingVHD_UnwiredParentFailsClosed verifies that reading a
// differencing disk before a parent is attached reports an error rather than
// silently returning zeroes. Zeroes are indistinguishable from real disk
// contents, which makes a silent fallback unusable for forensic work.
func TestDifferencingVHD_UnwiredParentFailsClosed(t *testing.T) {
	const (
		blockSize = 2 * 1024 * 1024
		mediaSize = uint64(blockSize)
	)

	// Block 0 unallocated in the child: its contents live in the parent.
	img := buildDynamicVHD(types.DiskTypeDifferential, mediaSize, blockSize, []vhdBlock{
		{allocated: false},
	}, "parent.vhd", [16]byte{0xAB})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	if !d.IsDifferencing() {
		t.Fatal("IsDifferencing() = false, want true")
	}
	if !d.NeedsParent() {
		t.Error("NeedsParent() = false, want true before SetParent")
	}

	buf := make([]byte, 512)
	_, err = d.ReadAt(buf, 0)
	if err == nil {
		t.Fatal("ReadAt on an unwired differencing disk succeeded; want ErrParentRequired")
	}
	if !errors.Is(err, ErrParentRequired) {
		t.Fatalf("ReadAt error = %v, want ErrParentRequired", err)
	}
}

// ============================================================================
// P0-1: sector-level parent fallthrough
// ============================================================================

// TestDifferencingVHD_SectorLevelParentFallthrough is the core differencing
// correctness case. In a VHD differencing disk an *allocated* block still
// carries a sector bitmap: sectors whose bit is set live in the child, and
// sectors whose bit is clear must be served from the parent. Resolving at
// whole-block granularity returns the child's zero-filled holes instead of the
// parent's data.
func TestDifferencingVHD_SectorLevelParentFallthrough(t *testing.T) {
	const (
		blockSize   = 2 * 1024 * 1024
		mediaSize   = uint64(blockSize)
		sectorSize  = 512
		testSectors = 6
	)

	parentID := [16]byte{0xAB, 0xCD}

	// Parent holds 0xA0 in every sector.
	parentImg := buildDynamicVHD(types.DiskTypeDynamic, mediaSize, blockSize, []vhdBlock{
		{allocated: true, presentSectors: allSectors(blockSize / sectorSize), data: repeatByte(0xA0, blockSize)},
	}, "", [16]byte{})

	// Child block 0 is allocated but only sectors 0, 2 and 4 were ever written.
	// On disk, the child's unwritten sectors are zero — exactly why they must
	// not be served from the child.
	childData := make([]byte, blockSize)
	for _, s := range []int{0, 2, 4} {
		copy(childData[s*sectorSize:(s+1)*sectorSize], repeatByte(0xC0, sectorSize))
	}
	childImg := buildDynamicVHD(types.DiskTypeDifferential, mediaSize, blockSize, []vhdBlock{
		{allocated: true, presentSectors: []int{0, 2, 4}, data: childData},
	}, "parent.vhd", parentID)

	parent, err := OpenVHD(bytes.NewReader(parentImg), int64(len(parentImg)))
	if err != nil {
		t.Fatalf("OpenVHD(parent): %v", err)
	}
	defer parent.Close()

	child, err := OpenVHD(bytes.NewReader(childImg), int64(len(childImg)))
	if err != nil {
		t.Fatalf("OpenVHD(child): %v", err)
	}
	defer child.Close()

	if err := child.SetParent(parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}

	buf := make([]byte, testSectors*sectorSize)
	if _, err := child.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	for s := 0; s < testSectors; s++ {
		got := buf[s*sectorSize : (s+1)*sectorSize]
		var want byte
		switch s {
		case 0, 2, 4:
			want = 0xC0 // present in child
		default:
			want = 0xA0 // absent from child, must come from parent
		}
		if !bytes.Equal(got, repeatByte(want, sectorSize)) {
			t.Errorf("sector %d = %#x..., want fill %#x", s, got[:4], want)
		}
	}
}

// TestVHDSectorBitmapBitOrderIsMSBFirst pins the bit order of the VHD block
// bitmap. The VHD specification numbers sectors from the most significant bit
// of each byte, matching the reference libvhdi C implementation and qemu's vpc
// driver. Getting this backwards silently swaps which sectors resolve to the
// parent.
func TestVHDSectorBitmapBitOrderIsMSBFirst(t *testing.T) {
	const (
		blockSize  = 2 * 1024 * 1024
		mediaSize  = uint64(blockSize)
		sectorSize = 512
	)

	parentImg := buildDynamicVHD(types.DiskTypeDynamic, mediaSize, blockSize, []vhdBlock{
		{allocated: true, presentSectors: allSectors(blockSize / sectorSize), data: repeatByte(0xA0, blockSize)},
	}, "", [16]byte{})

	// Only sector 0 is present in the child. MSB-first, that is bitmap byte 0
	// == 0x80. If the reader treats the bitmap as LSB-first it will read bit 0
	// (value 0) and wrongly send sector 0 to the parent, while sector 7 (bit 7,
	// value 1) wrongly resolves to the child.
	childData := make([]byte, blockSize)
	copy(childData[0:sectorSize], repeatByte(0xC0, sectorSize))
	childImg := buildDynamicVHD(types.DiskTypeDifferential, mediaSize, blockSize, []vhdBlock{
		{allocated: true, presentSectors: []int{0}, data: childData},
	}, "parent.vhd", [16]byte{0xAB})

	parent, err := OpenVHD(bytes.NewReader(parentImg), int64(len(parentImg)))
	if err != nil {
		t.Fatalf("OpenVHD(parent): %v", err)
	}
	defer parent.Close()
	child, err := OpenVHD(bytes.NewReader(childImg), int64(len(childImg)))
	if err != nil {
		t.Fatalf("OpenVHD(child): %v", err)
	}
	defer child.Close()
	if err := child.SetParent(parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}

	buf := make([]byte, 8*sectorSize)
	if _, err := child.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	if got := buf[0]; got != 0xC0 {
		t.Errorf("sector 0 = %#x, want 0xC0 from child (bitmap MSB-first)", got)
	}
	if got := buf[7*sectorSize]; got != 0xA0 {
		t.Errorf("sector 7 = %#x, want 0xA0 from parent (bitmap MSB-first)", got)
	}
}
