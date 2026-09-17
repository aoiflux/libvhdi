// SPDX-License-Identifier: MIT

package reader

import (
	"strconv"
	"time"

	"github.com/aoiflux/libvhdi/internal/binaryutil"
	"github.com/aoiflux/libvhdi/types"
)

// Geometry describes the physical shape an image reports for itself.
//
// Both formats record more than the virtual size, and the extra fields matter
// to an examiner even when they do not affect decoding: a disk's CHS geometry
// constrains how a legacy guest partitioned it, and a physical sector size of
// 4096 changes what alignment the guest's filesystem was built for.
//
// A zero field means the image did not record that value. It does not mean
// zero. VHDX has no CHS geometry at all, so Cylinders, Heads and
// SectorsPerTrack are always zero there, and HasCHS distinguishes the two
// cases.
type Geometry struct {
	// VirtualSize is the device size presented to a guest, in bytes.
	VirtualSize uint64

	// BlockSize is the allocation unit: the span one block allocation table
	// entry covers. A fixed VHD has no blocks and reports its whole size.
	BlockSize uint32

	// LogicalSectorSize is the sector size the guest sees, and
	// PhysicalSectorSize the one the image claims to sit on. They differ on a
	// 512e disk, where a guest addressing 512-byte sectors is really writing to
	// 4096-byte media.
	LogicalSectorSize  uint32
	PhysicalSectorSize uint32

	// Cylinders, Heads and SectorsPerTrack are the CHS geometry, VHD only.
	Cylinders       uint16
	Heads           uint8
	SectorsPerTrack uint8
}

// HasCHS reports whether the image recorded a CHS geometry.
//
// This is not the same as the fields being non-zero: a VHD may legitimately
// record a zero geometry, and that is a different fact from VHDX having no
// such field.
func (g Geometry) HasCHS() bool {
	return g.Cylinders != 0 || g.Heads != 0 || g.SectorsPerTrack != 0
}

// CHSCapacity returns the byte capacity the CHS geometry describes, and whether
// a geometry was recorded at all.
//
// A CHS capacity smaller than VirtualSize is normal rather than suspicious: the
// encoding tops out at about 127 GB, so every larger disk records a geometry
// that cannot describe it. The comparison is worth making at smaller sizes,
// where a disagreement means the footer is internally inconsistent.
func (g Geometry) CHSCapacity() (uint64, bool) {
	if !g.HasCHS() {
		return 0, false
	}
	sectorSize := uint64(g.LogicalSectorSize)
	if sectorSize == 0 {
		sectorSize = types.DefaultSectorSize
	}
	return uint64(g.Cylinders) * uint64(g.Heads) * uint64(g.SectorsPerTrack) * sectorSize, true
}

// Geometry returns the physical shape this image reports for itself.
func (d *VirtualDisk) Geometry() Geometry {
	g := Geometry{
		VirtualSize:       d.virtualSize,
		BlockSize:         d.blockSize,
		LogicalSectorSize: d.sectorSize,
	}

	if d.footer != nil {
		g.Cylinders = d.footer.Cylinders
		g.Heads = d.footer.Heads
		g.SectorsPerTrack = d.footer.SectorsPerTrack
		// VHD has no physical sector size field; the format is defined on
		// 512-byte sectors throughout.
		g.PhysicalSectorSize = types.DefaultSectorSize
	}

	if d.metaValues != nil {
		g.PhysicalSectorSize = d.metaValues.PhysicalSectorSize
	}

	return g
}

// Provenance describes what produced an image, when, and how much of that can
// be trusted.
//
// A forensic report has to be self-describing: given only the report, a reader
// must be able to say which tool wrote the image, which tool read it, and
// whether anything about the read was unusual. Every field here exists to
// answer one of those.
//
// Fields an image does not record are left zero rather than guessed.
type Provenance struct {
	// Format and DiskType are what the image says it is.
	Format   types.FileFormat
	DiskType types.DiskType

	// CreatorApplication identifies the producer. For VHD it is the footer's
	// four-byte code -- "win " for Hyper-V, "qemu" for qemu-img, "vpc " for
	// Virtual PC -- and for VHDX it is the creator string, which is free text.
	CreatorApplication string

	// CreatorVersion is the producer's version, VHD only, packed as two 16-bit
	// halves. CreatorVersionString renders it.
	CreatorVersion uint32

	// CreatorOS is the producer's host operating system as a four-byte code,
	// VHD only: "Wi2k" for Windows, "Mac " for Macintosh.
	CreatorOS string

	// Created is the image's creation time. It is the zero Time when the image
	// recorded none, which is a different fact from an epoch timestamp.
	Created time.Time

	// Identifier is the image's own GUID, and DataWriteIdentifier the GUID
	// VHDX rewrites on every modification -- which is what a differencing child
	// records to name its parent.
	Identifier          string
	DataWriteIdentifier string

	// OriginalSize is the disk's size at creation, where Geometry.VirtualSize
	// is its size now. They differ on an image that has been expanded, and this
	// is the only record that the expansion happened. VHD only.
	OriginalSize uint64

	// Features is the VHD footer's feature flag word.
	Features uint32

	// SavedState marks a VHD saved from a running machine. Its filesystem is a
	// crash-consistent snapshot rather than a cleanly unmounted one, which
	// changes what its contents can be taken to mean.
	SavedState bool

	// LeaveBlocksAllocated is the VHDX file parameter saying blocks are never
	// returned to the free pool once written. On such an image an allocated
	// block does not imply live data, so an extent map overstates what is in
	// use.
	LeaveBlocksAllocated bool

	// FooterSource says which copy of the VHD footer the image was opened from.
	// Anything but FooterSourceTrailing means the image was recovered.
	FooterSource FooterSource

	// HasLog and LogReplayed describe the VHDX log. An image with a log that
	// was not replayed may not reflect its last committed state.
	HasLog      bool
	LogReplayed bool

	// Path is the file the image was opened from, when it was opened from one.
	Path string

	// FileSize is the size of the backing file in bytes, as distinct from the
	// virtual size of the device it describes.
	FileSize int64

	// Warnings are the caveats raised for this image alone. Disk.Warnings
	// covers the whole chain; this field is deliberately per-image so that a
	// chain's record attributes each caveat to the disk it came from.
	Warnings []Warning
}

