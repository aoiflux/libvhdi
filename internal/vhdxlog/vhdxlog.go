// SPDX-License-Identifier: MIT

// Package vhdxlog parses and replays the VHDX log.
//
// A VHDX file whose active header carries a non-zero log GUID has writes that
// were journalled but may not have reached their final locations. Until the log
// is replayed, the block allocation table and metadata region can describe an
// earlier state than the last committed one, so anything decoded from them may
// be stale.
//
// Replay here is strictly read-only: entries are applied to an in-memory overlay
// that is layered over the backing reader, so the image on disk is never
// modified. That keeps a forensic copy byte-identical while still presenting the
// committed view of the disk.
//
// Layout, per the VHDX Format Specification:
//
//	log region  := 4 KB sectors, treated as a circular buffer
//	log entry   := entry header (64 bytes) | descriptors (32 bytes each) | data sectors (4 KB each)
//	descriptor  := "desc" (one 4 KB data sector follows) or "zero" (a run of zeroes)
package vhdxlog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
)

// SectorSize is the log's fixed sector size. Every entry, descriptor target and
// data sector is aligned to it.
const SectorSize = 4096

const (
	entryHeaderSize = 64
	descriptorSize  = 32

	// maxZeroRun bounds a single zero descriptor's length. A run longer than
	// this is treated as malformed rather than allowed to drive allocation.
	maxZeroRun = 1 << 30 // 1 GiB

	// maxOverlaySectors bounds total replayed sectors, capping memory at roughly
	// 256 MiB of overlay.
	maxOverlaySectors = 65536
)

var (
	sigEntry = []byte("loge")
	sigZero  = []byte("zero")
	sigDesc  = []byte("desc")
	sigData  = []byte("data")
)

// ErrNoEntries reports that the log region holds no entry matching the log GUID.
// The log is either fully flushed or unusable; either way there is nothing that
// can be replayed.
var ErrNoEntries = errors.New("libvhdi: no valid VHDX log entries")

// ErrBrokenSequence reports that valid entries exist but do not form a complete,
// consecutively numbered sequence, so replaying them could apply a partial
// transaction.
var ErrBrokenSequence = errors.New("libvhdi: VHDX log sequence is incomplete")

// errNotEntry marks a candidate offset that does not begin a valid entry. It is
// expected during scanning and never surfaces to callers.
var errNotEntry = errors.New("not a log entry")

// ============================================================================
// Circular log region
// ============================================================================

// region presents the log as a circular buffer. Entries and their data sectors
// may wrap past the end of the region back to its start.
type region struct {
	r      io.ReaderAt
	offset int64
	size   int64
}

// readAt fills p from the region starting at the region-relative offset off,
// wrapping at the end of the region.
func (g *region) readAt(p []byte, off int64) error {
	if int64(len(p)) > g.size {
		return fmt.Errorf("read of %d bytes exceeds the %d byte log region", len(p), g.size)
	}
	off = ((off % g.size) + g.size) % g.size

	if untilEnd := g.size - off; untilEnd >= int64(len(p)) {
		_, err := g.r.ReadAt(p, g.offset+off)
		return err
	} else {
		if _, err := g.r.ReadAt(p[:untilEnd], g.offset+off); err != nil {
			return err
		}
		_, err := g.r.ReadAt(p[untilEnd:], g.offset)
		return err
	}
}

// ============================================================================
// Entries
// ============================================================================

// entry is a validated log entry together with its raw bytes.
type entry struct {
	offset          int64 // region-relative
	entryLength     uint32
	tail            uint32
	sequenceNumber  uint64
	descriptorCount uint32
	raw             []byte
}

