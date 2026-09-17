// SPDX-License-Identifier: MIT

package report

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/aoiflux/libvhdi/reader"
	"github.com/aoiflux/libvhdi/types"
)

// CheckStatus is the outcome of one structural check.
type CheckStatus string

const (
	// StatusPass means the structure was present and verified.
	StatusPass CheckStatus = "pass"

	// StatusFail means the structure was present and did not verify.
	StatusFail CheckStatus = "fail"

	// StatusAbsent means the structure does not exist in this image. A VHDX has
	// no footer; a fixed VHD has no dynamic header. Absent is not a failure,
	// and a report that conflated the two would show every VHDX failing half
	// its checks.
	StatusAbsent CheckStatus = "absent"

	// StatusNotApplicable means the structure exists but this check does not
	// apply to it.
	StatusNotApplicable CheckStatus = "not_applicable"
)

// Check is one structural verification and its outcome.
//
// Opening an image already verifies most of these, but pass or fail is all a
// caller learns: the open succeeds or it does not. An integrity report makes
// each verdict individually reportable, which is what lets an examiner say
// *which* structure of a damaged image is intact.
type Check struct {
	// Name identifies the structure, spelled as the specification spells it.
	Name string `json:"name"`

	// Status is the outcome.
	Status CheckStatus `json:"status"`

	// Offset is where the structure lives, when it has a fixed location.
	Offset int64 `json:"offset,omitempty"`

	// Detail explains a failure, or adds context to a pass.
	Detail string `json:"detail,omitempty"`
}

// IntegrityReport records the per-structure verdicts for one image.
type IntegrityReport struct {
	// Header is embedded, so its fields appear at the top level of the
	// document rather than nested under a key.
	Header

	Source Source `json:"source"`
	Format string `json:"format"`

	// Checks are the individual verdicts, in the order the structures are read.
	Checks []Check `json:"checks"`

	// Healthy is true when no check failed. An absent structure does not make
	// an image unhealthy.
	Healthy bool `json:"healthy"`

	// Opened reports whether the image could be opened at all. A false value
	// with a populated Checks list is the interesting case: it says which
	// structures survived.
	Opened bool `json:"opened"`

	// OpenError explains a false Opened.
	OpenError string `json:"open_error,omitempty"`

	Warnings []Warning `json:"warnings,omitempty"`
}

func (r *IntegrityReport) add(c Check) {
	r.Checks = append(r.Checks, c)
	if c.Status == StatusFail {
		r.Healthy = false
	}
}

// Integrity verifies an image's structures one at a time and records each
// verdict.
//
// Unlike the other reports this takes a path rather than an open disk, because
// the interesting case is an image that will not open. Checking each structure
// independently is what turns "this file is corrupt" into "the trailing footer
// is gone but the mirror and the dynamic header are intact", which is the
// difference between an image being written off and being recovered.
func Integrity(ctx context.Context, path string, opts *Options) (*IntegrityReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	r := &IntegrityReport{
		Header:  newHeader(KindIntegrity),
		Source:  Source{Path: path, FileSize: info.Size()},
		Healthy: true,
	}

	if opts.hash() {
		digest, err := hashFile(path)
		if err != nil {
			return nil, fmt.Errorf("hashing %q: %w", path, err)
		}
		r.Source.SHA256 = digest
	}

	// Probing first establishes the format without needing the image to open
	// fully, so a badly damaged file still gets categorised.
	probed, probeErr := reader.Probe(f, info.Size())
	if probeErr == nil {
		r.Format = probed.Format.String()
	}

	switch {
	case probeErr == nil && probed.Format == types.FileFormatVHDX:
		checkVHDX(f, info.Size(), r)
	default:
		checkVHD(f, info.Size(), r)
	}

	// A full open exercises the cross-checks the individual reads do not: the
	// geometry against the file size, the BAT against the block count, the log
	// against the headers.
	d, openErr := reader.OpenFileWith(path, &reader.Options{
		ParentResolver: reader.NoParentResolution,
	})
	if openErr != nil {
		r.Opened = false
		r.OpenError = openErr.Error()
		r.add(Check{
			Name:   "full open",
			Status: StatusFail,
			Detail: openErr.Error(),
		})
		return r, nil
	}
	defer d.Close()

	r.Opened = true
	r.Warnings = convertWarnings(d.Warnings())
	r.add(Check{Name: "full open", Status: StatusPass})

	if r.Format == "" {
		r.Format = d.Format().String()
	}

	return r, nil
}

