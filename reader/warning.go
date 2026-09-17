// SPDX-License-Identifier: MIT

package reader

import "fmt"

// WarningKind names a class of warning. It is a string so that a report can
// carry it verbatim and a consumer can match on it without depending on this
// package's iota ordering.
type WarningKind string

const (
	// WarningFooterRecovered means a VHD was opened from a fallback footer copy
	// rather than the conformant trailing one, so the image is damaged even
	// though it opened.
	WarningFooterRecovered WarningKind = "footer_recovered"

	// WarningParentTimestampMismatch means a differencing child records a
	// different modification time for its parent than the parent file actually
	// carries.
	//
	// This is a warning rather than an error on purpose. Filesystem timestamps
	// do not survive a copy, so a chain moved between machines mismatches
	// routinely and is still the right chain. What the warning is for is the
	// other case: a parent that has been modified since the child was created,
	// which makes the reconstructed device wrong in a way nothing else detects.
	WarningParentTimestampMismatch WarningKind = "parent_timestamp_mismatch"

	// WarningParentIdentityUnverifiable means a child records no parent
	// identifier, so the only check its parent could be held to was virtual
	// size -- which any image of the same size satisfies.
	WarningParentIdentityUnverifiable WarningKind = "parent_identity_unverifiable"

	// WarningParentIdentityUnchecked means identity verification was disabled
	// by Options.AllowParentGUIDMismatch.
	WarningParentIdentityUnchecked WarningKind = "parent_identity_unchecked"
)

// Warning records something suspicious about an image or its parent chain that
// does not prevent it from being read.
//
// Warnings exist because refusing to open a damaged image and opening it
// silently are both wrong for forensic use. The first loses recoverable
// evidence; the second presents a recovered or unverified reconstruction as an
// intact one. A warning lets the image be read and makes the caveat part of the
// record.
type Warning struct {
	// Kind classifies the warning.
	Kind WarningKind

	// Path is the image the warning concerns, when it was opened from one.
	Path string

	// Detail is a human-readable explanation. It is meant to be shown, not
	// parsed; match on Kind instead.
	Detail string
}

// String renders the warning for a log line or a report.
func (w Warning) String() string {
	if w.Path == "" {
		return fmt.Sprintf("%s: %s", w.Kind, w.Detail)
	}
	return fmt.Sprintf("%s [%s]: %s", w.Kind, w.Path, w.Detail)
}

// warn records a warning against this disk. Path is filled in when the warning
// is read, since a disk often learns its own path after it is opened.
func (d *VirtualDisk) warn(kind WarningKind, format string, args ...any) {
	d.warnings = append(d.warnings, Warning{
		Kind:   kind,
		Detail: fmt.Sprintf(format, args...),
	})
}

// Warnings returns every warning raised for this disk and for each parent
// attached below it, in chain order.
//
// A differencing disk is only as trustworthy as the chain behind it, so a
// caller recording provenance wants the whole chain's caveats, not just the
// leaf's. Each warning carries the path of the image it concerns.
//
// The result is a fresh slice; modifying it does not affect the disk.
func (d *VirtualDisk) Warnings() []Warning {
	var out []Warning
	for cur := d; cur != nil; cur = cur.parent {
		for _, w := range cur.warnings {
			if w.Path == "" {
				w.Path = cur.path
			}
			out = append(out, w)
		}
	}
	return out
}

// HasWarnings reports whether Warnings would return anything.
func (d *VirtualDisk) HasWarnings() bool {
	for cur := d; cur != nil; cur = cur.parent {
		if len(cur.warnings) > 0 {
			return true
		}
	}
	return false
}