// readEntry validates and returns the entry beginning at the region-relative
// offset off, or errNotEntry if none does.
func (g *region) readEntry(off int64, logGUID [16]byte) (*entry, error) {
	var hdr [entryHeaderSize]byte
	if err := g.readAt(hdr[:], off); err != nil {
		return nil, errNotEntry
	}
	if !bytes.Equal(hdr[0:4], sigEntry) {
		return nil, errNotEntry
	}

	entryLength := binary.LittleEndian.Uint32(hdr[8:12])
	if entryLength < SectorSize || entryLength%SectorSize != 0 || int64(entryLength) > g.size {
		return nil, errNotEntry
	}

	raw := make([]byte, entryLength)
	if err := g.readAt(raw, off); err != nil {
		return nil, errNotEntry
	}

	// The entry checksum is CRC-32C over the whole entry with the checksum
	// field itself zeroed.
	stored := binary.LittleEndian.Uint32(raw[4:8])
	binary.LittleEndian.PutUint32(raw[4:8], 0)
	computed := binaryutil.CRC32(raw)
	binary.LittleEndian.PutUint32(raw[4:8], stored)
	if computed != stored {
		return nil, errNotEntry
	}

	// An entry belongs to this log only if it carries the same log GUID as the
	// active header. Stale entries from a previous log remain in the region.
	var guid [16]byte
	copy(guid[:], raw[32:48])
	if guid != logGUID {
		return nil, errNotEntry
	}

	sequenceNumber := binary.LittleEndian.Uint64(raw[16:24])
	if sequenceNumber == 0 {
		return nil, errNotEntry
	}

	descriptorCount := binary.LittleEndian.Uint32(raw[24:28])
	if uint64(entryHeaderSize)+uint64(descriptorCount)*descriptorSize > uint64(entryLength) {
		return nil, errNotEntry
	}

	tail := binary.LittleEndian.Uint32(raw[12:16])
	if tail%SectorSize != 0 || int64(tail) >= g.size {
		return nil, errNotEntry
	}

	return &entry{
		offset:          off,
		entryLength:     entryLength,
		tail:            tail,
		sequenceNumber:  sequenceNumber,
		descriptorCount: descriptorCount,
		raw:             raw,
	}, nil
}

// ============================================================================
// Replay
// ============================================================================

// Replay holds the sectors a log replay would write, keyed by their
// SectorSize-aligned offset in the file.
type Replay struct {
	sectors map[int64][]byte

	entryCount      int
	descriptorCount int
	firstSequence   uint64
	lastSequence    uint64
}

// EntryCount returns how many log entries were replayed.
func (rp *Replay) EntryCount() int { return rp.entryCount }

// DescriptorCount returns how many descriptors were applied.
func (rp *Replay) DescriptorCount() int { return rp.descriptorCount }

// SectorCount returns how many distinct file sectors the replay changes.
func (rp *Replay) SectorCount() int { return len(rp.sectors) }

// SequenceRange returns the first and last sequence numbers replayed.
func (rp *Replay) SequenceRange() (first, last uint64) {
	return rp.firstSequence, rp.lastSequence
}

// Analyze locates the active log sequence and computes the sectors its replay
// would produce. It reads only; nothing is written to r.
//
// logOffset and logSize come from the active image header, along with logGUID.
// A zero logGUID means the file has no log; callers should not call Analyze.
func Analyze(r io.ReaderAt, logOffset int64, logSize uint32, logGUID [16]byte) (*Replay, error) {
	if logGUID == ([16]byte{}) {
		return nil, ErrNoEntries
	}
	if logSize == 0 || logSize%SectorSize != 0 {
		return nil, fmt.Errorf("libvhdi: log size %d is not a non-zero multiple of %d", logSize, SectorSize)
	}
	if logOffset <= 0 || logOffset%SectorSize != 0 {
		return nil, fmt.Errorf("libvhdi: log offset %d is not a positive multiple of %d", logOffset, SectorSize)
	}

	g := &region{r: r, offset: logOffset, size: int64(logSize)}

	// Scan every sector boundary for entries belonging to this log, keeping the
	// one with the highest sequence number.
	var newest *entry
	found := 0
	for off := int64(0); off < g.size; off += SectorSize {
		e, err := g.readEntry(off, logGUID)
		if err != nil {
			continue
		}
		found++
		if newest == nil || e.sequenceNumber > newest.sequenceNumber {
			newest = e
		}
	}
	if newest == nil {
		return nil, ErrNoEntries
	}

	// The newest entry's tail points at the first entry of its sequence. Walk
	// forward from there, requiring consecutive sequence numbers, so a partial
	// transaction is refused rather than half-applied.
	chain, err := g.collectSequence(newest, logGUID)
	if err != nil {
		return nil, err
	}

	rp := &Replay{
		sectors:       make(map[int64][]byte),
		entryCount:    len(chain),
		firstSequence: chain[0].sequenceNumber,
		lastSequence:  chain[len(chain)-1].sequenceNumber,
	}

	// Apply in ascending sequence order so later writes win.
	for _, e := range chain {
		n, err := rp.apply(e)
		if err != nil {
			return nil, err
		}
		rp.descriptorCount += n
	}

	return rp, nil
}