// checkVHD verifies the structures of a VHD image.
func checkVHD(f *os.File, size int64, r *IntegrityReport) {
	fp := reader.NewVHDFooterParser(f)

	// The trailing footer is the conformant location.
	trailing := Check{Name: "footer (trailing)", Offset: size - 512}
	if size < 512 {
		trailing.Status = StatusAbsent
		trailing.Detail = "file is smaller than a footer"
	} else if _, err := fp.ReadFooterAt(size - 512); err != nil {
		trailing.Status = StatusFail
		trailing.Detail = err.Error()
	} else {
		trailing.Status = StatusPass
	}
	r.add(trailing)

	// The mirror at offset 0 exists only on dynamic and differencing disks, so
	// its absence on a fixed disk is not a failure.
	mirror := Check{Name: "footer (mirror at 0)", Offset: 0}
	mirrorFooter, mirrorErr := fp.ReadFooterAt(0)
	switch {
	case mirrorErr == nil && mirrorFooter.DiskType == types.DiskTypeFixed:
		mirror.Status = StatusAbsent
		mirror.Detail = "a fixed disk has no mirror footer; offset 0 is payload"
	case mirrorErr == nil:
		mirror.Status = StatusPass
	case trailing.Status == StatusPass:
		// Work out whether a mirror was supposed to be here at all.
		if footer, err := fp.ReadFooterAt(size - 512); err == nil && footer.DiskType == types.DiskTypeFixed {
			mirror.Status = StatusAbsent
			mirror.Detail = "a fixed disk has no mirror footer; offset 0 is payload"
		} else {
			mirror.Status = StatusFail
			mirror.Detail = mirrorErr.Error()
		}
	default:
		mirror.Status = StatusFail
		mirror.Detail = mirrorErr.Error()
	}
	r.add(mirror)

	// The dynamic disk header exists only on dynamic and differencing disks.
	footer, err := fp.ReadFooterAt(size - 512)
	if err != nil {
		footer, err = fp.ReadFooterAt(0)
	}
	dyn := Check{Name: "dynamic disk header"}
	switch {
	case err != nil:
		dyn.Status = StatusNotApplicable
		dyn.Detail = "no readable footer, so the header's location is unknown"
	case footer.DiskType == types.DiskTypeFixed:
		dyn.Status = StatusAbsent
		dyn.Detail = "a fixed disk has no dynamic disk header"
	default:
		dyn.Offset = footer.NextOffset
		if _, err := reader.NewVHDDynamicDiskHeaderParser(f).ReadHeaderAt(footer.NextOffset); err != nil {
			dyn.Status = StatusFail
			dyn.Detail = err.Error()
		} else {
			dyn.Status = StatusPass
		}
	}
	r.add(dyn)
}

