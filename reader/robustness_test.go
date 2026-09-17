// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/aoiflux/libvhdi/block"
	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

// ============================================================================
// Helpers for corrupting otherwise-valid images
// ============================================================================

// patchVHDDynHeader mutates the dynamic disk header in place and repairs its
// checksum, so the parser reaches the field under test instead of rejecting the
// image for a bad checksum.
func patchVHDDynHeader(img []byte, mutate func(h []byte)) {
	h := img[vhdFooterLen : vhdFooterLen+vhdDynHeaderLen]
	mutate(h)
	binary.BigEndian.PutUint32(h[36:40], 0)
	binary.BigEndian.PutUint32(h[36:40], vhdSpecChecksum(h))
}

// patchVHDFooter mutates both footer copies in place and repairs their
// checksums.
func patchVHDFooter(img []byte, mutate func(f []byte)) {
	for _, off := range []int{0, len(img) - vhdFooterLen} {
		f := img[off : off+vhdFooterLen]
		if !bytes.Equal(f[0:8], []byte(types.VHDFooterSignature)) {
			continue
		}
		mutate(f)
		binary.BigEndian.PutUint32(f[64:68], 0)
		binary.BigEndian.PutUint32(f[64:68], vhdSpecChecksum(f))
	}
}

func validDynamicVHD() []byte {
	return buildDynamicVHD(types.DiskTypeDynamic, chainMediaSize, chainBlockSize, []vhdBlock{
		{allocated: true, presentSectors: allSectors(chainBlockSize / 512), data: repeatByte(0x11, chainBlockSize)},
	}, "", [16]byte{})
}

// ============================================================================
// Malformed input must error, never panic or exhaust memory
// ============================================================================

// TestVHDXMissingFileParametersErrors covers a metadata table lacking the
// required File Parameters item. Block size is then zero, and the block count
// calculation divides by it.
func TestVHDXMissingFileParametersErrors(t *testing.T) {
	img := buildVHDX(vhdxParams{
		blockSize:          vhdxDiffBlockSize,
		sectorSize:         vhdxDiffSectorSize,
		virtualDiskSize:    vhdxDiffVirtual,
		blockState:         types.BlockStateFullyAllocated,
		omitFileParameters: true,
	})

	_, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err == nil {
		t.Fatal("OpenVHDX accepted a VHDX with no File Parameters metadata item")
	}
	t.Logf("rejected as expected: %v", err)
}

// TestVHDXMissingVirtualDiskSizeErrors covers the other required item.
func TestVHDXMissingVirtualDiskSizeErrors(t *testing.T) {
	img := buildVHDX(vhdxParams{
		blockSize:           vhdxDiffBlockSize,
		sectorSize:          vhdxDiffSectorSize,
		virtualDiskSize:     vhdxDiffVirtual,
		blockState:          types.BlockStateFullyAllocated,
		omitVirtualDiskSize: true,
	})

	if _, err := OpenVHDX(bytes.NewReader(img), int64(len(img))); err == nil {
		t.Fatal("OpenVHDX accepted a VHDX with no Virtual Disk Size metadata item")
	}
}

// TestVHDHugeBlockCountIsRejected covers an unbounded allocation driven by an
// attacker-controlled field. A NumberOfBlocks of 0xFFFFFFFF would size the BAT
// buffer at 16 GB; on a 32-bit platform the multiplication also overflows.
func TestVHDHugeBlockCountIsRejected(t *testing.T) {
	img := validDynamicVHD()
	patchVHDDynHeader(img, func(h []byte) {
		binary.BigEndian.PutUint32(h[28:32], 0xFFFFFFFF)
	})

	_, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err == nil {
		t.Fatal("OpenVHD accepted a block count of 0xFFFFFFFF")
	}
	t.Logf("rejected as expected: %v", err)
}

