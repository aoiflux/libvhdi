// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// batSectorWithBlock0 returns the 4 KB sector at the start of the BAT region as
// it should look after replay: block 0 fully present at the payload block.
//
// The on-disk BAT in these fixtures marks block 0 NOT_PRESENT, so a read only
// returns the payload if the log entry was actually applied.
func batSectorWithBlock0(p vhdxParams) []byte {
	sector := make([]byte, logSectorSize)

	chunkRatio := int((1 << 23) * uint64(p.sectorSize) / uint64(p.blockSize))

	// Block 0: state 6 (fully present) at the payload block.
	binary.LittleEndian.PutUint64(sector[0:8],
		uint64(vhdxPayloadBlockOff/mb)<<20|uint64(types.BlockStateFullyAllocated))

	// Preserve the sector bitmap entry if it happens to fall in this sector.
	if off := chunkRatio * 8; off+8 <= logSectorSize {
		binary.LittleEndian.PutUint64(sector[off:off+8], 0)
	}
	return sector
}

// TestVHDXLogReplayChangesDecodedContents is the end-to-end replay case. The
// on-disk BAT says block 0 is not present, so the disk reads as zeroes. The log
// holds a journalled write making block 0 fully present. Replaying it must change
// what the disk returns — and must not touch the file.
func TestVHDXLogReplayChangesDecodedContents(t *testing.T) {
	p := vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateNone, // on-disk: block 0 absent
		payload:         repeatByte(0xD7, vhdxDiffBlockSize),
	}
	p.logWrites = []logWrite{{fileOffset: vhdxBATRegionOff, data: batSectorWithBlock0(p)}}

	img := buildVHDX(p)
	original := append([]byte(nil), img...)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	if !d.HasLog() {
		t.Error("HasLog() = false, want true")
	}
	if !d.LogReplayed() {
		t.Fatal("LogReplayed() = false; the log entry should have been replayed")
	}
	if d.IsDirty() {
		t.Error("IsDirty() = true after a successful replay")
	}

	stats, ok := d.LogReplayStats()
	if !ok {
		t.Fatal("LogReplayStats() reported no replay")
	}
	if stats.Entries != 1 || stats.Descriptors != 1 || stats.Sectors != 1 {
		t.Errorf("stats = %+v, want 1 entry / 1 descriptor / 1 sector", stats)
	}
	if stats.FirstSequence != 1 || stats.LastSequence != 1 {
		t.Errorf("sequence range = (%d, %d), want (1, 1)", stats.FirstSequence, stats.LastSequence)
	}

	// The replayed BAT makes block 0 present, so the payload is now readable.
	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, repeatByte(0xD7, 512)) {
		t.Errorf("block 0 = %#x..., want 0xD7 via the replayed BAT entry", buf[:4])
	}

	// Replay is read-only: the backing bytes must be untouched.
	if !bytes.Equal(img, original) {
		t.Error("replay modified the image buffer; it must be read-only")
	}
}

// TestVHDXWithoutReplayReadsStaleState confirms the test above is meaningful: the
// same image opened without replay returns the pre-log state.
func TestVHDXWithoutReplayReadsStaleState(t *testing.T) {
	p := vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateNone,
		payload:         repeatByte(0xD7, vhdxDiffBlockSize),
	}
	p.logWrites = []logWrite{{fileOffset: vhdxBATRegionOff, data: batSectorWithBlock0(p)}}
	img := buildVHDX(p)

	// Corrupt the log entry's checksum so replay cannot proceed, then open with
	// AllowDirtyImage to read the un-replayed state.
	img[vhdxLogRegionOff+100] ^= 0xFF

	d, err := Open(bytes.NewReader(img), &Options{AllowDirtyImage: true})
	if err != nil {
		t.Fatalf("Open with AllowDirtyImage: %v", err)
	}
	defer d.Close()

	if !d.HasLog() {
		t.Error("HasLog() = false, want true")
	}
	if d.LogReplayed() {
		t.Error("LogReplayed() = true for a corrupt log entry")
	}
	if !d.IsDirty() {
		t.Error("IsDirty() = false for an un-replayed log")
	}

	// Without replay the BAT still marks block 0 absent, so the disk reads zeroes
	// even though the payload bytes are present in the file.
	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, make([]byte, 512)) {
		t.Errorf("block 0 = %#x..., want zeroes from the stale BAT", buf[:4])
	}
}