// collectSequence walks the log from newest.tail to newest, requiring
// consecutive sequence numbers throughout.
func (g *region) collectSequence(newest *entry, logGUID [16]byte) ([]*entry, error) {
	maxEntries := int(g.size / SectorSize)

	first, err := g.readEntry(int64(newest.tail), logGUID)
	if err != nil {
		return nil, fmt.Errorf("%w: tail at %d does not begin a valid entry", ErrBrokenSequence, newest.tail)
	}

	var chain []*entry
	off := int64(newest.tail)
	expected := first.sequenceNumber

	for len(chain) <= maxEntries {
		e, err := g.readEntry(off, logGUID)
		if err != nil {
			return nil, fmt.Errorf("%w: no valid entry at offset %d", ErrBrokenSequence, off)
		}
		if e.sequenceNumber != expected {
			return nil, fmt.Errorf("%w: entry at %d has sequence %d, expected %d",
				ErrBrokenSequence, off, e.sequenceNumber, expected)
		}

		chain = append(chain, e)
		if e.sequenceNumber == newest.sequenceNumber {
			return chain, nil
		}

		off = (off + int64(e.entryLength)) % g.size
		expected++
	}

	return nil, fmt.Errorf("%w: sequence exceeds %d entries", ErrBrokenSequence, maxEntries)
}

// apply adds one entry's descriptors to the overlay, returning how many it
// applied.
//
// Data sectors begin at the first sector boundary after the descriptors and are
// consumed in descriptor order; zero descriptors consume none.
func (rp *Replay) apply(e *entry) (int, error) {
	dataStart := alignUp(entryHeaderSize+int(e.descriptorCount)*descriptorSize, SectorSize)
	dataIndex := 0
	applied := 0

	for i := 0; i < int(e.descriptorCount); i++ {
		d := e.raw[entryHeaderSize+i*descriptorSize:][:descriptorSize]

		// Every descriptor repeats the entry's sequence number.
		if seq := binary.LittleEndian.Uint64(d[24:32]); seq != e.sequenceNumber {
			return applied, fmt.Errorf("libvhdi: log descriptor sequence %d does not match entry %d",
				seq, e.sequenceNumber)
		}

		switch {
		case bytes.Equal(d[0:4], sigZero):
			zeroLength := binary.LittleEndian.Uint64(d[8:16])
			fileOffset := binary.LittleEndian.Uint64(d[16:24])
			if err := rp.applyZero(fileOffset, zeroLength); err != nil {
				return applied, err
			}

		case bytes.Equal(d[0:4], sigDesc):
			sectorOff := dataStart + dataIndex*SectorSize
			dataIndex++
			if sectorOff+SectorSize > len(e.raw) {
				return applied, errors.New("libvhdi: log data sector extends beyond its entry")
			}
			if err := rp.applyData(d, e.raw[sectorOff:sectorOff+SectorSize], e.sequenceNumber); err != nil {
				return applied, err
			}

		default:
			return applied, fmt.Errorf("libvhdi: unknown log descriptor signature %q", d[0:4])
		}
		applied++
	}

	return applied, nil
}

// applyZero records a run of zeroed sectors.
func (rp *Replay) applyZero(fileOffset, zeroLength uint64) error {
	if fileOffset%SectorSize != 0 || zeroLength%SectorSize != 0 {
		return fmt.Errorf("libvhdi: zero descriptor offset %d length %d is not sector aligned",
			fileOffset, zeroLength)
	}
	if zeroLength > maxZeroRun {
		return fmt.Errorf("libvhdi: zero descriptor length %d exceeds the %d byte limit",
			zeroLength, int64(maxZeroRun))
	}
	if fileOffset > uint64(maxInt64)-zeroLength {
		return errors.New("libvhdi: zero descriptor range overflows")
	}

	for off := fileOffset; off < fileOffset+zeroLength; off += SectorSize {
		if err := rp.put(int64(off), make([]byte, SectorSize)); err != nil {
			return err
		}
	}
	return nil
}