// checkVHDX verifies the structures of a VHDX image.
func checkVHDX(f *os.File, size int64, r *IntegrityReport) {
	ihp := reader.NewVHDXImageHeaderParser(f)

	for _, h := range []struct {
		name   string
		offset int64
	}{
		{"header 1", types.VHDXFirstHeaderOffset},
		{"header 2", types.VHDXSecondHeaderOffset},
	} {
		c := Check{Name: h.name, Offset: h.offset}
		if _, err := ihp.ReadImageHeaderAt(h.offset); err != nil {
			c.Status = StatusFail
			c.Detail = err.Error()
		} else {
			c.Status = StatusPass
		}
		r.add(c)
	}

	rtp := reader.NewVHDXRegionTableParser(f)
	for _, t := range []struct {
		name   string
		offset int64
	}{
		{"region table 1", types.VHDXFirstRegionTableOffset},
		{"region table 2", types.VHDXSecondRegionTableOffset},
	} {
		c := Check{Name: t.name, Offset: t.offset}
		if _, err := rtp.ReadRegionTableAt(t.offset); err != nil {
			c.Status = StatusFail
			c.Detail = err.Error()
		} else {
			c.Status = StatusPass
		}
		r.add(c)
	}

	// Probing reads the metadata region, so its success or failure is the
	// metadata verdict.
	meta := Check{Name: "metadata"}
	info, err := reader.Probe(f, size)
	if err != nil {
		meta.Status = StatusFail
		meta.Detail = err.Error()
	} else {
		meta.Status = StatusPass
	}
	r.add(meta)

	// The log is optional. An image with one that cannot be replayed is not
	// corrupt -- it was captured mid-write -- but its structures may be stale,
	// which is a different and equally reportable state.
	log := Check{Name: "log"}
	switch {
	case err != nil:
		log.Status = StatusNotApplicable
		log.Detail = "metadata unreadable, so the log state is unknown"
	case !info.HasLog:
		log.Status = StatusAbsent
		log.Detail = "the image carries no log"
	default:
		// Opening without AllowDirtyImage fails when the log cannot be replayed.
		d, openErr := reader.OpenFileWith(f.Name(), &reader.Options{
			ParentResolver: reader.NoParentResolution,
		})
		switch {
		case openErr == nil:
			log.Status = StatusPass
			log.Detail = "log replayed into a read-only overlay"
			d.Close()
		case errors.Is(openErr, reader.ErrDirtyImage):
			log.Status = StatusFail
			log.Detail = "log present but not replayable; the image may be stale"
		default:
			log.Status = StatusFail
			log.Detail = openErr.Error()
		}
	}
	r.add(log)
}

// ============================================================================
// Checkpoint
// ============================================================================

// CheckpointNode is one image in a discovered chain tree.
type CheckpointNode struct {
	Path string `json:"path"`

	// Readable reports whether the image could be probed. An unreadable image
	// stays in the document rather than being dropped: a corrupt file in a
	// checkpoint directory is a finding, and it may be the link that would have
	// joined two halves of the tree.
	Readable bool   `json:"readable"`
	Error    string `json:"error,omitempty"`

	Format   string `json:"format,omitempty"`
	DiskType string `json:"disk_type,omitempty"`

	// Role is "base", "checkpoint" or "unknown".
	Role string `json:"role"`

	// HasCheckpointExtension reports Hyper-V's .avhdx naming convention.
	//
	// It is separate from Role because the extension is a convention, not a
	// format: a renamed differencing disk is still a checkpoint and a base disk
	// given the extension is not one. A disagreement between the two is worth
	// showing rather than resolving.
	HasCheckpointExtension bool `json:"has_checkpoint_extension"`

	Identifier       string `json:"identifier,omitempty"`
	LinkIdentity     string `json:"link_identity,omitempty"`
	ParentIdentifier string `json:"parent_identifier,omitempty"`
	ParentFilename   string `json:"parent_filename,omitempty"`

	VirtualSize uint64 `json:"virtual_size,omitempty"`
	FileSize    int64  `json:"file_size"`
	HasLog      bool   `json:"has_log"`

	// Depth is how many images lie between this one and its root.
	Depth int `json:"depth"`

	// ParentPath and ChildPaths give the tree's edges by file, which is how a
	// person reads it.
	ParentPath string   `json:"parent_path,omitempty"`
	ChildPaths []string `json:"child_paths,omitempty"`
}

// CheckpointLineage is one path through the tree, root first.
type CheckpointLineage struct {
	// Paths are the images, root first, that together constitute one device.
	Paths []string `json:"paths"`

	// Complete reports whether the lineage starts at a disk needing no parent.
	// False means the images present cannot reconstruct the device.
	Complete bool `json:"complete"`

	// CheckpointCount is how many differencing images the lineage holds.
	CheckpointCount int `json:"checkpoint_count"`
}