// CreatorVersionString renders the packed creator version as "major.minor".
// It returns "" when no version was recorded.
func (p Provenance) CreatorVersionString() string {
	if p.CreatorVersion == 0 {
		return ""
	}
	return formatVersionPair(uint16(p.CreatorVersion>>16), uint16(p.CreatorVersion))
}

// Provenance returns what is known about this image's origin.
//
// It describes this disk alone. For a differencing chain, call it on each entry
// of Chain(): the child and its parents were often produced by different tools
// at different times, and flattening that into one record loses the detail that
// makes a chain explicable.
func (d *VirtualDisk) Provenance() Provenance {
	p := Provenance{
		Format:       d.format,
		DiskType:     d.diskType,
		Path:         d.path,
		FileSize:     d.fileSize,
		FooterSource: d.footerSource,
		HasLog:       d.hasLog,
		LogReplayed:  d.replay != nil,
		Warnings:     d.ownWarnings(),
	}

	if id := d.Identifier(); id != ([16]byte{}) {
		p.Identifier = binaryutil.GUIDToString(id)
	}
	if id := d.DataWriteIdentifier(); id != ([16]byte{}) {
		p.DataWriteIdentifier = binaryutil.GUIDToString(id)
	}

	if d.footer != nil {
		p.CreatorApplication = trimCreatorCode(d.footer.CreatorApp)
		p.CreatorVersion = d.footer.CreatorVersion
		p.CreatorOS = trimCreatorCode(d.footer.CreatorOS)
		p.Created = d.footer.ModTime
		p.OriginalSize = d.footer.DataSize
		p.Features = d.footer.Features
		p.SavedState = d.footer.SavedState
	}

	if d.metaValues != nil {
		p.LeaveBlocksAllocated = !d.metaValues.LeaveBitsUnallocated
	}
	if d.creator != "" {
		p.CreatorApplication = d.creator
	}

	return p
}

// ownWarnings returns the warnings raised for this disk alone, with the path
// filled in. Warnings covers the whole chain; this is the per-disk view a
// per-disk provenance record needs.
func (d *VirtualDisk) ownWarnings() []Warning {
	if len(d.warnings) == 0 {
		return nil
	}
	out := make([]Warning, 0, len(d.warnings))
	for _, w := range d.warnings {
		if w.Path == "" {
			w.Path = d.path
		}
		out = append(out, w)
	}
	return out
}

// trimCreatorCode strips the padding from a four-byte creator code.
//
// The codes are space-padded ("vpc ", "win "), and some producers pad with NUL
// instead, which would otherwise end up embedded in a report.
func trimCreatorCode(s string) string {
	end := len(s)
	for end > 0 && (s[end-1] == ' ' || s[end-1] == 0) {
		end--
	}
	start := 0
	for start < end && s[start] == 0 {
		start++
	}
	return s[start:end]
}

// formatVersionPair renders a packed major/minor version.
func formatVersionPair(major, minor uint16) string {
	return strconv.Itoa(int(major)) + "." + strconv.Itoa(int(minor))
}

// GUIDString renders a 16-byte GUID in the canonical RFC 4122 form.
//
// Both VHD and VHDX store the first three fields of a GUID little-endian and
// the rest big-endian. Printing the bytes in order therefore produces a string
// that looks like a GUID and names a different one, which is a mistake every
// caller makes once. Exporting the conversion is cheaper than each of them
// getting it wrong.
func GUIDString(guid [16]byte) string {
	return binaryutil.GUIDToString(guid)
}

// ParseGUID parses a GUID in the canonical form GUIDString produces, so a value
// can survive a round trip through a report.
func ParseGUID(s string) ([16]byte, error) {
	return binaryutil.GUIDFromString(s)
}
