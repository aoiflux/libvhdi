package vhdxlog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
)

// ============================================================================
// Log entry builder
//
// Entries are assembled to the VHDX Format Specification so that a parser
// disagreeing with the spec fails rather than silently agreeing with itself.
// ============================================================================

type descriptor struct {
	// zero selects a zero descriptor; otherwise a data descriptor is written.
	zero bool

	fileOffset uint64

	// zeroLength is the run length for a zero descriptor.
	zeroLength uint64

	// data is the full SectorSize payload for a data descriptor.
	data []byte
}

// buildEntry assembles one log entry.
func buildEntry(logGUID [16]byte, sequenceNumber uint64, tail uint32, descs []descriptor) []byte {
	dataCount := 0
	for _, d := range descs {
		if !d.zero {
			dataCount++
		}
	}

	headerAndDescs := alignUp(entryHeaderSize+len(descs)*descriptorSize, SectorSize)
	entryLength := headerAndDescs + dataCount*SectorSize
	buf := make([]byte, entryLength)

	// Entry header.
	copy(buf[0:4], sigEntry)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(entryLength))
	binary.LittleEndian.PutUint32(buf[12:16], tail)
	binary.LittleEndian.PutUint64(buf[16:24], sequenceNumber)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(len(descs)))
	copy(buf[32:48], logGUID[:])
	binary.LittleEndian.PutUint64(buf[48:56], 0) // flushed file offset
	binary.LittleEndian.PutUint64(buf[56:64], 0) // last file offset

	dataIndex := 0
	for i, d := range descs {
		off := entryHeaderSize + i*descriptorSize
		slot := buf[off : off+descriptorSize]

		if d.zero {
			copy(slot[0:4], sigZero)
			binary.LittleEndian.PutUint64(slot[8:16], d.zeroLength)
			binary.LittleEndian.PutUint64(slot[16:24], d.fileOffset)
			binary.LittleEndian.PutUint64(slot[24:32], sequenceNumber)
			continue
		}

		// A data descriptor stashes the 8 leading and 4 trailing bytes of the
		// sector, because the writer overwrites those positions with the "data"
		// signature and the split sequence number.
		copy(slot[0:4], sigDesc)
		copy(slot[4:8], d.data[4092:4096]) // trailing bytes
		copy(slot[8:16], d.data[0:8])      // leading bytes
		binary.LittleEndian.PutUint64(slot[16:24], d.fileOffset)
		binary.LittleEndian.PutUint64(slot[24:32], sequenceNumber)

		sectorOff := headerAndDescs + dataIndex*SectorSize
		dataIndex++
		sector := buf[sectorOff : sectorOff+SectorSize]
		copy(sector, d.data)
		copy(sector[0:4], sigData)
		binary.LittleEndian.PutUint32(sector[4:8], uint32(sequenceNumber>>32))
		binary.LittleEndian.PutUint32(sector[4092:4096], uint32(sequenceNumber))
	}

	// Checksum last, over the whole entry with the field zeroed.
	binary.LittleEndian.PutUint32(buf[4:8], 0)
	binary.LittleEndian.PutUint32(buf[4:8], binaryutil.CRC32(buf))
	return buf
}

// logImage places a log region at logOffset within a file of the given size.
type logImage struct {
	data      []byte
	logOffset int64
	logSize   uint32
}

func newLogImage(fileSize int, logOffset int64, logSize uint32) *logImage {
	return &logImage{data: make([]byte, fileSize), logOffset: logOffset, logSize: logSize}
}

// place writes an entry at a region-relative offset, wrapping if needed.
func (li *logImage) place(regionOffset int64, entry []byte) {
	for i, b := range entry {
		at := (regionOffset + int64(i)) % int64(li.logSize)
		li.data[li.logOffset+at] = b
	}
}

func (li *logImage) reader() io.ReaderAt { return bytes.NewReader(li.data) }

func sectorOf(fill byte) []byte {
	s := make([]byte, SectorSize)
	for i := range s {
		s[i] = fill
	}
	return s
}

const (
	testFileSize  = 8 << 20
	testLogOffset = int64(1 << 20)
	testLogSize   = uint32(1 << 20)
)

