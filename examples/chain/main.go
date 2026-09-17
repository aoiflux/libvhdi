// SPDX-License-Identifier: MIT

// chain describes the checkpoint tree formed by a directory of VHD or VHDX
// images, or the differencing chain behind a single image.
//
// Only headers are read when scanning a directory. The block allocation table is
// skipped, and that is the part whose size scales with the virtual disk -- a
// million entries for a 1 TB disk with 1 MB blocks.
//
// Usage:
//
//	go run ./examples/chain <directory>              # the whole tree
//	go run ./examples/chain <disk.vhdx>              # one image's ancestry
//	go run ./examples/chain -json <directory>        # as a JSON document
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoiflux/libvhdi"
	"github.com/aoiflux/libvhdi/report"
)

func main() {
	asJSON := flag.Bool("json", false, "emit a JSON document instead of a summary")
	recursive := flag.Bool("recursive", false, "descend into subdirectories")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: chain [-json] [-recursive] <directory|disk>")
		os.Exit(2)
	}

	target := flag.Arg(0)
	info, err := os.Stat(target)
	if err != nil {
		fail(err)
	}

	if info.IsDir() {
		if err := describeTree(target, *recursive, *asJSON); err != nil {
			fail(err)
		}
		return
	}
	if err := describeImage(target, *asJSON); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// describeTree scans a directory and prints the parent-to-children graph.
func describeTree(dir string, recursive, asJSON bool) error {
	ctx := context.Background()
	opts := &libvhdi.DiscoverOptions{Recursive: recursive}

	if asJSON {
		doc, err := report.Checkpoint(ctx, dir, opts)
		if err != nil {
			return err
		}
		return report.Write(os.Stdout, doc)
	}

	tree, err := libvhdi.DiscoverChain(ctx, dir, opts)
	if err != nil {
		return err
	}

	fmt.Printf("%s\n", tree.Dir)
	fmt.Printf("  images     : %d\n", len(tree.Nodes))
	fmt.Printf("  roots      : %d\n", len(tree.Roots()))
	fmt.Printf("  leaves     : %d\n", len(tree.Leaves()))
	if len(tree.Skipped) > 0 {
		fmt.Printf("  non-images : %d (skipped)\n", len(tree.Skipped))
	}
	fmt.Println()

	for _, root := range tree.Roots() {
		printSubtree(root, "")
	}

	if bad := tree.Unreadable(); len(bad) > 0 {
		fmt.Println("\nunreadable:")
		for _, n := range bad {
			fmt.Printf("  %s\n    %v\n", filepath.Base(n.Path), n.Err)
		}
	}

	if tree.Branched() {
		// A branched tree has no single current disk. Which leaf a machine is
		// using is recorded in its .vmcx configuration, not in the disks, so
		// this reports the branches and does not guess.
		fmt.Printf("\nthis tree has branched into %d devices:\n", len(tree.Lineages()))
		for i, l := range tree.Lineages() {
			state := "complete"
			if !l.Complete() {
				state = "INCOMPLETE - the base disk is not present"
			}
			fmt.Printf("  %d. %s (%s)\n", i+1, filepath.Base(l.Leaf().Path), state)
		}
		fmt.Println("\n  which one is current cannot be determined from the disks.")
	}

	return nil
}

// printSubtree renders one node and everything below it.
func printSubtree(n *libvhdi.Node, indent string) {
	marker := "+-"
	if indent == "" {
		marker = ""
	}

	fmt.Printf("%s%s%s", indent, marker, filepath.Base(n.Path))
	if n.Err != nil {
		fmt.Printf("  [unreadable]\n")
		return
	}

	fmt.Printf("  [%s, %s", n.Info.Format, n.Role())
	if n.Info.HasLog {
		// Discovery does not replay logs, so this only says the image carries
		// one -- it may or may not be replayable.
		fmt.Printf(", has log")
	}
	// The .avhdx extension is Hyper-V's convention, not the format's. A
	// disagreement between the name and the disk type is worth showing.
	if n.Info.HasCheckpointExtension() != (n.Role() == libvhdi.RoleCheckpoint) {
		fmt.Printf(", name and type disagree")
	}
	fmt.Printf("]\n")

	for _, child := range n.Children {
		printSubtree(child, indent+"  ")
	}
}

// describeImage prints the differencing chain behind one image.
func describeImage(path string, asJSON bool) error {
	disk, err := libvhdi.OpenFile(path)
	if err != nil {
		return err
	}
	defer disk.Close()

	if asJSON {
		doc, err := report.Chain(disk, nil)
		if err != nil {
			return err
		}
		return report.Write(os.Stdout, doc)
	}

	chain := disk.Chain()
	fmt.Printf("%s\n", path)
	fmt.Printf("  format     : %s\n", disk.Format())
	fmt.Printf("  type       : %s\n", disk.DiskType())
	fmt.Printf("  size       : %d bytes\n", disk.Size())
	fmt.Printf("  chain depth: %d\n", len(chain))
	fmt.Println()

	for _, e := range chain {
		name := e.Path
		if name == "" {
			name = "(no path)"
		}
		fmt.Printf("  %d. %s\n", e.Index, filepath.Base(name))
		fmt.Printf("       %s, %s, %s\n", e.Format, e.DiskType, e.GUIDString())
	}

	// The chain may be shorter than the image requires. Reads into the missing
	// ranges fail rather than returning zeroes, so this is worth saying.
	if !disk.ChainComplete() {
		fmt.Printf("\n  INCOMPLETE: %v\n", disk.ParentResolveError())
		fmt.Println("  reads into the missing ranges will fail rather than return zeroes.")
	}

	// A report that presents a recovered or unverified reconstruction as an
	// intact read is worse than no report.
	if ws := disk.Warnings(); len(ws) > 0 {
		fmt.Println("\n  warnings:")
		for _, w := range ws {
			fmt.Printf("    %s\n", strings.TrimSpace(w.String()))
		}
	}

	return nil
}