// CheckpointReport describes a directory of images and the chain tree they
// form.
type CheckpointReport struct {
	// Header is embedded, so its fields appear at the top level of the
	// document rather than nested under a key.
	Header

	// Directory is the folder that was scanned.
	Directory string `json:"directory"`

	// Nodes are every image found, ordered by path.
	Nodes []CheckpointNode `json:"nodes"`

	// RootPaths and LeafPaths name the ends of the tree.
	RootPaths []string `json:"root_paths"`
	LeafPaths []string `json:"leaf_paths"`

	// Branched reports more than one leaf.
	//
	// A branched tree has no single current disk. Which leaf a virtual machine
	// is using is recorded in its configuration, not in the disks, so this
	// document reports the branches and does not guess.
	Branched bool `json:"branched"`

	// Lineages is one entry per leaf: every distinct device the directory
	// describes.
	Lineages []CheckpointLineage `json:"lineages"`

	// SkippedFiles are files in the directory that are not disk images, such as
	// the .vmcx and .vmrs a Hyper-V machine folder holds. They are listed
	// separately so they do not read as failures.
	SkippedFiles []string `json:"skipped_files,omitempty"`

	// UnreadablePaths names the images that could not be probed.
	UnreadablePaths []string `json:"unreadable_paths,omitempty"`

	// Limitations records what this document cannot say, so its silence is not
	// mistaken for a finding.
	Limitations []string `json:"limitations,omitempty"`
}

// Checkpoint builds a report describing a directory of images.
func Checkpoint(ctx context.Context, dir string, opts *reader.DiscoverOptions) (*CheckpointReport, error) {
	tree, err := reader.DiscoverChain(ctx, dir, opts)
	if err != nil {
		return nil, err
	}

	r := &CheckpointReport{
		Header:       newHeader(KindCheckpoint),
		Directory:    tree.Dir,
		SkippedFiles: tree.Skipped,
		Branched:     tree.Branched(),
		Limitations: []string{
			"Checkpoint display names and which leaf is current are recorded in the " +
				"virtual machine's .vmcx configuration, which is an undocumented " +
				"proprietary format and is not parsed. This document describes the " +
				"disks alone.",
		},
	}

	for _, n := range tree.Nodes {
		node := CheckpointNode{
			Path:                   n.Path,
			Readable:               n.Readable(),
			Role:                   n.Role().String(),
			HasCheckpointExtension: n.Info.HasCheckpointExtension(),
			Depth:                  n.Depth(),
		}
		if n.Err != nil {
			node.Error = n.Err.Error()
			r.UnreadablePaths = append(r.UnreadablePaths, n.Path)
		} else {
			node.Format = n.Info.Format.String()
			node.DiskType = n.Info.DiskType.String()
			node.ParentFilename = n.Info.ParentFilename
			node.VirtualSize = n.Info.VirtualSize
			node.FileSize = n.Info.FileSize
			node.HasLog = n.Info.HasLog

			if n.Info.Identifier != ([16]byte{}) {
				node.Identifier = reader.GUIDString(n.Info.Identifier)
			}
			if n.Info.LinkIdentity != ([16]byte{}) {
				node.LinkIdentity = reader.GUIDString(n.Info.LinkIdentity)
			}
			if n.Info.ParentIdentifier != ([16]byte{}) {
				node.ParentIdentifier = reader.GUIDString(n.Info.ParentIdentifier)
			}
		}
		if n.Parent != nil {
			node.ParentPath = n.Parent.Path
		}
		for _, c := range n.Children {
			node.ChildPaths = append(node.ChildPaths, c.Path)
		}
		r.Nodes = append(r.Nodes, node)
	}

	for _, n := range tree.Roots() {
		r.RootPaths = append(r.RootPaths, n.Path)
	}
	for _, n := range tree.Leaves() {
		r.LeafPaths = append(r.LeafPaths, n.Path)
	}

	for _, l := range tree.Lineages() {
		r.Lineages = append(r.Lineages, CheckpointLineage{
			Paths:           l.Paths(),
			Complete:        l.Complete(),
			CheckpointCount: len(l.Checkpoints()),
		})
	}

	if r.Branched {
		r.Limitations = append(r.Limitations,
			"This tree has more than one leaf, so it describes more than one device. "+
				"Which is current cannot be determined from the disks.")
	}

	return r, nil
}
