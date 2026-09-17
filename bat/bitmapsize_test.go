// SPDX-License-Identifier: MIT

package bat

import "testing"

// Every allocated block of a dynamic or differencing VHD is preceded by a sector
// bitmap holding one bit per 512-byte sector. Getting its size wrong is not a
// rounding error: the bitmap sits between the BAT entry's target and the block
// payload, so an incorrect size shifts every read of that block.

func TestSectorBitmapIsNeverZero(t *testing.T) {
	// A block smaller than 4096 bytes needs fewer than eight bits of bitmap, so
	// the unpadded arithmetic yields zero. A zero-sized bitmap would put each
	// block's data one sector early -- the parser would read the bitmap itself
	// as payload -- and would leave a differencing disk with no sector-presence
	// information, so every sector would appear to come from the parent.
	for _, blockSize := range []uint32{1, 512, 1024, 2048, 4095} {
		if got := vhdSectorBitmapSize(blockSize); got == 0 {
			t.Errorf("vhdSectorBitmapSize(%d) = 0; the bitmap is at least one sector", blockSize)
		}
	}
}

func TestSectorBitmapIsSectorAligned(t *testing.T) {
	// The bitmap is padded to a whole sector so the payload that follows starts
	// on a sector boundary, which is what the BAT entry's 512-byte units assume.
	for _, blockSize := range []uint32{512, 2048, 4096, 8192, 64 << 10, 1 << 20, 2 << 20, 32 << 20} {
		got := vhdSectorBitmapSize(blockSize)
		if got%512 != 0 {
			t.Errorf("vhdSectorBitmapSize(%d) = %d, which is not a multiple of 512", blockSize, got)
		}
	}
}

func TestSectorBitmapCoversEverySector(t *testing.T) {
	// The bitmap must hold at least one bit per sector of the block. Truncating
	// division would leave the final sectors of a block with no bit at all, and
	// on a differencing disk an absent bit reads as "not present here", so those
	// sectors would be served from the parent even after the child wrote them.
	for _, blockSize := range []uint32{512, 2048, 4096, 5000, 1 << 20, 2 << 20, (2 << 20) + 512} {
		sectors := (blockSize + 511) / 512
		bits := uint64(vhdSectorBitmapSize(blockSize)) * 8
		if bits < uint64(sectors) {
			t.Errorf("vhdSectorBitmapSize(%d) gives %d bits for %d sectors", blockSize, bits, sectors)
		}
	}
}

func TestSectorBitmapMatchesKnownSizes(t *testing.T) {
	// The two block sizes that matter in practice. Hyper-V writes 2 MB; older
	// tools and qemu write 1 MB or smaller.
	for _, tc := range []struct {
		blockSize uint32
		want      uint32
	}{
		{512, 512},       // floored
		{2048, 512},      // floored
		{4096, 512},      // exactly one bit-byte, padded to a sector
		{1 << 20, 512},   // 256 bytes of bitmap, padded to a sector
		{2 << 20, 512},   // 512 bytes of bitmap exactly
		{4 << 20, 1024},  // 1024 bytes, already sector-aligned
		{32 << 20, 8192}, // 8192 bytes
	} {
		if got := vhdSectorBitmapSize(tc.blockSize); got != tc.want {
			t.Errorf("vhdSectorBitmapSize(%d) = %d, want %d", tc.blockSize, got, tc.want)
		}
	}
}
