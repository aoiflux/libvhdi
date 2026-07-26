// SPDX-License-Identifier: MIT

// offsets detects partition tables and filesystems inside a VHD or VHDX virtual disk image.
// Partition table parsing is provided by github.com/aoiflux/libtable.
//
// Usage:
//
//	go run . [flags] <disk.vhd|disk.vhdx>
//
// Flags:
//
//	-pt-offset N       byte offset where partition-table probing starts (default 0)
//	-verbose-detect    print all detection attempts
//	-dump-sectors N    hex dump first N sectors (512 bytes each) from the decoded stream
//	-map-first-mib N   print a virtual-to-file mapping summary for first N MiB
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	libext "github.com/aoiflux/libext"
	libtable "github.com/aoiflux/libtable"
	libvhdi "github.com/aoiflux/libvhdi"
)

const usage = "usage: go run . [-pt-offset N] [-verbose-detect] [-dump-sectors N] [-map-first-mib N] <disk.vhd|disk.vhdx>"

// detectAttempt describes one partition-table probe strategy.
type detectAttempt struct {
	label string
	opts  libtable.Options
	size  uint64
}

func main() {
	ptOffset := flag.Uint64("pt-offset", 0, "byte offset where partition-table probing starts")
	verbose := flag.Bool("verbose-detect", false, "print all partition/filesystem detection attempts")
	dumpSectors := flag.Int("dump-sectors", 0, "hex dump first N sectors (512 bytes each) from the decoded stream")
	mapFirstMiB := flag.Int("map-first-mib", 0, "print a virtual-to-file mapping summary for first N MiB")
	flag.Usage = func() { fmt.Fprintln(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}

	path := flag.Arg(0)
	disk, err := libvhdi.OpenFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open error:", err)
		os.Exit(1)
	}
	defer disk.Close()

	printDiskReport(disk, path)

	if *dumpSectors > 0 {
		dumpStreamSectors(disk, *dumpSectors)
	}
	if *mapFirstMiB > 0 {
		dumpOffsetMap(disk, *mapFirstMiB)
	}

	tbl, strategy, err := detectPartitionTable(disk, *ptOffset, disk.Size(), *verbose)
	if err != nil {
		fsType, fsOff := probeFilesystems(disk, candidateOffsets(*ptOffset), *verbose)
		if fsType != "" {
			fmt.Printf("Filesystem detected at virtual offset %d: %s (no partition table)\n", fsOff, fsType)
			if fileOff, mapped, mapErr := disk.VirtualToFileOffset(int64(fsOff)); mapErr == nil && mapped {
				fmt.Printf("Filesystem backing file offset: %d\n", fileOff)
			} else if mapErr != nil {
				fmt.Printf("Filesystem backing file offset: unresolved (%v)\n", mapErr)
			} else {
				fmt.Printf("Filesystem backing file offset: unresolved (sparse/unmapped/parent-backed)\n")
			}
			printExtFSInfo(disk, fsOff)
			return
		}
		fmt.Printf("No partition table or filesystem detected: %v\n", err)
		return
	}

	fmt.Printf("Detection strategy  : %s\n\n", strategy)
	printPartitionTable(disk, tbl)
}

// ============================================================================
// Disk info
// ============================================================================

func printDiskReport(disk *libvhdi.Disk, path string) {
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
	}
	fmt.Println()
}

// ============================================================================
// Partition table detection via libtable
// ============================================================================

