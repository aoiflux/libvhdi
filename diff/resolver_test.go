// SPDX-License-Identifier: MIT

package diff

import (
	"bytes"
	"errors"
	"testing"

	"github.com/aoiflux/libvhdi/types"
)

// The resolver is exercised thoroughly through the reader's chain tests. What
// those cannot reach is its own argument validation and the paths that exist
// for misconfiguration -- which matter because this is the layer that decides,
// sector by sector, whether a byte comes from the child or the parent. A
// resolver built wrong does not fail loudly; it returns the wrong disk.

const (
	testBlockSize  = 1024 * 1024
	testSectorSize = 512
	testVirtual    = uint64(4 * testBlockSize)
)

// vhdBAT builds a VHD block allocation table with the given allocation states.
func vhdBAT(allocated ...bool) *types.VHDBlockAllocationTable {
	bat := &types.VHDBlockAllocationTable{
		NumberOfEntries:  uint32(len(allocated)),
		BlockSize:        testBlockSize,
		SectorBitmapSize: testSectorSize,
		Entries:          make([]types.BlockAllocationEntry, len(allocated)),
	}
	for i, ok := range allocated {
		bat.Entries[i] = types.BlockAllocationEntry{IsAllocated: ok}
		if ok {
			bat.Entries[i].FileOffset = int64(i) * testBlockSize
		}
	}
	return bat
}

// vhdxBAT builds a VHDX block allocation table with the given block states.
func vhdxBAT(states ...types.BlockState) *types.VHDXBlockAllocationTable {
	bat := &types.VHDXBlockAllocationTable{
		NumberOfEntries: uint32(len(states)),
		BlockSize:       testBlockSize,
		Entries:         make([]types.BlockAllocationEntry, len(states)),
	}
	for i, s := range states {
		bat.Entries[i] = types.BlockAllocationEntry{
			BlockState:  s,
			IsAllocated: s == types.BlockStateFullyAllocated || s == types.BlockStatePartiallyAllocated,
			FileOffset:  int64(i) * testBlockSize,
		}
	}
	return bat
}

func baseConfig() Config {
	return Config{
		Source:      bytes.NewReader(make([]byte, testBlockSize)),
		Child:       bytes.NewReader(make([]byte, testVirtual)),
		BlockSize:   testBlockSize,
		SectorSize:  testSectorSize,
		VirtualSize: testVirtual,
		VHDBAT:      vhdBAT(true, false, false, false),
	}
}

func TestNewRejectsAMisconfiguredResolver(t *testing.T) {
	// Each of these would otherwise produce a resolver that resolves
	// incorrectly rather than one that fails, which is the dangerous direction.
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name:   "no child reader",
			mutate: func(c *Config) { c.Child = nil },
		},
		{
			name:   "zero block size",
			mutate: func(c *Config) { c.BlockSize = 0 },
		},
		{
			name: "neither BAT set",
			mutate: func(c *Config) {
				c.VHDBAT = nil
				c.VHDXBAT = nil
			},
		},
		{
			name: "both BATs set",
			mutate: func(c *Config) {
				c.VHDXBAT = vhdxBAT(types.BlockStateNone)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			tc.mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("New accepted a configuration it cannot resolve correctly")
			}
		})
	}
}

func TestNewAcceptsEitherBATAlone(t *testing.T) {
	vhd := baseConfig()
	if _, err := New(vhd); err != nil {
		t.Fatalf("New with a VHD BAT: %v", err)
	}

	vhdx := baseConfig()
	vhdx.VHDBAT = nil
	vhdx.VHDXBAT = vhdxBAT(types.BlockStateNone, types.BlockStateNone, types.BlockStateNone, types.BlockStateNone)
	if _, err := New(vhdx); err != nil {
		t.Fatalf("New with a VHDX BAT: %v", err)
	}
}

func TestReadWithNoParentFailsClosed(t *testing.T) {
	// The rule the whole library rests on. A range that resolves to an absent
	// parent must error, because zeroes would be indistinguishable from genuine
	// disk contents and nothing downstream could tell.
	cfg := baseConfig()
	cfg.VHDBAT = vhdBAT(false, false, false, false) // everything from the parent
	cfg.Parent = nil

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	buf := make([]byte, 512)
	if _, err := r.ReadAt(buf, 0); !errors.Is(err, ErrParentRequired) {
		t.Fatalf("ReadAt returned %v, want ErrParentRequired", err)
	}
}

