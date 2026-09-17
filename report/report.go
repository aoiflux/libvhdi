// SPDX-License-Identifier: MIT

// Package report defines versioned JSON documents describing VHD and VHDX
// images.
//
// # Why these types are separate from the parse structs
//
// The structures in types are how the formats are laid out on disk. Hanging
// json tags on them would tie the on-disk parse to the on-the-wire schema, so
// renaming a parser field would silently break every stored report. These are
// separate data transfer objects, converted from the reader's view, and they
// change only when the schema is meant to.
//
// # What makes a report forensically usable
//
// A report has to be readable years later by a tool that was not built against
// the library that produced it. That imposes three things, and every document
// here carries all three:
//
//   - A schema version, so a consumer can refuse a document it does not
//     understand rather than misread it.
//   - Provenance: which image, how large, which library version, when. A report
//     that does not identify its own input is an assertion without a subject.
//   - The caveats. If the image was recovered from a fallback footer, or a
//     parent could not be verified, the document says so. A report that
//     presents a recovered reconstruction as an intact read is worse than no
//     report.
//
// # Zero dependencies
//
// This package is part of the core module and imports only the standard library
// and libvhdi's own packages. That is deliberate: the file-level ChangeReport
// types live here too, so a pipeline that reads and validates change reports
// never has to import a filesystem library. Only the code that *produces* them
// does.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aoiflux/libvhdi/internal/buildinfo"
	"github.com/aoiflux/libvhdi/reader"
)

// SchemaVersion is the version of every document this package produces.
//
// It increments when a field is removed, renamed, or changes meaning. Adding a
// field does not increment it, so a consumer that ignores unknown fields keeps
// working across additive changes.
const SchemaVersion = 1

// Kind names the type of a document, so a consumer can dispatch on one field
// without guessing from the shape.
type Kind string

const (
	KindDisk       Kind = "disk"
	KindChain      Kind = "chain"
	KindCheckpoint Kind = "checkpoint"
	KindAllocation Kind = "allocation"
	KindIntegrity  Kind = "integrity"
	KindChange     Kind = "change"
)

// Header is common to every document.
type Header struct {
	// SchemaVersion is the schema this document was written against.
	SchemaVersion int `json:"schema_version"`

	// Kind names the document type.
	Kind Kind `json:"kind"`

	// Generated is when the document was produced, in UTC.
	Generated time.Time `json:"generated"`

	// Library identifies the code that produced it.
	Library Library `json:"library"`
}

// Library identifies the producing code, so a report can be attributed.
type Library struct {
	// Module is the Go module path, and Version its version as recorded in the
	// producing binary's build information.
	//
	// Version can be "(devel)" or "unknown". Those are honest answers: a report
	// generator must be able to record that its own version could not be
	// determined rather than fail or invent one.
	Module  string `json:"module"`
	Version string `json:"version"`
}

func newHeader(kind Kind) Header {
	return Header{
		SchemaVersion: SchemaVersion,
		Kind:          kind,
		Generated:     time.Now().UTC(),
		Library: Library{
			Module:  buildinfo.ModulePath,
			Version: buildinfo.Version(),
		},
	}
}

// Source identifies the image a document describes.
//
// A report that does not identify its own input is an assertion with no
// subject, so every document carries one of these.
type Source struct {
	// Path is the file the image was opened from, empty when it was opened from
	// a reader that names no file.
	Path string `json:"path,omitempty"`

	// FileSize is the length of the backing file in bytes, which is distinct
	// from the virtual size of the device it describes.
	FileSize int64 `json:"file_size"`

	// SHA256 is the digest of the image *file*, lowercase hex, present only when
	// the caller asked for it. It is the file's digest, not the decoded device's
	// -- two images with identical contents but different block layouts have
	// different file digests and the same device digest.
	SHA256 string `json:"sha256,omitempty"`
}

// Warning is a caveat about an image or its chain that did not prevent it being
// read.
type Warning struct {
	// Kind classifies the warning. Match on this rather than on Message.
	Kind string `json:"kind"`

	// Path is the image the warning concerns.
	Path string `json:"path,omitempty"`

	// Message is a human-readable explanation, meant to be shown rather than
	// parsed.
	Message string `json:"message"`
}

func convertWarnings(in []reader.Warning) []Warning {
	if len(in) == 0 {
		return nil
	}
	out := make([]Warning, 0, len(in))
	for _, w := range in {
		out = append(out, Warning{
			Kind:    string(w.Kind),
			Path:    w.Path,
			Message: w.Detail,
		})
	}
	return out
}

// Geometry is the physical shape an image reports for itself.
type Geometry struct {
	VirtualSize        uint64 `json:"virtual_size"`
	VirtualSizeHuman   string `json:"virtual_size_human"`
	BlockSize          uint32 `json:"block_size"`
	LogicalSectorSize  uint32 `json:"logical_sector_size"`
	PhysicalSectorSize uint32 `json:"physical_sector_size"`

	// HasCHS distinguishes a VHD that recorded a zero geometry from a VHDX,
	// which has no CHS field at all. Those are different facts.
	HasCHS          bool   `json:"has_chs"`
	Cylinders       uint16 `json:"cylinders,omitempty"`
	Heads           uint8  `json:"heads,omitempty"`
	SectorsPerTrack uint8  `json:"sectors_per_track,omitempty"`
}