// TestVHDBlockCountInconsistentWithMediaSizeIsRejected checks the BAT is
// cross-checked against the disk geometry rather than trusted outright.
func TestVHDBlockCountInconsistentWithMediaSizeIsRejected(t *testing.T) {
	img := validDynamicVHD()
	patchVHDDynHeader(img, func(h []byte) {
		// One block for a disk that needs one: inflate to a million.
		binary.BigEndian.PutUint32(h[28:32], 1_000_000)
	})

	if _, err := OpenVHD(bytes.NewReader(img), int64(len(img))); err == nil {
		t.Fatal("OpenVHD accepted a block count inconsistent with the media size")
	}
}

// TestVHDBATOffsetBeyondEOFIsRejected covers a BAT pointer outside the file.
func TestVHDBATOffsetBeyondEOFIsRejected(t *testing.T) {
	img := validDynamicVHD()
	patchVHDDynHeader(img, func(h []byte) {
		binary.BigEndian.PutUint64(h[16:24], 1<<40)
	})

	if _, err := OpenVHD(bytes.NewReader(img), int64(len(img))); err == nil {
		t.Fatal("OpenVHD accepted a BAT offset beyond the end of the file")
	}
}

// TestVHDMediaSizeBeyondFileIsRejectedForFixed covers a fixed disk claiming more
// payload than the file physically holds.
func TestVHDMediaSizeBeyondFileIsRejectedForFixed(t *testing.T) {
	img := buildFixedVHD(4096, nil)
	patchVHDFooter(img, func(f []byte) {
		binary.BigEndian.PutUint64(f[40:48], 1<<40)
	})

	if _, err := OpenVHD(bytes.NewReader(img), int64(len(img))); err == nil {
		t.Fatal("OpenVHD accepted a fixed disk larger than its backing file")
	}
}

// TestVHDXHugeVirtualDiskSizeIsRejected covers the VHDX BAT allocation, which is
// sized from the virtual disk size and block size.
func TestVHDXHugeVirtualDiskSizeIsRejected(t *testing.T) {
	img := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: 1 << 62, // 4 exabytes: the BAT alone would be terabytes
		blockState:      types.BlockStateFullyAllocated,
	})

	if _, err := OpenVHDX(bytes.NewReader(img), int64(len(img))); err == nil {
		t.Fatal("OpenVHDX accepted a virtual disk size of 2^62")
	}
}

// TestBlockReadersRejectZeroBlockSize covers the exported block reader
// constructors, which divide by the BAT's block size on every read.
func TestBlockReadersRejectZeroBlockSize(t *testing.T) {
	// A zero block size reaching a block reader must not divide by zero.
	vhdBAT := &types.VHDBlockAllocationTable{
		NumberOfEntries: 1,
		BlockSize:       0,
		Entries:         []types.BlockAllocationEntry{{IsAllocated: false}},
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("VHD block reader panicked on zero block size: %v", r)
		}
	}()

	buf := make([]byte, 512)

	vhdReader := block.NewVHDBlockReader(bytes.NewReader(make([]byte, 4096)), vhdBAT, 512)
	if _, err := vhdReader.ReadAt(buf, 0); err == nil {
		t.Error("VHD ReadAt on a zero-block-size BAT succeeded; want an error")
	}

	vhdxBAT := &types.VHDXBlockAllocationTable{
		NumberOfEntries: 1,
		BlockSize:       0,
		Entries:         []types.BlockAllocationEntry{{IsAllocated: false}},
	}
	vhdxReader := block.NewVHDXBlockReader(bytes.NewReader(make([]byte, 4096)), vhdxBAT, 512)
	if _, err := vhdxReader.ReadAt(buf, 0); err == nil {
		t.Error("VHDX ReadAt on a zero-block-size BAT succeeded; want an error")
	}
}

// TestVHDXZeroChunkRatioDoesNotPanic covers the sector bitmap path, which
// divides by the BAT's chunk ratio.
func TestVHDXZeroChunkRatioDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked on zero chunk ratio: %v", r)
		}
	}()

	vhdxBAT := &types.VHDXBlockAllocationTable{
		NumberOfEntries: 1,
		BlockSize:       mb,
		ChunkRatio:      0,
		SectorSize:      512,
		Entries: []types.BlockAllocationEntry{
			{IsAllocated: true, FileOffset: 0, BlockState: types.BlockStatePartiallyAllocated},
		},
	}
	br := block.NewVHDXBlockReader(bytes.NewReader(make([]byte, 2*mb)), vhdxBAT, 512)

	buf := make([]byte, 512)
	if _, err := br.ReadAt(buf, 0); err == nil {
		t.Error("ReadAt with a zero chunk ratio succeeded; want an error")
	}
}