func TestZeroBlockNeedsNoParent(t *testing.T) {
	// A VHDX block recorded as zero or unmapped is not parent-backed, so it
	// reads successfully even with no parent attached. Failing here would make
	// a partially-recoverable image unreadable for no reason.
	for _, state := range []types.BlockState{
		types.BlockStateZero,
		types.BlockStateUnmapped,
		types.BlockStateUndefined,
	} {
		cfg := baseConfig()
		cfg.VHDBAT = nil
		cfg.VHDXBAT = vhdxBAT(state, state, state, state)
		cfg.Parent = nil

		r, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		buf := make([]byte, 512)
		for i := range buf {
			buf[i] = 0xFF
		}
		if _, err := r.ReadAt(buf, 0); err != nil {
			t.Fatalf("block state %d: ReadAt: %v", state, err)
		}
		for _, b := range buf {
			if b != 0 {
				t.Fatalf("block state %d read as %#x, want zeroes", state, b)
			}
		}
	}
}

func TestNotPresentBlockComesFromTheParent(t *testing.T) {
	// PAYLOAD_BLOCK_NOT_PRESENT means the parent owns the range. Confusing it
	// with the zero states would return zeroes where the parent holds data.
	parent := bytes.NewReader(bytes.Repeat([]byte{0xA5}, int(testVirtual)))

	cfg := baseConfig()
	cfg.VHDBAT = nil
	cfg.VHDXBAT = vhdxBAT(
		types.BlockStateNone, types.BlockStateNone,
		types.BlockStateNone, types.BlockStateNone,
	)
	cfg.Parent = parent

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	buf := make([]byte, 512)
	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	for _, b := range buf {
		if b != 0xA5 {
			t.Fatalf("read %#x, want the parent's 0xA5", b)
		}
	}
}

func TestReadRejectsANegativeOffset(t *testing.T) {
	r, err := New(baseConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.ReadAt(make([]byte, 16), -1); err == nil {
		t.Fatal("a negative offset was accepted")
	}
}

func TestReadPastTheEndReportsEOF(t *testing.T) {
	r, err := New(baseConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.ReadAt(make([]byte, 16), int64(testVirtual)+1); err == nil {
		t.Fatal("a read past the end of the device was accepted")
	}
}

func TestVirtualSizeIsReported(t *testing.T) {
	r, err := New(baseConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := r.VirtualSize(); got != int64(testVirtual) {
		t.Fatalf("VirtualSize() = %d, want %d", got, testVirtual)
	}
}

func TestSectorBitmapResolutionNeedsTheChildsBackingReader(t *testing.T) {
	// A partially-written block can only be resolved by reading its sector
	// bitmap, which lives in the child's file rather than in its decoded
	// stream. Without Source there is no way to tell which sectors belong to
	// the child, and both alternatives -- returning the child's zero-filled
	// holes, or the parent's bytes where the child has overwritten them --
	// produce a plausible device that is wrong.
	cfg := baseConfig()
	cfg.Source = nil
	cfg.Parent = bytes.NewReader(make([]byte, testVirtual))

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Block 0 is allocated, so on a differencing VHD it resolves per sector.
	buf := make([]byte, 512)
	_, err = r.ReadAt(buf, 0)
	if !errors.Is(err, ErrNoBitmapSource) {
		t.Fatalf("ReadAt returned %v, want ErrNoBitmapSource", err)
	}
}

func TestBlockKindRejectsAnOutOfRangeIndex(t *testing.T) {
	r, err := New(baseConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.BlockKind(999); err == nil {
		t.Fatal("an out-of-range block index was accepted")
	}
}

func TestSetParentAttachesAfterConstruction(t *testing.T) {
	// The open path builds the resolver before the parent is located, so a
	// resolver has to be completable after the fact.
	cfg := baseConfig()
	cfg.VHDBAT = vhdBAT(false, false, false, false)
	cfg.Parent = nil

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	buf := make([]byte, 512)
	if _, err := r.ReadAt(buf, 0); !errors.Is(err, ErrParentRequired) {
		t.Fatalf("ReadAt before SetParent returned %v, want ErrParentRequired", err)
	}

	r.SetParent(bytes.NewReader(bytes.Repeat([]byte{0x5A}, int(testVirtual))))

	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt after SetParent: %v", err)
	}
	if buf[0] != 0x5A {
		t.Fatalf("read %#x, want the parent's 0x5A", buf[0])
	}
}