func convertGeometry(g reader.Geometry) Geometry {
	out := Geometry{
		VirtualSize:        g.VirtualSize,
		VirtualSizeHuman:   humanBytes(int64(g.VirtualSize)),
		BlockSize:          g.BlockSize,
		LogicalSectorSize:  g.LogicalSectorSize,
		PhysicalSectorSize: g.PhysicalSectorSize,
		HasCHS:             g.HasCHS(),
	}
	if out.HasCHS {
		out.Cylinders = g.Cylinders
		out.Heads = g.Heads
		out.SectorsPerTrack = g.SectorsPerTrack
	}
	return out
}

// Provenance describes what produced an image and how much of the read can be
// trusted.
type Provenance struct {
	Format   string `json:"format"`
	DiskType string `json:"disk_type"`

	CreatorApplication string `json:"creator_application,omitempty"`
	CreatorVersion     string `json:"creator_version,omitempty"`
	CreatorOS          string `json:"creator_os,omitempty"`

	// Created is the image's own creation timestamp, omitted when the image
	// recorded none. That absence is itself a fact, and inventing an epoch date
	// to fill it would state something the image does not say.
	Created *time.Time `json:"created,omitempty"`

	// Identifier is the image's own GUID and DataWriteIdentifier the GUID VHDX
	// rewrites on every modification -- which is what a differencing child
	// records to name its parent. Both are canonical strings rather than byte
	// arrays, which would serialise as sixteen numbers.
	Identifier          string `json:"identifier,omitempty"`
	DataWriteIdentifier string `json:"data_write_identifier,omitempty"`

	// OriginalSize is the disk's size at creation where the geometry's virtual
	// size is its size now. A difference is the only record of an expansion.
	OriginalSize uint64 `json:"original_size,omitempty"`

	// SavedState marks a VHD saved from a running machine, whose filesystem is
	// crash-consistent rather than cleanly unmounted.
	SavedState bool `json:"saved_state"`

	// LeaveBlocksAllocated marks a VHDX where blocks are never returned to the
	// free pool, so an allocated block does not imply live data.
	LeaveBlocksAllocated bool `json:"leave_blocks_allocated"`

	// FooterSource says which copy of the VHD footer was used. Anything but
	// "trailing" means the image was recovered rather than read intact.
	FooterSource    string `json:"footer_source,omitempty"`
	FooterRecovered bool   `json:"footer_recovered"`

	// HasLog and LogReplayed describe the VHDX log state.
	HasLog      bool `json:"has_log"`
	LogReplayed bool `json:"log_replayed"`
}

func convertProvenance(p reader.Provenance) Provenance {
	out := Provenance{
		Format:               p.Format.String(),
		DiskType:             p.DiskType.String(),
		CreatorApplication:   p.CreatorApplication,
		CreatorVersion:       p.CreatorVersionString(),
		CreatorOS:            p.CreatorOS,
		Identifier:           p.Identifier,
		DataWriteIdentifier:  p.DataWriteIdentifier,
		OriginalSize:         p.OriginalSize,
		SavedState:           p.SavedState,
		LeaveBlocksAllocated: p.LeaveBlocksAllocated,
		HasLog:               p.HasLog,
		LogReplayed:          p.LogReplayed,
		FooterRecovered:      p.FooterSource.Recovered(),
	}
	if p.FooterSource != reader.FooterSourceUnknown {
		out.FooterSource = p.FooterSource.String()
	}
	if !p.Created.IsZero() {
		created := p.Created.UTC()
		out.Created = &created
	}
	return out
}

// ============================================================================
// Disk
// ============================================================================

// DiskReport describes a single image.
type DiskReport struct {
	// Header is embedded, so its fields appear at the top level of the
	// document rather than nested under a key.
	Header

	Source     Source     `json:"source"`
	Geometry   Geometry   `json:"geometry"`
	Provenance Provenance `json:"provenance"`

	// NeedsParent reports a differencing image whose chain is incomplete.
	// Reads into the missing ranges fail rather than returning zeroes.
	NeedsParent       bool   `json:"needs_parent"`
	ParentIdentifier  string `json:"parent_identifier,omitempty"`
	ParentFilename    string `json:"parent_filename,omitempty"`
	ParentResolveNote string `json:"parent_resolve_note,omitempty"`

	// Warnings are the caveats for this image and its chain.
	Warnings []Warning `json:"warnings,omitempty"`
}

// Options controls what a report includes.
type Options struct {
	// Hash computes the SHA-256 of each image file.
	//
	// It is off by default because it reads every byte of every file in the
	// chain, which on a chain of large images is minutes of I/O -- a cost a
	// caller should choose rather than discover.
	Hash bool
}

func (o *Options) hash() bool { return o != nil && o.Hash }