// ============================================================================
// Dirty VHDX images
// ============================================================================

// TestVHDXDirtyLogIsRefused covers an image whose active header carries a
// non-zero log GUID. The log has not been replayed, so the BAT and metadata may
// be stale and the decoded contents cannot be trusted.
func TestVHDXDirtyLogIsRefused(t *testing.T) {
	img := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
		dirtyLog:        true,
	})

	_, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if !errors.Is(err, ErrDirtyImage) {
		t.Fatalf("OpenVHDX error = %v, want ErrDirtyImage", err)
	}
}

// TestVHDXDirtyLogOverride checks the documented escape hatch, which a forensic
// caller needs in order to inspect a dirty image at all.
func TestVHDXDirtyLogOverride(t *testing.T) {
	img := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
		dirtyLog:        true,
	})

	d, err := Open(bytes.NewReader(img), &Options{AllowDirtyImage: true})
	if err != nil {
		t.Fatalf("Open with AllowDirtyImage: %v", err)
	}
	defer d.Close()

	if !d.IsDirty() {
		t.Error("IsDirty() = false, want true for an image with an unreplayed log")
	}

	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if buf[0] != 0xA0 {
		t.Errorf("payload = %#x, want 0xA0", buf[0])
	}
}

// TestCleanVHDXIsNotDirty guards against flagging every image as dirty.
func TestCleanVHDXIsNotDirty(t *testing.T) {
	img := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
	})

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	if d.IsDirty() {
		t.Error("IsDirty() = true for a clean image")
	}
}

// ============================================================================
// Truncation and garbage
// ============================================================================

// TestTruncatedImagesError walks every truncation of a valid image. None may
// panic; all must either open or report an error.
func TestTruncatedImagesError(t *testing.T) {
	images := map[string][]byte{
		"fixedVHD":   buildFixedVHD(4096, nil),
		"dynamicVHD": validDynamicVHD(),
		"vhdx": buildVHDX(vhdxParams{
			blockSize:       vhdxDiffBlockSize,
			sectorSize:      vhdxDiffSectorSize,
			virtualDiskSize: vhdxDiffVirtual,
			blockState:      types.BlockStateFullyAllocated,
		}),
	}

	for name, img := range images {
		t.Run(name, func(t *testing.T) {
			// Step in irregular increments to cover structure boundaries
			// without running the full byte range of a multi-megabyte image.
			for _, n := range []int{0, 1, 8, 63, 64, 511, 512, 513, 1023, 1536, 4095, 65535, 65536, 196608, len(img) - 1} {
				if n < 0 || n > len(img) {
					continue
				}
				truncated := img[:n]

				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("panic on %s truncated to %d bytes: %v", name, n, r)
						}
					}()
					d, err := Open(bytes.NewReader(truncated), nil)
					if err == nil && d != nil {
						// A read must not panic either.
						buf := make([]byte, 4096)
						_, _ = d.ReadAt(buf, 0)
						d.Close()
					}
				}()
			}
		})
	}
}

// TestGarbageDoesNotPanic feeds structured garbage that keeps a valid signature
// so parsing proceeds deep into the format before failing.
func TestGarbageDoesNotPanic(t *testing.T) {
	patterns := [][]byte{
		bytes.Repeat([]byte{0x00}, 4096),
		bytes.Repeat([]byte{0xFF}, 4096),
		append([]byte(types.VHDXFileSignature), bytes.Repeat([]byte{0xFF}, 1<<20)...),
		append([]byte(types.VHDFooterSignature), bytes.Repeat([]byte{0xFF}, 4096)...),
	}

	for i, p := range patterns {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on garbage pattern %d: %v", i, r)
				}
			}()
			d, err := Open(bytes.NewReader(p), nil)
			if err == nil && d != nil {
				buf := make([]byte, 4096)
				_, _ = d.ReadAt(buf, 0)
				d.Close()
			}
		}()
	}
}