var testGUID = [16]byte{0xAA, 0xBB, 0xCC, 0xDD, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

// ============================================================================
// Tests
// ============================================================================

func TestAnalyzeSingleDataDescriptor(t *testing.T) {
	const target = uint64(4 << 20)

	li := newLogImage(testFileSize, testLogOffset, testLogSize)
	li.place(0, buildEntry(testGUID, 1, 0, []descriptor{
		{fileOffset: target, data: sectorOf(0xAB)},
	}))

	rp, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if rp.EntryCount() != 1 {
		t.Errorf("EntryCount() = %d, want 1", rp.EntryCount())
	}
	if rp.DescriptorCount() != 1 {
		t.Errorf("DescriptorCount() = %d, want 1", rp.DescriptorCount())
	}
	if rp.SectorCount() != 1 {
		t.Errorf("SectorCount() = %d, want 1", rp.SectorCount())
	}

	// The overlay must present the reconstructed sector, including the leading
	// and trailing bytes the writer displaced.
	r := rp.Overlay(li.reader())
	got := make([]byte, SectorSize)
	if _, err := r.ReadAt(got, int64(target)); err != nil {
		t.Fatalf("overlay ReadAt: %v", err)
	}
	if !bytes.Equal(got, sectorOf(0xAB)) {
		t.Errorf("replayed sector = %x..%x, want all 0xAB", got[:8], got[4088:])
	}

	// Bytes outside the replayed sector still come from the file.
	before := make([]byte, 8)
	if _, err := r.ReadAt(before, int64(target)-8); err != nil {
		t.Fatalf("overlay ReadAt before: %v", err)
	}
	if !bytes.Equal(before, make([]byte, 8)) {
		t.Errorf("bytes before the sector = %x, want zeroes from the file", before)
	}
}

func TestAnalyzeZeroDescriptor(t *testing.T) {
	const target = uint64(4 << 20)

	li := newLogImage(testFileSize, testLogOffset, testLogSize)
	// Pre-fill the target so a successful replay is visible as a change.
	for i := 0; i < 3*SectorSize; i++ {
		li.data[target+uint64(i)] = 0xFF
	}
	li.place(0, buildEntry(testGUID, 1, 0, []descriptor{
		{zero: true, fileOffset: target, zeroLength: 3 * SectorSize},
	}))

	rp, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rp.SectorCount() != 3 {
		t.Errorf("SectorCount() = %d, want 3", rp.SectorCount())
	}

	r := rp.Overlay(li.reader())
	got := make([]byte, 3*SectorSize)
	if _, err := r.ReadAt(got, int64(target)); err != nil {
		t.Fatalf("overlay ReadAt: %v", err)
	}
	if !bytes.Equal(got, make([]byte, 3*SectorSize)) {
		t.Error("zero descriptor did not zero the range")
	}
}

// TestAnalyzeAppliesSequenceInOrder checks that a later entry writing the same
// sector wins, which is what makes replay order correctness-critical.
func TestAnalyzeAppliesSequenceInOrder(t *testing.T) {
	const target = uint64(4 << 20)

	li := newLogImage(testFileSize, testLogOffset, testLogSize)

	e1 := buildEntry(testGUID, 7, 0, []descriptor{{fileOffset: target, data: sectorOf(0x11)}})
	e2 := buildEntry(testGUID, 8, 0, []descriptor{{fileOffset: target, data: sectorOf(0x22)}})
	li.place(0, e1)
	li.place(int64(len(e1)), e2)

	rp, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if rp.EntryCount() != 2 {
		t.Fatalf("EntryCount() = %d, want 2", rp.EntryCount())
	}
	if first, last := rp.SequenceRange(); first != 7 || last != 8 {
		t.Errorf("SequenceRange() = (%d, %d), want (7, 8)", first, last)
	}

	r := rp.Overlay(li.reader())
	got := make([]byte, 16)
	if _, err := r.ReadAt(got, int64(target)); err != nil {
		t.Fatalf("overlay ReadAt: %v", err)
	}
	if got[0] != 0x22 {
		t.Errorf("sector = %#x, want 0x22 from the higher sequence number", got[0])
	}
}

// TestAnalyzeWrapsAroundLogRegion covers an entry straddling the end of the
// circular buffer.
func TestAnalyzeWrapsAroundLogRegion(t *testing.T) {
	const target = uint64(4 << 20)

	li := newLogImage(testFileSize, testLogOffset, testLogSize)
	entry := buildEntry(testGUID, 1, 0, []descriptor{{fileOffset: target, data: sectorOf(0x5A)}})

	// Start the entry one sector before the end of the region so it wraps.
	tail := int64(testLogSize) - SectorSize
	li.place(tail, entry)
	// The tail field must point at the entry's own start.
	wrapped := buildEntry(testGUID, 1, uint32(tail), []descriptor{{fileOffset: target, data: sectorOf(0x5A)}})
	li.place(tail, wrapped)

	rp, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID)
	if err != nil {
		t.Fatalf("Analyze on a wrapped entry: %v", err)
	}

	r := rp.Overlay(li.reader())
	got := make([]byte, SectorSize)
	if _, err := r.ReadAt(got, int64(target)); err != nil {
		t.Fatalf("overlay ReadAt: %v", err)
	}
	if !bytes.Equal(got, sectorOf(0x5A)) {
		t.Errorf("wrapped entry did not replay: got %x..", got[:8])
	}
}