// Disk builds a report describing one image.
func Disk(d *reader.VirtualDisk, opts *Options) (*DiskReport, error) {
	if d == nil {
		return nil, fmt.Errorf("report: nil disk")
	}

	src, err := sourceOf(d, opts)
	if err != nil {
		return nil, err
	}

	r := &DiskReport{
		Header:         newHeader(KindDisk),
		Source:         src,
		Geometry:       convertGeometry(d.Geometry()),
		Provenance:     convertProvenance(d.Provenance()),
		NeedsParent:    d.NeedsParent(),
		ParentFilename: d.ParentFilename(),
		Warnings:       convertWarnings(d.Warnings()),
	}

	if id := d.ParentIdentifier(); id != ([16]byte{}) {
		r.ParentIdentifier = reader.GUIDString(id)
	}
	if err := d.ParentResolveError(); err != nil {
		r.ParentResolveNote = err.Error()
	}

	return r, nil
}

// sourceOf describes the file behind a disk, hashing it when asked.
func sourceOf(d *reader.VirtualDisk, opts *Options) (Source, error) {
	p := d.Provenance()
	src := Source{Path: p.Path, FileSize: p.FileSize}

	if !opts.hash() || p.Path == "" {
		return src, nil
	}

	digest, err := hashFile(p.Path)
	if err != nil {
		return Source{}, fmt.Errorf("hashing %q: %w", p.Path, err)
	}
	src.SHA256 = digest
	return src, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ============================================================================
// Chain
// ============================================================================

// ChainLink is one image in a differencing chain.
type ChainLink struct {
	// Index is the link's position: 0 is the disk the report was built from,
	// 1 its parent, and so on.
	Index int `json:"index"`

	Path             string `json:"path,omitempty"`
	Format           string `json:"format"`
	DiskType         string `json:"disk_type"`
	Identifier       string `json:"identifier,omitempty"`
	ParentIdentifier string `json:"parent_identifier,omitempty"`
	ParentFilename   string `json:"parent_filename,omitempty"`
	VirtualSize      uint64 `json:"virtual_size"`
	IsDifferencing   bool   `json:"is_differencing"`
	HasLog           bool   `json:"has_log"`
	LogReplayed      bool   `json:"log_replayed"`

	// Role is this link's part in a Hyper-V chain: "base" or "checkpoint".
	Role string `json:"role"`
}

// ChainReport describes the set of files that together constitute one device,
// which is what an evidence record has to name.
type ChainReport struct {
	// Header is embedded, so its fields appear at the top level of the
	// document rather than nested under a key.
	Header

	Source Source `json:"source"`

	// Links are the images, leaf first.
	Links []ChainLink `json:"links"`

	// Complete reports whether every link the chain needs is attached. When
	// false the device cannot be fully read, and the ranges backed by the
	// missing images fail rather than returning zeroes.
	Complete bool `json:"complete"`

	// IncompleteReason explains a false Complete.
	IncompleteReason string `json:"incomplete_reason,omitempty"`

	Warnings []Warning `json:"warnings,omitempty"`
}

// Chain builds a report describing a disk's differencing chain.
func Chain(d *reader.VirtualDisk, opts *Options) (*ChainReport, error) {
	if d == nil {
		return nil, fmt.Errorf("report: nil disk")
	}

	src, err := sourceOf(d, opts)
	if err != nil {
		return nil, err
	}

	r := &ChainReport{
		Header:   newHeader(KindChain),
		Source:   src,
		Complete: d.ChainComplete(),
		Warnings: convertWarnings(d.Warnings()),
	}

	for _, e := range d.Chain() {
		link := ChainLink{
			Index:          e.Index,
			Path:           e.Path,
			Format:         e.Format.String(),
			DiskType:       e.DiskType.String(),
			ParentFilename: e.ParentFilename,
			VirtualSize:    e.VirtualSize,
			IsDifferencing: e.IsDifferencing,
			HasLog:         e.HasLog,
			LogReplayed:    e.LogReplayed,
			Role:           roleOf(e.IsDifferencing),
		}
		if e.Identifier != ([16]byte{}) {
			link.Identifier = reader.GUIDString(e.Identifier)
		}
		if e.ParentIdentifier != ([16]byte{}) {
			link.ParentIdentifier = reader.GUIDString(e.ParentIdentifier)
		}
		r.Links = append(r.Links, link)
	}

	if !r.Complete {
		if err := d.ParentResolveError(); err != nil {
			r.IncompleteReason = err.Error()
		} else {
			r.IncompleteReason = "a differencing image in the chain has no parent attached"
		}
	}

	return r, nil
}

func roleOf(differencing bool) string {
	if differencing {
		return reader.RoleCheckpoint.String()
	}
	return reader.RoleBase.String()
}

// ============================================================================
// Encoding
// ============================================================================

// Write encodes a document as indented JSON.
//
// Any of this package's report types can be passed. Indentation is not
// negotiable: these documents are read by people as often as by machines, and a
// single-line megabyte of JSON is not readable by either.
func Write(w io.Writer, doc any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(doc)
}

// humanBytes renders a size for a person, alongside the exact number rather
// than instead of it.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