// ============================================================================
// Fuzzing
// ============================================================================

// FuzzOpen asserts the open path never panics and never allocates without
// bound, whatever bytes it is handed.
func FuzzOpen(f *testing.F) {
	f.Add(buildFixedVHD(4096, nil))
	f.Add(validDynamicVHD())
	f.Add(buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
	}))
	f.Add([]byte(types.VHDXFileSignature))
	f.Add([]byte(types.VHDFooterSignature))

	// Seeds for the footer recovery paths added in v0.3.0. Recovery code is
	// where a parser is most easily tricked, because it runs precisely when the
	// conformant structures have already failed -- so the fuzzer should start
	// from inputs that reach it rather than having to discover them.
	for _, seed := range recoverySeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := Open(bytes.NewReader(data), nil)
		if err != nil {
			return
		}
		defer d.Close()

		// Reads at assorted offsets must also stay panic-free.
		buf := make([]byte, 1024)
		for _, off := range []int64{0, 511, 512, 4096, int64(d.Size()) - 1, int64(d.Size())} {
			if off < 0 {
				continue
			}
			_, _ = d.ReadAt(buf, off)
		}
		_, _, _ = d.VirtualToFileOffset(0)
	})
}

// FuzzVHDFooter concentrates on the footer, which drives disk type and geometry
// selection, keeping the checksum valid so the fuzzer reaches the field
// validation rather than bouncing off the checksum.
func FuzzVHDFooter(f *testing.F) {
	f.Add(buildFixedVHD(4096, nil))
	f.Add(validDynamicVHD())

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < vhdFooterLen {
			return
		}
		img := append([]byte(nil), data...)

		// Repair the trailing footer's checksum if the signature is intact.
		f := img[len(img)-vhdFooterLen:]
		if bytes.Equal(f[0:8], []byte(types.VHDFooterSignature)) {
			binary.BigEndian.PutUint32(f[64:68], 0)
			binary.BigEndian.PutUint32(f[64:68], vhdSpecChecksum(f))
		}

		d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			return
		}
		defer d.Close()
		buf := make([]byte, 512)
		_, _ = d.ReadAt(buf, 0)
	})
}

