// SPDX-License-Identifier: MIT

package reader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// TestChainSingleDisk checks that a disk with no parent yields exactly one entry.
func TestChainSingleDisk(t *testing.T) {
	img := buildDynamicVHD(types.DiskTypeDynamic, chainMediaSize, chainBlockSize, []vhdBlock{
		{allocated: true, presentSectors: allSectors(chainBlockSize / 512), data: repeatByte(0x11, chainBlockSize)},
	}, "", [16]byte{})

	d, err := OpenVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHD: %v", err)
	}
	defer d.Close()

	chain := d.Chain()
	if len(chain) != 1 {
		t.Fatalf("Chain() returned %d entries, want 1", len(chain))
	}
	e := chain[0]
	if e.Index != 0 {
		t.Errorf("Index = %d, want 0", e.Index)
	}
	if e.IsDifferencing {
		t.Error("IsDifferencing = true for a dynamic disk")
	}
	if e.Format != types.FileFormatVHD || e.DiskType != types.DiskTypeDynamic {
		t.Errorf("Format/DiskType = %v/%d, want VHD/dynamic", e.Format, e.DiskType)
	}
	if e.VirtualSize != chainMediaSize {
		t.Errorf("VirtualSize = %d, want %d", e.VirtualSize, chainMediaSize)
	}
	if !d.ChainComplete() {
		t.Error("ChainComplete() = false for a disk needing no parent")
	}
}

