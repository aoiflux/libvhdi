// SPDX-License-Identifier: MIT

// readat reads bytes from a VHD or VHDX virtual disk at a given offset and hex-dumps them.
//
// Usage:
//
//	go run ./examples/readat <disk.vhd|disk.vhdx> [offset] [length]
//
// Defaults: offset=0, length=512
package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	libvhdi "github.com/aoiflux/libvhdi"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./examples/readat <disk.vhd|disk.vhdx> [offset] [length]")
		os.Exit(1)
	}

	path := os.Args[1]
	offset := int64(0)
	length := 512

	if len(os.Args) >= 3 {
		n, err := strconv.ParseInt(os.Args[2], 0, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid offset:", err)
			os.Exit(1)
		}
		offset = n
	}
	if len(os.Args) >= 4 {
		n, err := strconv.Atoi(os.Args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid length:", err)
			os.Exit(1)
		}
		length = n
	}

	disk, err := libvhdi.OpenFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open error:", err)
		os.Exit(1)
	}
	defer disk.Close()

	buf := make([]byte, length)
	n, err := disk.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "read error:", err)
		os.Exit(1)
	}
	buf = buf[:n]

	fmt.Printf("--- hex dump: offset=%d length=%d ---\n", offset, n)
	hexDump(buf, offset)
	fmt.Println("---")
}

func hexDump(data []byte, baseOffset int64) {
	const cols = 16
	for i := 0; i < len(data); i += cols {
		row := data[i:]
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
		// Pad short final row
		for j := len(row); j < cols; j++ {
			if j == 8 {
				hexPart.WriteByte(' ')
			}
			hexPart.WriteString("   ")
		}

		fmt.Printf("%08x  %-*s |%s|\n",
			baseOffset+int64(i), cols*3+1, hexPart.String(), asciiPart.String())
	}

	// Print SHA-256-style summary line
	if len(data) > 0 {
		fmt.Printf("\n%d bytes read\n", len(data))
		_ = hex.EncodeToString // imported for doc purposes; full digest not computed here
	}
}
