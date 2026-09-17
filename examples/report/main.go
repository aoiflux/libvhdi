// SPDX-License-Identifier: MIT

// report emits a versioned JSON document describing a VHD or VHDX image.
//
// Usage:
//
//	go run ./examples/report -kind disk       <disk.vhdx>
//	go run ./examples/report -kind chain      <disk.vhdx>
//	go run ./examples/report -kind allocation <disk.vhdx>
//	go run ./examples/report -kind integrity  <disk.vhdx>
//	go run ./examples/report -kind change     -since 1 <checkpoint.avhdx>
//
// The integrity report is the one worth knowing about: it is the only kind that
// describes an image which will not open, saying which of its structures
// survived.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/aoiflux/libvhdi"
	"github.com/aoiflux/libvhdi/report"
)

func main() {
	kind := flag.String("kind", "disk", "disk, chain, allocation, integrity or change")
	hash := flag.Bool("hash", false, "compute the SHA-256 of each image file (reads every byte)")
	extents := flag.Bool("extents", false, "include the full extent list")
	since := flag.Int("since", 1, "for -kind change: the chain index to measure changes since")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: report -kind <kind> [-hash] [-extents] <disk>")
		os.Exit(2)
	}

	if err := run(*kind, flag.Arg(0), *hash, *extents, *since); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(kind, path string, hash, extents bool, since int) error {
	ctx := context.Background()
	base := report.Options{Hash: hash}

	// Integrity takes a path rather than an open disk, because the interesting
	// case is an image that cannot be opened at all.
	if kind == "integrity" {
		doc, err := report.Integrity(ctx, path, &base)
		if err != nil {
			return err
		}
		return report.Write(os.Stdout, doc)
	}

	disk, err := libvhdi.OpenFile(path)
	if err != nil {
		return err
	}
	defer disk.Close()

	switch kind {
	case "disk":
		doc, err := report.Disk(disk, &base)
		if err != nil {
			return err
		}
		return report.Write(os.Stdout, doc)

	case "chain":
		doc, err := report.Chain(disk, &base)
		if err != nil {
			return err
		}
		return report.Write(os.Stdout, doc)

	case "allocation":
		doc, err := report.Allocation(ctx, disk, &report.AllocationOptions{
			Options:        base,
			IncludeExtents: extents,
		})
		if err != nil {
			return err
		}
		return report.Write(os.Stdout, doc)

	case "change":
		// A block-tier document: which byte ranges the disks nearer the leaf
		// than -since wrote. It says nothing about files, and its tier field
		// makes that unambiguous -- an absent volumes list means no filesystem
		// analysis was run, not that no files changed.
		doc, err := report.Change(ctx, disk, since, &report.ChangeOptions{
			Options:        base,
			IncludeExtents: extents,
		})
		if err != nil {
			return err
		}
		return report.Write(os.Stdout, doc)

	default:
		return fmt.Errorf("unknown kind %q: want disk, chain, allocation, integrity or change", kind)
	}
}
