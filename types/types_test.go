// SPDX-License-Identifier: MIT

package types_test

import (
	"testing"

	"github.com/aoiflux/libvhdi/internal/binaryutil"

	"github.com/aoiflux/libvhdi/types"
)

func TestConstants(t *testing.T) {
	tests := []struct {
		name string
		got  int
		want int
	}{
		{"VHDFooterSize", types.VHDFooterSize, 512},
		{"VHDDynamicHeaderSize", types.VHDDynamicHeaderSize, 1024},
		{"VHDXHeaderSize", types.VHDXHeaderSize, 4096},
		{"DefaultSectorSize", types.DefaultSectorSize, 512},
		{"VHDXFirstHeaderOffset", types.VHDXFirstHeaderOffset, 64 * 1024},
		{"VHDXSecondHeaderOffset", types.VHDXSecondHeaderOffset, 128 * 1024},
		{"VHDXFirstRegionTableOffset", types.VHDXFirstRegionTableOffset, 192 * 1024},
		{"VHDXSecondRegionTableOffset", types.VHDXSecondRegionTableOffset, 256 * 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %d, want %d", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestMagicSignatures(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"VHDFooterSignature", types.VHDFooterSignature, "conectix"},
		{"VHDDynamicDiskSignature", types.VHDDynamicDiskSignature, "cxsparse"},
		{"VHDXFileSignature", types.VHDXFileSignature, "vhdxfile"},
		{"VHDXHeaderSignature", types.VHDXHeaderSignature, "head"},
		{"VHDXRegionSignature", types.VHDXRegionSignature, "regi"},
		{"VHDXMetadataSignature", types.VHDXMetadataSignature, "metadata"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestDiskTypeValues(t *testing.T) {
	if types.DiskTypeFixed != 0x2 {
		t.Errorf("DiskTypeFixed = %d, want 2", types.DiskTypeFixed)
	}
	if types.DiskTypeDynamic != 0x3 {
		t.Errorf("DiskTypeDynamic = %d, want 3", types.DiskTypeDynamic)
	}
	if types.DiskTypeDifferential != 0x4 {
		t.Errorf("DiskTypeDifferential = %d, want 4", types.DiskTypeDifferential)
	}
}

func TestBATUnallocatedValue(t *testing.T) {
	if types.VHDUnallocatedBlockMarker != 0xFFFFFFFF {
		t.Errorf("VHDUnallocatedBlockMarker = %X, want FFFFFFFF", types.VHDUnallocatedBlockMarker)
	}
}

func TestKnownGUIDs(t *testing.T) {
	// Verify the BAT region GUID is not zero.
	var zero [16]byte
	if types.RegionTypeBAT == zero {
		t.Error("RegionTypeBAT GUID is all zeros")
	}
	if types.RegionTypeMetadata == zero {
		t.Error("RegionTypeMetadata GUID is all zeros")
	}
	// Verify they are distinct.
	if types.RegionTypeBAT == types.RegionTypeMetadata {
		t.Error("RegionTypeBAT and RegionTypeMetadata have the same GUID")
	}
}

func TestMetadataGUIDs(t *testing.T) {
	guids := [][16]byte{
		types.MetadataItemFileParameters,
		types.MetadataItemLogicalSectorSize,
		types.MetadataItemParentLocator,
		types.MetadataItemPhysicalSectorSize,
		types.MetadataItemVirtualDiskIdentifier,
		types.MetadataItemVirtualDiskSize,
	}
	var zero [16]byte
	for i, g := range guids {
		if g == zero {
			t.Errorf("metadata GUID[%d] is all zeros", i)
		}
	}
	// All must be distinct.
	for i := 0; i < len(guids); i++ {
		for j := i + 1; j < len(guids); j++ {
			if guids[i] == guids[j] {
				t.Errorf("metadata GUIDs[%d] and [%d] are identical", i, j)
			}
		}
	}
}

func TestVHDXGUIDConstantByteOrder(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		actual [16]byte
	}{
		{"RegionTypeBAT", "2dc27766-f623-4200-9d64-115e9bfd4a08", types.RegionTypeBAT},
		{"RegionTypeMetadata", "8b7ca206-4790-4b9a-b8fe-575f050f886e", types.RegionTypeMetadata},
		{"MetadataItemFileParameters", "caa16737-fa36-4d43-b3b6-33f0aa44e76b", types.MetadataItemFileParameters},
		{"MetadataItemLogicalSectorSize", "8141bf1d-a96f-4709-ba47-f233a8faab5f", types.MetadataItemLogicalSectorSize},
		{"MetadataItemParentLocator", "a8d35f2d-b30b-454d-abf7-d3d84834ab0c", types.MetadataItemParentLocator},
		{"MetadataItemPhysicalSectorSize", "cda348c7-445d-4471-9cc9-e9885251c556", types.MetadataItemPhysicalSectorSize},
		{"MetadataItemVirtualDiskIdentifier", "beca12ab-b2e6-4523-93ef-c309e000c746", types.MetadataItemVirtualDiskIdentifier},
		{"MetadataItemVirtualDiskSize", "2fa54224-cd1b-4876-b211-5dbed83bf4b8", types.MetadataItemVirtualDiskSize},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, err := binaryutil.GUIDFromString(tt.want)
			if err != nil {
				t.Fatalf("GUIDFromString(%q) failed: %v", tt.want, err)
			}
			if tt.actual != want {
				t.Fatalf("%s mismatch: got=%x want=%x", tt.name, tt.actual, want)
			}
		})
	}
}
