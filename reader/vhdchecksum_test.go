package reader

import (
	"bytes"
	"testing"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

// TestVHDSpecChecksumIsNotCRC32C documents that the two algorithms genuinely
// differ, so the tests below are not tautological.
func TestVHDSpecChecksumIsNotCRC32C(t *testing.T) {
	buf := repeatByte(0xA5, 512)
	if vhdSpecChecksum(buf) == binaryutil.CRC32(buf) {
		t.Fatal("one's-complement sum and CRC-32C agree; test fixture is meaningless")
	}
}

// TestOpenFixedVHD_SpecConformantChecksum opens a fixed VHD whose footer
// checksum is computed per the VHD specification (one's complement of the sum
// of the footer bytes). CRC-32C is a VHDX construct and must not be applied to
// VHD structures.
func TestOpenFixedVHD_SpecConformantChecksum(t *testing.T) {
	const mediaSize = 4096

	img := buildFixedVHD(mediaSize, func(payload []byte) {
		copy(payload, repeatByte(0xCC, mediaSize))
	})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD rejected a spec-conformant fixed VHD: %v", err)
	}
	defer d.Close()

	if d.Size() != mediaSize {
		t.Fatalf("Size() = %d, want %d", d.Size(), mediaSize)
	}
	if d.DiskType() != types.DiskTypeFixed {
		t.Fatalf("DiskType() = %d, want fixed", d.DiskType())
	}
}

// TestOpenDynamicVHD_SpecConformantChecksum covers the dynamic disk header,
// which the specification checksums the same way as the footer.
func TestOpenDynamicVHD_SpecConformantChecksum(t *testing.T) {
	const (
		blockSize = 2 * 1024 * 1024
		mediaSize = uint64(blockSize)
	)

	img := buildDynamicVHD(types.DiskTypeDynamic, mediaSize, blockSize, []vhdBlock{
		{allocated: true, presentSectors: allSectors(blockSize / 512), data: repeatByte(0x11, blockSize)},
	}, "", [16]byte{})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD rejected a spec-conformant dynamic VHD: %v", err)
	}
	defer d.Close()

	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, repeatByte(0x11, 512)) {
		t.Fatalf("ReadAt returned %#x..., want 0x11 fill", buf[:8])
	}
}

func allSectors(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}