func TestAnalyzeRejectsForeignLogGUID(t *testing.T) {
	li := newLogImage(testFileSize, testLogOffset, testLogSize)
	other := [16]byte{0x99}
	li.place(0, buildEntry(other, 1, 0, []descriptor{
		{fileOffset: 4 << 20, data: sectorOf(0xAB)},
	}))

	// Entries from a previous log remain in the region; they must be ignored.
	if _, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID); !errors.Is(err, ErrNoEntries) {
		t.Fatalf("Analyze error = %v, want ErrNoEntries", err)
	}
}

func TestAnalyzeRejectsBadChecksum(t *testing.T) {
	li := newLogImage(testFileSize, testLogOffset, testLogSize)
	entry := buildEntry(testGUID, 1, 0, []descriptor{
		{fileOffset: 4 << 20, data: sectorOf(0xAB)},
	})
	// Corrupt a byte after the checksum field.
	entry[100] ^= 0xFF
	li.place(0, entry)

	if _, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID); !errors.Is(err, ErrNoEntries) {
		t.Fatalf("Analyze error = %v, want ErrNoEntries", err)
	}
}

// TestAnalyzeRejectsTornDataSector covers the split sequence number, which is
// how the format detects a sector written only partially.
func TestAnalyzeRejectsTornDataSector(t *testing.T) {
	li := newLogImage(testFileSize, testLogOffset, testLogSize)
	entry := buildEntry(testGUID, 1, 0, []descriptor{
		{fileOffset: 4 << 20, data: sectorOf(0xAB)},
	})

	// Clobber the trailing sequence number in the data sector, then repair the
	// entry checksum so only the torn-write check can catch it.
	headerAndDescs := alignUp(entryHeaderSize+descriptorSize, SectorSize)
	binary.LittleEndian.PutUint32(entry[headerAndDescs+4092:headerAndDescs+4096], 0xDEADBEEF)
	binary.LittleEndian.PutUint32(entry[4:8], 0)
	binary.LittleEndian.PutUint32(entry[4:8], binaryutil.CRC32(entry))
	li.place(0, entry)

	_, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID)
	if err == nil {
		t.Fatal("Analyze accepted a torn data sector")
	}
	t.Logf("rejected as expected: %v", err)
}

// TestAnalyzeRejectsBrokenSequence covers a gap in the sequence numbers, where
// replaying would apply a partial transaction.
func TestAnalyzeRejectsBrokenSequence(t *testing.T) {
	const target = uint64(4 << 20)

	li := newLogImage(testFileSize, testLogOffset, testLogSize)
	e1 := buildEntry(testGUID, 5, 0, []descriptor{{fileOffset: target, data: sectorOf(0x11)}})
	// Sequence jumps from 5 to 9, so the chain from the tail cannot reach it.
	e2 := buildEntry(testGUID, 9, 0, []descriptor{{fileOffset: target, data: sectorOf(0x22)}})
	li.place(0, e1)
	li.place(int64(len(e1)), e2)

	if _, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID); !errors.Is(err, ErrBrokenSequence) {
		t.Fatalf("Analyze error = %v, want ErrBrokenSequence", err)
	}
}

func TestAnalyzeRejectsUnalignedZeroDescriptor(t *testing.T) {
	li := newLogImage(testFileSize, testLogOffset, testLogSize)
	li.place(0, buildEntry(testGUID, 1, 0, []descriptor{
		{zero: true, fileOffset: 4<<20 + 7, zeroLength: SectorSize},
	}))

	if _, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID); err == nil {
		t.Fatal("Analyze accepted an unaligned zero descriptor offset")
	}
}

func TestAnalyzeRejectsOversizedZeroRun(t *testing.T) {
	li := newLogImage(testFileSize, testLogOffset, testLogSize)
	li.place(0, buildEntry(testGUID, 1, 0, []descriptor{
		{zero: true, fileOffset: 0, zeroLength: 1 << 40},
	}))

	if _, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID); err == nil {
		t.Fatal("Analyze accepted a 1 TiB zero run")
	}
}

