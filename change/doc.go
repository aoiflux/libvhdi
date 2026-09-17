// SPDX-License-Identifier: MIT

// Package change turns libvhdi's block-level view of what a checkpoint wrote
// into a file-level one.
//
// libvhdi can say which byte ranges of a virtual disk a checkpoint wrote. That
// is a complete answer for delta imaging or targeted acquisition, and it needs
// no filesystem knowledge, which is why it lives in the core module and costs
// nothing. Saying *which files* those ranges belong to needs a filesystem
// parser, and that is what this module adds.
//
// # Why this is a separate module
//
// The core libvhdi module has no dependencies and must never acquire one. A
// consumer who wants only VHD/VHDX parsing has to be able to take it without
// absorbing six filesystem libraries into their module graph -- not in go.mod,
// not in go.sum, not transitively. So the adapters live here, in
// github.com/aoiflux/libvhdi/change, and not importing this module costs
// nothing at all.
//
// The contract between the two is [vhdimap], which is in the core and is
// interfaces only. A consumer who wants file-level change detection over their
// own filesystem parser implements that contract and never imports this module
// either.
//
// # How the comparison works
//
// The naive approach inventories both volume states and compares them, which
// means reading a terabyte to find a megabyte of changes. This one inverts it:
//
//  1. libvhdi gives the exact disk ranges the newer disks wrote.
//  2. The partition table says which volume holds each range.
//  3. Each affected volume is opened at both states.
//  4. A file is examined only when its extent map intersects a written range.
//  5. Added, deleted, modified and renamed are classified by file identity.
//  6. Where a journal exists, the renames are checked against it.
//
// Step 4 is what makes it viable, and it is why per-file extent enumeration is
// the capability [vhdimap.Filesystem] is built around.
//
// # Coordinates
//
// Every byte range here is absolute within the disk. libvhdi's changed ranges
// are, and each adapter opens its filesystem with the partition's base offset
// so that the filesystem's are too. Nothing in this package adds or subtracts a
// base offset, because that arithmetic is the one mistake in this design that
// produces a confident wrong answer rather than an error: a volume-relative
// range compared against a whole-disk one still intersects, and every file
// appears changed, or none does.
//
// # What the result can be trusted to say
//
// A change is reported with a confidence, and the three levels are about what
// kind of evidence was available rather than about quality:
//
//   - [ConfidenceProven] -- a journal recorded the event. NTFS's USN journal is
//     the only source of this today.
//   - [ConfidenceIdentified] -- the filesystem keeps a real reuse counter, so
//     the file was followed across both states by an identity the filesystem
//     maintains. ext and XFS are here, and so is NTFS without a journal.
//   - [ConfidenceInferred] -- the identity was synthesised, because the format
//     records none. FAT, exFAT and HFS+ are here.
//
// [VolumeDiff.Notes] states the limits in prose, so a report can be read by
// someone who did not write the code that produced it.
//
// # Read-only
//
// No code path here writes to an image. Every reader handed to a filesystem
// library is wrapped so that it implements io.ReaderAt and nothing else, so a
// library that opportunistically type-asserts to io.WriterAt cannot find one --
// regardless of what the library does or later starts doing.
//
// # Deletions on VHD
//
// A VHD differencing disk has no block state meaning zero, so a region cleared
// on a VHD chain reads back as the parent's old contents and there is nothing
// to detect. Deletion-aware change tracking requires VHDX. This is a limit of
// the format rather than of the analysis, and it is stated in the document this
// package produces.
package change