func detectPartitionTable(disk *libvhdi.Disk, startOff uint64, diskSize uint64, verbose bool) (*libtable.Table, string, error) {
	offsets := candidateOffsets(startOff)
	sizeKnown := diskSize > 0

	if verbose {
		fmt.Printf("[detect] candidate offsets: %v\n", offsets)
		if sizeKnown {
			fmt.Printf("[detect] image size: %d bytes\n", diskSize)
		} else {
			fmt.Println("[detect] image size: unknown")
		}
	}

	var lastErr error
	for _, off := range offsets {
		for _, attempt := range buildAttempts(off, sizeKnown, diskSize) {
			if verbose {
				fmt.Printf("[detect] trying offset=%d mode=%s size=%d\n", off, attempt.label, attempt.size)
			}
			tbl, err := libtable.Parse(disk, attempt.size, attempt.opts)
			if err == nil {
				if verbose {
					fmt.Printf("[detect] success: offset=%d mode=%s\n", off, attempt.label)
				}
				return tbl, fmt.Sprintf("offset=%d, %s", off, attempt.label), nil
			}
			if verbose {
				fmt.Printf("[detect] failed: %v\n", err)
			}
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no partition table found")
	}
	return nil, "", lastErr
}

func buildAttempts(off uint64, sizeKnown bool, imageSize uint64) []detectAttempt {
	strict := libtable.Options{Type: libtable.TypeUnknown, Offset: off}
	relaxed := libtable.Options{Type: libtable.TypeUnknown, Offset: off, GPTDisableCRC: true}
	attempts := []detectAttempt{
		{label: "unknown-size/strict", opts: strict, size: 0},
		{label: "unknown-size/gpt-crc-relax", opts: relaxed, size: 0},
	}
	if sizeKnown {
		attempts = append(attempts,
			detectAttempt{label: "known-size/strict", opts: strict, size: imageSize},
			detectAttempt{label: "known-size/gpt-crc-relax", opts: relaxed, size: imageSize},
		)
	}
	return attempts
}

// ============================================================================
// Partition table printing
// ============================================================================

func printPartitionTable(disk *libvhdi.Disk, tbl *libtable.Table) {
	backup := ""
	if tbl.IsBackup {
		backup = " (backup)"
	}
	fmt.Printf("Table type          : %s%s\n", tbl.Type, backup)
	fmt.Printf("Block size          : %d bytes\n", tbl.BlockSize)
	fmt.Printf("Partition count     : %d\n\n", len(tbl.Partitions))

	for _, p := range tbl.Partitions {
		sizeBytes := p.LengthLBA * uint64(tbl.BlockSize)
		startVirtual := p.StartLBA * uint64(tbl.BlockSize)
		fmt.Printf("  [%d]\n", p.Index)
		fmt.Printf("    start_lba   : %d\n", p.StartLBA)
		fmt.Printf("    end_lba     : %d\n", p.StartLBA+p.LengthLBA-1)
		fmt.Printf("    length_lba  : %d\n", p.LengthLBA)
		fmt.Printf("    start_byte_virtual : %d\n", startVirtual)
		if fileOff, mapped, err := disk.VirtualToFileOffset(int64(startVirtual)); err == nil && mapped {
			fmt.Printf("    start_byte_file    : %d\n", fileOff)
		} else if err != nil {
			fmt.Printf("    start_byte_file    : unresolved (%v)\n", err)
		} else {
			fmt.Printf("    start_byte_file    : unresolved (sparse/unmapped/parent-backed)\n")
		}
		fmt.Printf("    size        : %s (%d bytes)\n", formatSize(sizeBytes), sizeBytes)
		fmt.Printf("    type        : %s\n", p.TypeName)
		fmt.Printf("    flags       : %s\n", partFlagsString(p.Flags))
		if p.GUIDType != "" {
			fmt.Printf("    guid_type   : %s\n", p.GUIDType)
		}
		if p.GUIDUnique != "" {
			fmt.Printf("    guid_unique : %s\n", p.GUIDUnique)
		}
		if p.Name != "" {
			fmt.Printf("    name        : %s\n", p.Name)
		}
		if p.Attributes != 0 {
			fmt.Printf("    attributes  : 0x%016x\n", p.Attributes)
		}
		fmt.Println()
	}
}

func partFlagsString(f libtable.PartFlag) string {
	var parts []string
	if f&libtable.PartFlagAlloc != 0 {
		parts = append(parts, "allocated")
	}
	if f&libtable.PartFlagUnalloc != 0 {
		parts = append(parts, "unallocated")
	}
	if f&libtable.PartFlagMeta != 0 {
		parts = append(parts, "meta")
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, ", ")
}

// ============================================================================
// Filesystem detection (fallback when no partition table found)
// ============================================================================

func probeFilesystems(disk *libvhdi.Disk, offsets []uint64, verbose bool) (string, uint64) {
	for _, off := range offsets {
		if verbose {
			fmt.Printf("[detect] probing filesystem at offset=%d\n", off)
		}
		fsType := detectFilesystem(disk, int64(off))
		if fsType != "" {
			if verbose {
				fmt.Printf("[detect] filesystem match: offset=%d type=%s\n", off, fsType)
			}
			return fsType, off
		}
	}
	if verbose {
		fmt.Println("[detect] no filesystem signatures matched")
	}
	return "", 0
}

func detectFilesystem(disk *libvhdi.Disk, baseOffset int64) string {
	boot, err := readBytes(disk, baseOffset, 4096)
	if err != nil {
		return ""
	}
	switch {
	case hasMarker(boot, 3, "NTFS    "):
		return "NTFS"
	case hasMarker(boot, 82, "FAT32   "):
		return "FAT32"
	case hasMarker(boot, 54, "FAT16   ") || hasMarker(boot, 54, "FAT12   "):
		return "FAT16/12"
	case hasMarker(boot, 3, "EXFAT   "):
		return "exFAT"
	}
	if len(boot) >= 1024+58 {
		ext := boot[1024:]
		if ext[56] == 0x53 && ext[57] == 0xef {
			return "ext2/3/4"
		}
		if ext[0] == 'H' && (ext[1] == '+' || ext[1] == 'X') {
			return "HFS+"
		}
	}
	return ""
}

func hasMarker(data []byte, off int, marker string) bool {
	if off < 0 || off+len(marker) > len(data) {
		return false
	}
	return string(data[off:off+len(marker)]) == marker
}

// ============================================================================
// Sector hex dump
// ============================================================================

func dumpStreamSectors(disk *libvhdi.Disk, sectors int) {
	buf := make([]byte, sectors*512)
	n, err := disk.ReadAt(buf, 0)
	buf = buf[:n]

	fmt.Printf("\n--- hex dump: first %d sector(s) from decoded stream (%d bytes) ---\n", sectors, n)
	const cols = 16
	for i := 0; i < len(buf); i += cols {
		row := buf[i:]
		if len(row) > cols {
			row = row[:cols]
		}
		var hexPart strings.Builder
		var asciiPart strings.Builder
		for j, b := range row {
			if j == 8 {
				hexPart.WriteByte(' ')
			}
			fmt.Fprintf(&hexPart, "%02x ", b)
			if b >= 0x20 && b < 0x7f {
				asciiPart.WriteByte(b)
			} else {
				asciiPart.WriteByte('.')
			}
		}
		for j := len(row); j < cols; j++ {
			if j == 8 {
				hexPart.WriteByte(' ')
			}
			hexPart.WriteString("   ")
		}
		fmt.Printf("%08x  %-*s |%s|\n", i, cols*3+1, hexPart.String(), asciiPart.String())
	}
	if err != nil && err != io.EOF {
		fmt.Printf("read error after %d bytes: %v\n", n, err)
	}
	fmt.Println("---")
}

// ============================================================================
// Virtual-to-file mapping summary
// ============================================================================

func dumpOffsetMap(disk *libvhdi.Disk, firstMiB int) {
	if firstMiB <= 0 {
		return
	}

	const oneMiB = int64(1024 * 1024)
	sectorSize := int64(disk.SectorSize())
	if sectorSize <= 0 {
		sectorSize = 512
	}
	maxVirtual := int64(disk.Size())
	if maxVirtual <= 0 {
		return
	}

	fmt.Printf("\n--- virtual-to-file mapping summary (first %d MiB) ---\n", firstMiB)
	fmt.Printf("%4s  %-14s  %-14s  %-12s  %-8s\n", "MiB", "virtual_start", "file_start", "mapped_bytes", "linear")

	for i := 0; i < firstMiB; i++ {
		chunkStart := int64(i) * oneMiB
		if chunkStart >= maxVirtual {
			break
		}
		chunkEnd := chunkStart + oneMiB
		if chunkEnd > maxVirtual {
			chunkEnd = maxVirtual
		}

		mappedBytes := int64(0)
		firstFileOffset := int64(-1)
		linear := true
		havePrev := false
		prevVirtual := int64(0)
		prevFile := int64(0)

		for v := chunkStart; v < chunkEnd; v += sectorSize {
			fileOff, mapped, err := disk.VirtualToFileOffset(v)
			if err != nil {
				continue
			}
			if !mapped {
				linear = false
				continue
			}

			step := sectorSize
			if v+step > chunkEnd {
				step = chunkEnd - v
			}
			mappedBytes += step

			if firstFileOffset < 0 {
				firstFileOffset = fileOff
			}
			if havePrev {
				if fileOff-prevFile != v-prevVirtual {
					linear = false
				}
			}
			havePrev = true
			prevVirtual = v
			prevFile = fileOff
		}

		mappedDisplay := fmt.Sprintf("%d", mappedBytes)
		if mappedBytes == 0 {
			mappedDisplay = "0"
		}
		fileStartDisplay := "unmapped"
		if firstFileOffset >= 0 {
			fileStartDisplay = fmt.Sprintf("%d", firstFileOffset)
		}
		linearDisplay := "no"
		if linear && mappedBytes > 0 {
			linearDisplay = "yes"
		}
		fmt.Printf("%4d  %-14d  %-14s  %-12s  %-8s\n", i, chunkStart, fileStartDisplay, mappedDisplay, linearDisplay)
	}
	fmt.Println("---")
}

// ============================================================================
// Filesystem info via libext
// ============================================================================

// offsetReaderAt wraps an io.ReaderAt and shifts every read by baseOffset,
// so that libext (which expects reads starting at 0) sees the filesystem
// beginning at the given virtual disk byte offset.
type offsetReaderAt struct {
	r          io.ReaderAt
	baseOffset int64
}

func (o offsetReaderAt) ReadAt(p []byte, off int64) (int, error) {
	return o.r.ReadAt(p, o.baseOffset+off)
}

// printExtFSInfo opens the filesystem at baseVirtual inside disk using libext
// and prints a summary of the superblock. Only ext2/3/4 images are supported;
// other filesystem types are silently skipped.
func printExtFSInfo(disk *libvhdi.Disk, baseVirtual uint64) {
	fs, err := libext.Open(offsetReaderAt{r: disk, baseOffset: int64(baseVirtual)})
	if err != nil {
		fmt.Printf("\nlibext: could not parse filesystem: %v\n", err)
		return
	}
	defer fs.Close()

	sb := fs.Superblock()

	fmt.Println()
	fmt.Println("--- filesystem info (libext) ---")
	fmt.Printf("  kind             : %s\n", fs.Kind())

	volName := strings.TrimRight(sb.VolumeName, "\x00")
	if volName != "" {
		fmt.Printf("  volume_name      : %s\n", volName)
	}

	uuidBytes := sb.UUID[:]
	uuidStr := fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(uuidBytes[0:4]),
		hex.EncodeToString(uuidBytes[4:6]),
		hex.EncodeToString(uuidBytes[6:8]),
		hex.EncodeToString(uuidBytes[8:10]),
		hex.EncodeToString(uuidBytes[10:16]))
	fmt.Printf("  uuid             : %s\n", uuidStr)

	fmt.Printf("  block_size       : %d bytes\n", sb.BlockSize)
	fmt.Printf("  inode_size       : %d bytes\n", sb.InodeSize)
	fmt.Printf("  total_blocks     : %d\n", sb.BlocksCount)
	fmt.Printf("  free_blocks      : %d\n", sb.FreeBlocks)
	fmt.Printf("  total_inodes     : %d\n", sb.InodesCount)
	fmt.Printf("  free_inodes      : %d\n", sb.FreeInodes)
	fmt.Printf("  groups           : %d\n", sb.GroupsCount)

	if !sb.MountTime.IsZero() {
		fmt.Printf("  last_mount_time  : %s\n", sb.MountTime.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	if !sb.WriteTime.IsZero() {
		fmt.Printf("  last_write_time  : %s\n", sb.WriteTime.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	lastMounted := strings.TrimRight(sb.LastMounted, "\x00")
	if lastMounted != "" {
		fmt.Printf("  last_mounted_at  : %s\n", lastMounted)
	}
	fmt.Printf("  mount_count      : %d / %d\n", sb.MountCount, sb.MaxMountCount)

	if features := fs.DescribeFeatures(); features != "" {
		fmt.Printf("  features         : %s\n", features)
	}
	fmt.Println("---")
}

// ============================================================================
// Helpers
// ============================================================================

func candidateOffsets(requested uint64) []uint64 {
	seen := make(map[uint64]struct{})
	all := []uint64{requested, 0, 63 * 512, 2048 * 512}
	out := make([]uint64, 0, len(all))
	for _, off := range all {
		if _, dup := seen[off]; dup {
			continue
		}
		seen[off] = struct{}{}
		out = append(out, off)
	}
	return out
}

func readBytes(r *libvhdi.Disk, offset int64, length int) ([]byte, error) {
	buf := make([]byte, length)
	n, err := r.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
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
		return fmt.Sprintf("%.2f TB", float64(b)/TB)
	case b >= GB:
		return fmt.Sprintf("%.2f GB", float64(b)/GB)
	case b >= MB:
		return fmt.Sprintf("%.2f MB", float64(b)/MB)
	case b >= KB:
		return fmt.Sprintf("%.2f KB", float64(b)/KB)
	default:
		return fmt.Sprintf("%d bytes", b)
	}
}
