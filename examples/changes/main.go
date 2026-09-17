// SPDX-License-Identifier: MIT

// changes reports which files a Hyper-V checkpoint wrote.
//
// It opens the leaf of a differencing chain, asks libvhdi which byte ranges the
// newer disks wrote, and attributes those ranges to files by reading the
// filesystems at both states. Only the files whose extents fall inside a
// written range are examined, so the cost tracks what changed rather than how
// big the disk is.
//
// It is a module of its own, so run it from this directory rather than from the
// repository root:
//
//	cd examples/changes
//	go run . disk.avhdx                # since the immediate parent
//	go run . -since 2 disk.avhdx       # since two checkpoints back
//	go run . -json disk.avhdx          # as a JSON document
//	go run . -blocks disk.avhdx        # block ranges only, no filesystem opened
//
// This is a separate module. It depends on libvhdi/change, which depends on six
// filesystem libraries, and folding it into the root would drag all of them
// into the core go.mod -- which the core's zero-dependency guarantee forbids.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/aoiflux/libvhdi"
	"github.com/aoiflux/libvhdi/change"
	"github.com/aoiflux/libvhdi/report"
)

func main() {
	since := flag.Int("since", 1, "chain index to compare against; 1 is the immediate parent")
	asJSON := flag.Bool("json", false, "emit a JSON document instead of a summary")
	blocks := flag.Bool("blocks", false, "report changed byte ranges only, without opening any filesystem")
	extents := flag.Bool("extents", false, "include the raw changed byte ranges in the JSON document")
	maxFiles := flag.Int("max", 0, "cap the number of files reported; 0 means no cap")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: changes [-since N] [-json] [-blocks] [-extents] [-max N] <disk>")
		os.Exit(2)
	}

	if err := run(flag.Arg(0), *since, *asJSON, *blocks, *extents, *maxFiles); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(path string, since int, asJSON, blocks, extents bool, maxFiles int) error {
	ctx := context.Background()

	disk, err := libvhdi.OpenFile(path)
	if err != nil {
		return err
	}
	defer disk.Close()

	// A chain that is missing an image cannot be compared against a state it no
	// longer has. Saying so up front is better than reporting every file in the
	// unbacked ranges as changed.
	if !disk.ChainComplete() {
		fmt.Fprintf(os.Stderr, "warning: chain incomplete: %v\n", disk.ParentResolveError())
	}

	doc, err := change.Compare(ctx, disk, since, &change.Options{
		SkipVolumes: blocks,
		Diff:        change.DiffOptions{MaxFiles: maxFiles},
		Report:      report.ChangeOptions{IncludeExtents: extents},
	})
	if err != nil {
		if errors.Is(err, change.ErrVolumeMismatch) {
			return fmt.Errorf("%w\n  the two states are not the same volume; "+
				"check that -since names the right checkpoint", err)
		}
		return err
	}

	if asJSON {
		return report.Write(os.Stdout, doc)
	}
	print(doc, path, since)
	return nil
}

func print(doc *report.ChangeReport, path string, since int) {
	fmt.Printf("%s\n", filepath.Base(path))
	fmt.Printf("  compared against : chain index %d", since)
	if doc.From.Path != "" {
		fmt.Printf(" (%s)", filepath.Base(doc.From.Path))
	}
	fmt.Println()
	fmt.Printf("  tier             : %s\n", doc.Tier)
	fmt.Printf("  written          : %s\n", doc.Totals.WrittenHuman)
	if doc.Totals.Cleared > 0 {
		fmt.Printf("  cleared          : %d bytes (deletions)\n", doc.Totals.Cleared)
	}
	if doc.Totals.Unresolved > 0 {
		// Neither changed nor unchanged: the image that would say is missing.
		fmt.Printf("  unresolved       : %d bytes -- an image in the chain is missing\n", doc.Totals.Unresolved)
	}
	fmt.Printf("  changed ranges   : %d\n", doc.ExtentCount)

	if doc.Tier == report.TierBlock {
		fmt.Println("\n  No filesystem analysis was run, so this says which ranges changed")
		fmt.Println("  and nothing about files. That is not the same as no files changing.")
		printWarnings(doc)
		return
	}

	for _, v := range doc.Volumes {
		fmt.Printf("\n%s volume at offset %d\n", v.Filesystem, v.BaseOffset)
		if v.VolumeIdentity != "" {
			fmt.Printf("  identity: %s\n", v.VolumeIdentity)
		}
		if len(v.Files) == 0 {
			fmt.Println("  no files changed in the written ranges")
		}

		files := append([]report.ChangedFile(nil), v.Files...)
		sort.SliceStable(files, func(i, j int) bool { return files[i].Change < files[j].Change })

		for _, f := range files {
			fmt.Printf("  %-9s %s", f.Change, f.Path)
			if f.PreviousPath != "" {
				fmt.Printf("  (was %s)", f.PreviousPath)
			}
			if f.Stream != "" {
				fmt.Printf("  [stream %s]", f.Stream)
			}
			// The confidence is the part a report must not drop: a rename
			// inferred from a synthesised identity and one read out of a journal
			// look identical without it.
			if f.Confidence != "" {
				fmt.Printf("  (%s)", f.Confidence)
			}
			fmt.Println()
			for _, r := range f.ChangedRanges {
				fmt.Printf("              bytes %d..%d of the file\n", r.Offset, r.Offset+r.Length)
			}
		}

		for _, n := range v.Notes {
			fmt.Printf("  note: %s\n", n)
		}
	}

	printWarnings(doc)
}

func printWarnings(doc *report.ChangeReport) {
	if len(doc.Warnings) == 0 {
		return
	}
	fmt.Println("\nwarnings:")
	for _, w := range doc.Warnings {
		fmt.Printf("  %s: %s\n", w.Kind, w.Message)
	}
}