// TestVHDXUnreplayableLogIsRefusedByDefault covers the default policy: a log that
// cannot be replayed is refused rather than read as though clean.
func TestVHDXUnreplayableLogIsRefusedByDefault(t *testing.T) {
	p := vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateNone,
		payload:         repeatByte(0xD7, vhdxDiffBlockSize),
	}
	p.logWrites = []logWrite{{fileOffset: vhdxBATRegionOff, data: batSectorWithBlock0(p)}}
	img := buildVHDX(p)
	img[vhdxLogRegionOff+100] ^= 0xFF

	if _, err := OpenVHDX(bytes.NewReader(img), int64(len(img))); !errors.Is(err, ErrDirtyImage) {
		t.Fatalf("OpenVHDX error = %v, want ErrDirtyImage", err)
	}
}

// TestVHDXLogReplayThroughOpenFile checks replay happens on the path a caller
// actually uses, not only via OpenVHDX.
func TestVHDXLogReplayThroughOpenFile(t *testing.T) {
	p := vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateNone,
		payload:         repeatByte(0xD7, vhdxDiffBlockSize),
	}
	p.logWrites = []logWrite{{fileOffset: vhdxBATRegionOff, data: batSectorWithBlock0(p)}}

	path := writeTempImage(t, "logged.vhdx", buildVHDX(p))

	d, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if !d.LogReplayed() {
		t.Fatal("LogReplayed() = false via OpenFile")
	}

	buf := make([]byte, 512)
	if _, err := d.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if buf[0] != 0xD7 {
		t.Errorf("block 0 = %#x, want 0xD7", buf[0])
	}
}

// TestVHDXCleanImageReportsNoLog guards against flagging every image.
func TestVHDXCleanImageReportsNoLog(t *testing.T) {
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

	if d.HasLog() {
		t.Error("HasLog() = true for a clean image")
	}
	if d.LogReplayed() {
		t.Error("LogReplayed() = true for a clean image")
	}
	if d.IsDirty() {
		t.Error("IsDirty() = true for a clean image")
	}
	if _, ok := d.LogReplayStats(); ok {
		t.Error("LogReplayStats() reported a replay for a clean image")
	}
}

// TestVHDXLogReplayRewritesMetadata covers a log entry targeting the metadata
// region, which changes the disk's reported geometry rather than its contents.
func TestVHDXLogReplayRewritesMetadata(t *testing.T) {
	p := vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
	}

	// Build the image once to capture its metadata sector, then journal a version
	// with a smaller virtual disk size.
	base := buildVHDX(p)

	const metaItemSector = 0x1000 // the sector holding the metadata item payloads
	sector := append([]byte(nil), base[vhdxMetaRegionOff+metaItemSector:vhdxMetaRegionOff+metaItemSector+logSectorSize]...)

	// Virtual Disk Size item lives at 0x1010, i.e. offset 0x10 within this sector.
	const shrunk = uint64(4096) * mb
	binary.LittleEndian.PutUint64(sector[0x10:0x18], shrunk)

	p.logWrites = []logWrite{{
		fileOffset: uint64(vhdxMetaRegionOff + metaItemSector),
		data:       sector,
	}}
	img := buildVHDX(p)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	if !d.LogReplayed() {
		t.Fatal("LogReplayed() = false")
	}
	if d.Size() != shrunk {
		t.Errorf("Size() = %d, want %d from the replayed metadata", d.Size(), shrunk)
	}
}