// TestChainThreeDeepRecordsEveryFile is the evidence-record case: naming every
// file that together constitutes the device.
func TestChainThreeDeepRecordsEveryFile(t *testing.T) {
	const (
		blockSize = chainBlockSize
		mediaSize = chainMediaSize
	)
	sectors := blockSize / 512

	base := buildVHD(vhdImageParams{
		diskType: types.DiskTypeDynamic, mediaSize: mediaSize, blockSize: blockSize,
		blocks: []vhdBlock{{allocated: true, presentSectors: allSectors(sectors), data: repeatByte(0xA0, blockSize)}},
		selfID: idBase,
	})
	midData := make([]byte, blockSize)
	copy(midData[0:1024], repeatByte(0xB0, 1024))
	mid := buildVHD(vhdImageParams{
		diskType: types.DiskTypeDifferential, mediaSize: mediaSize, blockSize: blockSize,
		blocks:     []vhdBlock{{allocated: true, presentSectors: []int{0, 1}, data: midData}},
		parentName: "base.vhd", parentID: idBase, selfID: idMid,
	})
	topData := make([]byte, blockSize)
	copy(topData[0:512], repeatByte(0xC0, 512))
	top := buildVHD(vhdImageParams{
		diskType: types.DiskTypeDifferential, mediaSize: mediaSize, blockSize: blockSize,
		blocks:     []vhdBlock{{allocated: true, presentSectors: []int{0}, data: topData}},
		parentName: "mid.vhd", parentID: idMid, selfID: idTop,
	})

	dir := writeChainDir(t, map[string][]byte{
		"base.vhd": base, "mid.vhd": mid, "top.vhd": top,
	})

	d, err := OpenFile(filepath.Join(dir, "top.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	chain := d.Chain()
	if len(chain) != 3 {
		t.Fatalf("Chain() returned %d entries, want 3: %+v", len(chain), chain)
	}
	if !d.ChainComplete() {
		t.Error("ChainComplete() = false for a fully resolved chain")
	}

	wantIDs := [][16]byte{idTop, idMid, idBase}
	wantPaths := []string{"top.vhd", "mid.vhd", "base.vhd"}
	wantDiff := []bool{true, true, false}

	for i, e := range chain {
		if e.Index != i {
			t.Errorf("entry %d Index = %d", i, e.Index)
		}
		if e.Identifier != wantIDs[i] {
			t.Errorf("entry %d Identifier = %x, want %x", i, e.Identifier, wantIDs[i])
		}
		if filepath.Base(e.Path) != wantPaths[i] {
			t.Errorf("entry %d Path = %q, want basename %q", i, e.Path, wantPaths[i])
		}
		if e.IsDifferencing != wantDiff[i] {
			t.Errorf("entry %d IsDifferencing = %v, want %v", i, e.IsDifferencing, wantDiff[i])
		}
		if e.VirtualSize != mediaSize {
			t.Errorf("entry %d VirtualSize = %d, want %d", i, e.VirtualSize, mediaSize)
		}
		if e.GUIDString() == "" {
			t.Errorf("entry %d GUIDString() is empty", i)
		}
	}

	// Each differencing link must name the next one.
	if chain[0].ParentIdentifier != idMid {
		t.Errorf("top records parent %x, want %x", chain[0].ParentIdentifier, idMid)
	}
	if chain[1].ParentIdentifier != idBase {
		t.Errorf("mid records parent %x, want %x", chain[1].ParentIdentifier, idBase)
	}
}

// TestChainIncompleteIsVisible checks that a chain missing its parent is
// reported as such rather than looking complete.
func TestChainIncompleteIsVisible(t *testing.T) {
	_, childImg, _ := diffVHDPair("parent.vhd")
	dir := writeChainDir(t, map[string][]byte{"child.vhd": childImg})

	d, err := OpenFile(filepath.Join(dir, "child.vhd"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	chain := d.Chain()
	if len(chain) != 1 {
		t.Fatalf("Chain() returned %d entries, want 1 (the parent is missing)", len(chain))
	}
	if !chain[0].IsDifferencing {
		t.Error("the sole entry should report IsDifferencing, marking the chain unfinished")
	}
	if d.ChainComplete() {
		t.Error("ChainComplete() = true but the parent was never attached")
	}
	if d.ParentResolveError() == nil {
		t.Error("ParentResolveError() = nil for an incomplete chain")
	}
}

// TestChainReportsLogState checks the log fields travel with the chain, so an
// evidence record can note which link was captured mid-write.
func TestChainReportsLogState(t *testing.T) {
	p := vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateNone,
		payload:         repeatByte(0xD7, vhdxDiffBlockSize),
	}
	p.logWrites = []logWrite{{fileOffset: vhdxBATRegionOff, data: batSectorWithBlock0(p)}}
	img := buildVHDX(p)

	d, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("OpenVHDX: %v", err)
	}
	defer d.Close()

	chain := d.Chain()
	if len(chain) != 1 {
		t.Fatalf("Chain() returned %d entries, want 1", len(chain))
	}
	if !chain[0].HasLog {
		t.Error("HasLog = false, want true")
	}
	if !chain[0].LogReplayed {
		t.Error("LogReplayed = false, want true")
	}
}

// TestVHDXMetadataRegionBeyondEOFIsRejected covers the region offset check added
// alongside the BAT one, so a bad pointer is reported as a malformed image rather
// than as an I/O error from deep in the parser.
func TestVHDXMetadataRegionBeyondEOFIsRejected(t *testing.T) {
	img := buildVHDX(vhdxParams{
		blockSize:       vhdxDiffBlockSize,
		sectorSize:      vhdxDiffSectorSize,
		virtualDiskSize: vhdxDiffVirtual,
		blockState:      types.BlockStateFullyAllocated,
		payload:         repeatByte(0xA0, vhdxDiffBlockSize),
	})

	// Point the metadata region past the end of the file in both region tables,
	// repairing their CRCs so the parser reaches the validation.
	for _, off := range []int{types.VHDXFirstRegionTableOffset, types.VHDXSecondRegionTableOffset} {
		binary.LittleEndian.PutUint64(img[off+48+16:off+48+24], 1<<40)
		finalizeRegionTableCRC(img, int64(off))
	}

	_, err := OpenVHDX(bytes.NewReader(img), int64(len(img)))
	if !errors.Is(err, ErrCorruptImage) {
		t.Fatalf("OpenVHDX error = %v, want ErrCorruptImage", err)
	}
}