func TestAnalyzeEmptyLogRegion(t *testing.T) {
	li := newLogImage(testFileSize, testLogOffset, testLogSize)
	if _, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID); !errors.Is(err, ErrNoEntries) {
		t.Fatalf("Analyze error = %v, want ErrNoEntries", err)
	}
}

func TestAnalyzeRejectsInvalidLogGeometry(t *testing.T) {
	li := newLogImage(testFileSize, testLogOffset, testLogSize)

	cases := []struct {
		name      string
		logOffset int64
		logSize   uint32
	}{
		{"zero size", testLogOffset, 0},
		{"unaligned size", testLogOffset, 5000},
		{"zero offset", 0, testLogSize},
		{"unaligned offset", 1234, testLogSize},
	}
	for _, c := range cases {
		if _, err := Analyze(li.reader(), c.logOffset, c.logSize, testGUID); err == nil {
			t.Errorf("%s: Analyze accepted invalid geometry", c.name)
		}
	}
}

// TestOverlayPassesThroughUntouchedRanges guards the common path: a read that no
// replayed sector covers must be handed straight to the base reader.
func TestOverlayPassesThroughUntouchedRanges(t *testing.T) {
	const target = uint64(4 << 20)

	li := newLogImage(testFileSize, testLogOffset, testLogSize)
	for i := range li.data {
		li.data[i] = 0x33
	}
	li.place(0, buildEntry(testGUID, 1, 0, []descriptor{
		{fileOffset: target, data: sectorOf(0xAB)},
	}))

	rp, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	r := rp.Overlay(li.reader())
	got := make([]byte, 512)
	if _, err := r.ReadAt(got, 6<<20); err != nil {
		t.Fatalf("overlay ReadAt: %v", err)
	}
	for i, b := range got {
		if b != 0x33 {
			t.Fatalf("byte %d = %#x, want 0x33 passed through from the file", i, b)
		}
	}
}

// TestOverlayReadSpanningBoundary covers a read that starts mid-sector and
// crosses from replayed into non-replayed territory.
func TestOverlayReadSpanningBoundary(t *testing.T) {
	const target = uint64(4 << 20)

	li := newLogImage(testFileSize, testLogOffset, testLogSize)
	for i := range li.data {
		li.data[i] = 0x33
	}
	li.place(0, buildEntry(testGUID, 1, 0, []descriptor{
		{fileOffset: target, data: sectorOf(0xAB)},
	}))

	rp, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	r := rp.Overlay(li.reader())

	// Start 16 bytes before the replayed sector and run 32 bytes past its end.
	buf := make([]byte, 16+SectorSize+32)
	if _, err := r.ReadAt(buf, int64(target)-16); err != nil {
		t.Fatalf("overlay ReadAt: %v", err)
	}

	for i := 0; i < 16; i++ {
		if buf[i] != 0x33 {
			t.Fatalf("leading byte %d = %#x, want 0x33 from the file", i, buf[i])
		}
	}
	for i := 16; i < 16+SectorSize; i++ {
		if buf[i] != 0xAB {
			t.Fatalf("replayed byte %d = %#x, want 0xAB", i, buf[i])
		}
	}
	for i := 16 + SectorSize; i < len(buf); i++ {
		if buf[i] != 0x33 {
			t.Fatalf("trailing byte %d = %#x, want 0x33 from the file", i, buf[i])
		}
	}
}

// FuzzAnalyze asserts the log parser never panics and never allocates without
// bound, whatever the log region holds.
func FuzzAnalyze(f *testing.F) {
	valid := buildEntry(testGUID, 1, 0, []descriptor{{fileOffset: 4 << 20, data: sectorOf(0xAB)}})
	f.Add(valid)
	f.Add(make([]byte, SectorSize))
	f.Add(bytes.Repeat([]byte{0xFF}, SectorSize))

	f.Fuzz(func(t *testing.T, seed []byte) {
		if len(seed) == 0 || len(seed) > int(testLogSize) {
			return
		}

		li := newLogImage(testFileSize, testLogOffset, testLogSize)
		copy(li.data[testLogOffset:testLogOffset+int64(testLogSize)], seed)

		rp, err := Analyze(li.reader(), testLogOffset, testLogSize, testGUID)
		if err != nil {
			return
		}

		r := rp.Overlay(li.reader())
		buf := make([]byte, 8192)
		for _, off := range []int64{0, 4 << 20, testFileSize - 1, testFileSize} {
			_, _ = r.ReadAt(buf, off)
		}
	})
}