// FuzzVHDXStructures overlays fuzzer-supplied bytes onto the header, region
// table and metadata region of an otherwise valid VHDX image, repairing the
// CRCs afterwards so the fuzzer explores field values rather than bouncing off
// checksum rejection.
//
// The overlay is deliberate: handing the fuzzer a multi-megabyte image as its
// corpus wastes nearly all of its budget mutating payload bytes that no parser
// reads. Seeding with small inputs and splicing them into the structures that
// actually drive allocation and arithmetic reaches the interesting code far
// more often.
func FuzzVHDXStructures(f *testing.F) {
	f.Add(make([]byte, 96))
	f.Add(bytes.Repeat([]byte{0xFF}, 96))
	f.Add(bytes.Repeat([]byte{0x01}, 192))

	template := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStatePartiallyAllocated,
		presentSectors:  []int{0, 2, 4},
		payload:         repeatByte(0xC0, vhdxDiffBlockSize),
		hasParent:       true,
	})

	// Regions of the image worth corrupting, in the order fuzz bytes are
	// consumed: header log fields, region table entries, metadata table header
	// and entries, and the start of the BAT.
	type span struct{ off, size int }
	spans := []span{
		{types.VHDXFirstHeaderOffset + 48, 32},  // log guid, version, size, offset
		{types.VHDXSecondHeaderOffset + 48, 32}, //
		{types.VHDXFirstRegionTableOffset + 8, 8},
		{types.VHDXFirstRegionTableOffset + 16, 64}, // two region entries
		{vhdxMetaRegionOff + 8, 8},                  // table header count
		{vhdxMetaRegionOff + 32, 160},               // metadata entries
		{vhdxMetaRegionOff + 0x1000, 64},            // item payloads
		{vhdxMetaRegionOff + 0x1100, 64},            // parent locator item
		{vhdxBATRegionOff, 64},                      // BAT entries
	}

	f.Fuzz(func(t *testing.T, seed []byte) {
		if len(seed) == 0 {
			return
		}

		img := append([]byte(nil), template...)

		// Spread the seed across the spans, cycling if it is short.
		pos := 0
		for _, s := range spans {
			for i := 0; i < s.size; i++ {
				if s.off+i >= len(img) {
					break
				}
				img[s.off+i] = seed[pos%len(seed)]
				pos++
			}
		}

		// Restore the signatures and CRCs the parser checks first, so the
		// fuzzer's bytes are evaluated as field values.
		copy(img[0:8], []byte(types.VHDXFileSignature))
		for _, off := range []int{types.VHDXFirstHeaderOffset, types.VHDXSecondHeaderOffset} {
			copy(img[off:off+4], []byte(types.VHDXHeaderSignature))
			binary.LittleEndian.PutUint16(img[off+66:off+68], 0x0001)
			finalizeImageHeaderCRC(img, int64(off))
		}
		for _, off := range []int{types.VHDXFirstRegionTableOffset, types.VHDXSecondRegionTableOffset} {
			copy(img[off:off+4], []byte(types.VHDXRegionSignature))
			finalizeRegionTableCRC(img, int64(off))
		}
		copy(img[vhdxMetaRegionOff:vhdxMetaRegionOff+8], []byte(types.VHDXMetadataSignature))

		// AllowDirtyImage keeps the log-guid check from short-circuiting the
		// parse, since the overlay frequently sets a non-zero log guid.
		d, err := Open(bytes.NewReader(img), &Options{AllowDirtyImage: true})
		if err != nil {
			return
		}
		defer d.Close()

		buf := make([]byte, 4096)
		for _, off := range []int64{0, 512, 4096, int64(d.Size()) - 512} {
			if off < 0 {
				continue
			}
			_, _ = d.ReadAt(buf, off)
		}
		_, _, _ = d.VirtualToFileOffset(0)
	})
}

// guard against the CRC helper drifting away from the VHDX polynomial.
func TestVHDXCRCPolynomial(t *testing.T) {
	// CRC-32C of "123456789" is a published check value.
	if got := binaryutil.CRC32([]byte("123456789")); got != 0xE3069283 {
		t.Errorf("CRC32 check value = %#x, want 0xE3069283 (CRC-32C)", got)
	}
}

// recoverySeeds returns images that exercise the footer recovery paths.
//
// Each one opens successfully today by a route other than the conformant
// trailing footer, which is what makes them useful starting points: a mutation
// of one is far more likely to reach the recovery code than a mutation of a
// random buffer.
func recoverySeeds() [][]byte {
	var out [][]byte

	// A 511-byte legacy footer, as Virtual PC wrote before the format was
	// documented.
	full := buildFixedVHD(4096, nil)
	out = append(out, append([]byte(nil), full[:len(full)-1]...))

	// A dynamic image whose trailing footer is destroyed, recoverable only from
	// the mirror at offset 0.
	mirrorOnly := validDynamicVHD()
	tail := mirrorOnly[len(mirrorOnly)-vhdFooterLen:]
	for i := range tail {
		tail[i] = 0xDB
	}
	out = append(out, mirrorOnly)

	// A fixed image carrying a valid footer at offset 0 as ordinary payload,
	// with its real footer destroyed. This must stay refused: offset 0 on a
	// fixed disk is data, not a mirror.
	decoy := buildFixedVHD(4096, nil)
	footerBytes := append([]byte(nil), decoy[4096:]...)
	trap := buildFixedVHD(4096, func(payload []byte) { copy(payload, footerBytes) })
	trapTail := trap[len(trap)-vhdFooterLen:]
	for i := range trapTail {
		trapTail[i] = 0xDB
	}
	out = append(out, trap)

	return out
}