// applyData reconstructs the 4 KB sector a data descriptor describes.
//
// The writer overwrites the first 8 and last 4 bytes of each data sector with a
// signature and the entry's sequence number, stashing the displaced bytes in the
// descriptor. Reconstruction puts them back.
func (rp *Replay) applyData(d, sector []byte, sequenceNumber uint64) error {
	if !bytes.Equal(sector[0:4], sigData) {
		return errors.New("libvhdi: log data sector has an invalid signature")
	}

	// The sector carries the entry's sequence number split across its ends,
	// which detects a torn write within the sector.
	if got := binary.LittleEndian.Uint32(sector[4:8]); got != uint32(sequenceNumber>>32) {
		return errors.New("libvhdi: log data sector sequence high mismatch (torn write)")
	}
	if got := binary.LittleEndian.Uint32(sector[4092:4096]); got != uint32(sequenceNumber) {
		return errors.New("libvhdi: log data sector sequence low mismatch (torn write)")
	}

	fileOffset := binary.LittleEndian.Uint64(d[16:24])
	if fileOffset%SectorSize != 0 {
		return fmt.Errorf("libvhdi: data descriptor offset %d is not sector aligned", fileOffset)
	}
	if fileOffset > uint64(maxInt64)-SectorSize {
		return errors.New("libvhdi: data descriptor offset overflows")
	}

	out := make([]byte, SectorSize)
	copy(out[0:8], d[8:16]) // leading bytes displaced by the signature
	copy(out[8:4092], sector[8:4092])
	copy(out[4092:4096], d[4:8]) // trailing bytes displaced by the sequence low

	return rp.put(int64(fileOffset), out)
}

func (rp *Replay) put(offset int64, sector []byte) error {
	if _, exists := rp.sectors[offset]; !exists && len(rp.sectors) >= maxOverlaySectors {
		return fmt.Errorf("libvhdi: log replay would touch more than %d sectors", maxOverlaySectors)
	}
	rp.sectors[offset] = sector
	return nil
}

// ============================================================================
// Overlay
// ============================================================================

// Overlay returns a reader presenting base with the replayed sectors applied.
// base is never written to.
//
// The returned reader is safe for concurrent use whenever base is, since the
// overlay is immutable once Analyze returns.
func (rp *Replay) Overlay(base io.ReaderAt) io.ReaderAt {
	if rp == nil || len(rp.sectors) == 0 {
		return base
	}
	return &overlayReader{base: base, sectors: rp.sectors}
}

type overlayReader struct {
	base    io.ReaderAt
	sectors map[int64][]byte
}

// ReadAt implements io.ReaderAt, serving any byte a replayed sector covers from
// the overlay and everything else from the underlying reader.
//
// A byte of p is valid if the base read reached it or a replayed sector covers
// it. Only a contiguous prefix can be reported, per the io.ReaderAt contract, so
// the count returned is the length of that prefix.
func (o *overlayReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("libvhdi: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}

	baseN, baseErr := o.base.ReadAt(p, off)

	end := off + int64(len(p))
	firstSector := off - off%SectorSize

	// Replayed sectors are authoritative: they hold the committed contents,
	// whereas the file still holds the pre-replay bytes.
	overlaid := false
	for s := firstSector; s < end; s += SectorSize {
		sector, ok := o.sectors[s]
		if !ok {
			continue
		}
		overlaid = true

		from := max(s, off)
		to := min(s+SectorSize, end)
		copy(p[from-off:to-off], sector[from-s:to-s])
	}

	if !overlaid {
		return baseN, baseErr
	}

	// Extend the valid prefix past where the base read stopped, for as long as
	// replayed sectors cover the gap. This matters when the log describes
	// sectors at or beyond the current end of file.
	valid := baseN
	for valid < len(p) {
		at := off + int64(valid)
		s := at - at%SectorSize
		if _, ok := o.sectors[s]; !ok {
			break
		}
		valid = int(min(s+SectorSize, end) - off)
	}

	if valid >= len(p) {
		return len(p), nil
	}
	return valid, baseErr
}

func alignUp(n, to int) int {
	if n%to == 0 {
		return n
	}
	return (n/to + 1) * to
}

const maxInt64 = int64(^uint64(0) >> 1)
