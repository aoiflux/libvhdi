// SPDX-License-Identifier: MIT

package reader

import (
	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

// ChainEntry describes one disk in a differencing chain.
type ChainEntry struct {
	// Index is the disk's position: 0 is the disk Chain was called on, 1 its
	// parent, and so on.
	Index int

	// Path is the file the disk was opened from, or "" when it came from a bare
	// io.ReaderAt.
	Path string

	// Identifier is the disk's own GUID.
	Identifier [16]byte

	// ParentIdentifier is the GUID this disk records for its parent, zero for a
	// disk that has none.
	ParentIdentifier [16]byte

	// ParentFilename is the parent path recorded in this disk's metadata.
	ParentFilename string

	Format   types.FileFormat
	DiskType types.DiskType

	// VirtualSize is the disk's virtual size. Every link in a valid chain
	// reports the same value.
	VirtualSize uint64

	// IsDifferencing reports whether this disk depends on a parent.
	IsDifferencing bool

	// HasLog and LogReplayed describe the disk's VHDX log state.
	HasLog      bool
	LogReplayed bool
}

// GUIDString returns the disk's identifier as a formatted GUID.
func (e ChainEntry) GUIDString() string { return binaryutil.GUIDToString(e.Identifier) }

// Chain returns the disks backing this one, from this disk outwards.
//
// A fixed or dynamic disk yields a single entry. A differencing disk yields one
// entry per attached link, so the result is the set of files that together
// constitute the device — which is what an evidence record needs to name.
//
// The chain may be shorter than the image requires: if the last entry reports
// IsDifferencing, its parent was never attached, and ParentResolveError explains
// why. Reads into that parent's ranges fail with ErrParentRequired.
func (d *VirtualDisk) Chain() []ChainEntry {
	var out []ChainEntry

	for disk, i := d, 0; disk != nil; disk, i = disk.parent, i+1 {
		out = append(out, ChainEntry{
			Index:            i,
			Path:             disk.path,
			Identifier:       disk.Identifier(),
			ParentIdentifier: disk.ParentIdentifier(),
			ParentFilename:   disk.ParentFilename(),
			Format:           disk.format,
			DiskType:         disk.diskType,
			VirtualSize:      disk.virtualSize,
			IsDifferencing:   disk.IsDifferencing(),
			HasLog:           disk.hasLog,
			LogReplayed:      disk.replay != nil,
		})
	}

	return out
}

// ChainComplete reports whether every link the chain needs is attached, meaning
// the whole virtual disk is readable.
func (d *VirtualDisk) ChainComplete() bool {
	for disk := d; disk != nil; disk = disk.parent {
		if disk.IsDifferencing() && disk.parent == nil {
			return false
		}
	}
	return true
}
