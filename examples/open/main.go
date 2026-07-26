// SPDX-License-Identifier: MIT

// open prints metadata and geometry information for a VHD or VHDX virtual disk image.
//
// Usage:
//
//	go run ./examples/open <disk.vhd|disk.vhdx>
package main

import (
	"fmt"
	"os"

	libvhdi "github.com/aoiflux/libvhdi"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./examples/open <disk.vhd|disk.vhdx>")
		os.Exit(1)
	}

	path := os.Args[1]
	disk, err := libvhdi.OpenFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open error:", err)
		os.Exit(1)
	}
	defer disk.Close()

	formatName := "unknown"
	switch disk.Format() {
	case libvhdi.FormatVHD:
		formatName = "VHD"
	case libvhdi.FormatVHDX:
		formatName = "VHDX"
	}

	diskTypeName := "unknown"
	switch disk.DiskType() {
	case libvhdi.DiskTypeFixed:
		diskTypeName = "fixed"
	case libvhdi.DiskTypeDynamic:
		diskTypeName = "dynamic"
	case libvhdi.DiskTypeDifferential:
		diskTypeName = "differencing"
	}

	fmt.Printf("file             : %s\n", path)
	fmt.Printf("format           : %s\n", formatName)
	fmt.Printf("disk_type        : %s\n", diskTypeName)
	fmt.Printf("virtual_size     : %s\n", formatSize(disk.Size()))
	fmt.Printf("block_size       : %s\n", formatSize(uint64(disk.BlockSize())))
	fmt.Printf("sector_size      : %d bytes\n", disk.SectorSize())
	fmt.Printf("identifier       : %s\n", disk.GUIDString())

	if disk.IsDifferencing() {
		fmt.Printf("parent_filename  : %s\n", disk.ParentFilename())
		fmt.Printf("parent_id        : %s\n", guidToString(disk.ParentIdentifier()))
	}
}

func formatSize(b uint64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
		TB = 1024 * GB
	)
	switch {
	case b >= TB:
		return fmt.Sprintf("%.2f TB (%d bytes)", float64(b)/TB, b)
	case b >= GB:
		return fmt.Sprintf("%.2f GB (%d bytes)", float64(b)/GB, b)
	case b >= MB:
		return fmt.Sprintf("%.2f MB (%d bytes)", float64(b)/MB, b)
	case b >= KB:
		return fmt.Sprintf("%.2f KB (%d bytes)", float64(b)/KB, b)
	default:
		return fmt.Sprintf("%d bytes", b)
	}
}

func guidToString(guid [16]byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%x",
		guid[0:4], guid[4:6], guid[6:8], guid[8:10], guid[10:16])
}
