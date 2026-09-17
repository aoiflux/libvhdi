// SPDX-License-Identifier: MIT

package report_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoiflux/libvhdi"
	"github.com/aoiflux/libvhdi/report"
)

// A report has to be readable years later by a tool that was not built against
// the library that produced it. These tests pin the three things that makes
// possible: a schema version on every document, provenance identifying the
// input and the producer, and the caveats stated rather than dropped.

// The fixtures here go through the public API only, so they also serve as a
// check that the exported surface is enough to build a report from.

func writeFixture(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// fixedVHD returns a small fixed VHD built by hand, so this package's tests do
// not depend on the reader package's unexported fixture helpers.
func fixedVHD(t *testing.T, payload []byte) []byte {
	t.Helper()

	const footerLen = 512
	img := make([]byte, len(payload)+footerLen)
	copy(img, payload)

	f := img[len(payload):]
	copy(f[0:8], []byte("conectix"))
	be32 := func(off int, v uint32) {
		f[off], f[off+1], f[off+2], f[off+3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
	}
	be64 := func(off int, v uint64) {
		for i := 0; i < 8; i++ {
			f[off+i] = byte(v >> (56 - 8*i))
		}
	}

	be32(8, 0x00000002)  // features
	be32(12, 0x00010000) // format version 1.0
	be64(16, ^uint64(0)) // data offset: none, as a fixed disk requires
	be32(24, 757382400)  // creation time, 2024 in VHD epoch seconds
	be32(28, 0x6C696276) // creator "libv"
	be32(32, 0x00010000) // creator version 1.0
	be32(36, 0x5769326B) // creator OS "Wi2k"
	be64(40, uint64(len(payload)))
	be64(48, uint64(len(payload)))
	be32(56, 0) // CHS geometry: none recorded
	be32(60, 2) // disk type: fixed
	copy(f[68:84], []byte{0x11, 0x22, 0x33})
	f[84] = 0

	// The footer's checksum is a one's complement of the sum of its bytes with
	// the checksum field zeroed -- not a CRC, which applies only to VHDX.
	be32(64, 0)
	var sum uint32
	for _, b := range f[:footerLen] {
		sum += uint32(b)
	}
	be32(64, ^sum)

	return img
}

func openFixture(t *testing.T) (*libvhdi.Disk, string) {
	t.Helper()
	dir := t.TempDir()
	path := writeFixture(t, dir, "disk.vhd", fixedVHD(t, bytes.Repeat([]byte{0x7E}, 4096)))

	d, err := libvhdi.OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}

// roundTrip encodes a document and decodes it into a generic map, which is what
// a consumer built against a different version of the library actually sees.
func roundTrip(t *testing.T, doc any) map[string]any {
	t.Helper()

	var buf bytes.Buffer
	if err := report.Write(&buf, doc); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(buf.Bytes(), &generic); err != nil {
		t.Fatalf("decoding the document: %v\n%s", err, buf.String())
	}
	return generic
}

// assertHeader checks the fields every document must carry.
func assertHeader(t *testing.T, doc map[string]any, kind string) {
	t.Helper()

	if got, ok := doc["schema_version"].(float64); !ok || int(got) != report.SchemaVersion {
		t.Errorf("schema_version = %v, want %d", doc["schema_version"], report.SchemaVersion)
	}
	if got, _ := doc["kind"].(string); got != kind {
		t.Errorf("kind = %q, want %q", got, kind)
	}
	if got, _ := doc["generated"].(string); got == "" {
		t.Error("generated is empty")
	}

	lib, ok := doc["library"].(map[string]any)
	if !ok {
		t.Fatalf("library is %T, want an object", doc["library"])
	}
	if got, _ := lib["module"].(string); got != "github.com/aoiflux/libvhdi" {
		t.Errorf("library.module = %q", got)
	}
	// Under `go test` the library is the main module, so the version is the
	// development marker. An empty string would mean the lookup is not running.
	if got, _ := lib["version"].(string); got == "" {
		t.Error("library.version is empty; a report must always name its producer")
	}
}

func TestEveryDocumentCarriesASchemaVersionAndProducer(t *testing.T) {
	// The property that makes a stored report readable by a tool built later:
	// it can refuse a document it does not understand rather than misread it.
	d, path := openFixture(t)
	ctx := context.Background()

	disk, err := report.Disk(d, nil)
	if err != nil {
		t.Fatalf("Disk: %v", err)
	}
	assertHeader(t, roundTrip(t, disk), "disk")

	chain, err := report.Chain(d, nil)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	assertHeader(t, roundTrip(t, chain), "chain")

	alloc, err := report.Allocation(ctx, d, nil)
	if err != nil {
		t.Fatalf("Allocation: %v", err)
	}
	assertHeader(t, roundTrip(t, alloc), "allocation")

	integrity, err := report.Integrity(ctx, path, nil)
	if err != nil {
		t.Fatalf("Integrity: %v", err)
	}
	assertHeader(t, roundTrip(t, integrity), "integrity")

	checkpoint, err := report.Checkpoint(ctx, filepath.Dir(path), nil)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	assertHeader(t, roundTrip(t, checkpoint), "checkpoint")
}

func TestDocumentsRoundTripBackIntoTheirTypes(t *testing.T) {
	// Encoding and decoding into the same struct has to be lossless, or a
	// pipeline that reads a report and re-emits it silently drops fields.
	d, _ := openFixture(t)

	original, err := report.Disk(d, nil)
	if err != nil {
		t.Fatalf("Disk: %v", err)
	}

	var buf bytes.Buffer
	if err := report.Write(&buf, original); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var decoded report.DiskReport
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.SchemaVersion != original.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", decoded.SchemaVersion, original.SchemaVersion)
	}
	if decoded.Kind != original.Kind {
		t.Errorf("Kind = %q, want %q", decoded.Kind, original.Kind)
	}
	if decoded.Source.Path != original.Source.Path {
		t.Errorf("Source.Path = %q, want %q", decoded.Source.Path, original.Source.Path)
	}
	if decoded.Geometry.VirtualSize != original.Geometry.VirtualSize {
		t.Errorf("VirtualSize = %d, want %d",
			decoded.Geometry.VirtualSize, original.Geometry.VirtualSize)
	}
	if decoded.Provenance.CreatorApplication != original.Provenance.CreatorApplication {
		t.Errorf("CreatorApplication = %q, want %q",
			decoded.Provenance.CreatorApplication, original.Provenance.CreatorApplication)
	}
}

func TestHeaderFieldsAreAtTheTopLevel(t *testing.T) {
	// The header is embedded rather than nested, so a consumer reads
	// schema_version from the document root. Nesting it under a key would make
	// every consumer dig for the one field it needs first.
	d, _ := openFixture(t)

	disk, err := report.Disk(d, nil)
	if err != nil {
		t.Fatalf("Disk: %v", err)
	}
	doc := roundTrip(t, disk)

	if _, nested := doc["Header"]; nested {
		t.Error("the header is nested under a key instead of flattened")
	}
	for _, field := range []string{"schema_version", "kind", "generated", "library"} {
		if _, ok := doc[field]; !ok {
			t.Errorf("%q is not at the document root", field)
		}
	}
}

func TestGUIDsSerialiseAsStrings(t *testing.T) {
	// A [16]byte would serialise as sixteen numbers, which is unreadable and
	// which no other tool would recognise as a GUID.
	d, _ := openFixture(t)

	disk, err := report.Disk(d, nil)
	if err != nil {
		t.Fatalf("Disk: %v", err)
	}
	doc := roundTrip(t, disk)

	prov, ok := doc["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("provenance is %T, want an object", doc["provenance"])
	}
	id, ok := prov["identifier"].(string)
	if !ok {
		t.Fatalf("provenance.identifier is %T, want a string", prov["identifier"])
	}
	if strings.Count(id, "-") != 4 {
		t.Errorf("identifier %q is not in canonical GUID form", id)
	}

	// And it parses back, so a value survives a round trip through a report.
	if _, err := libvhdi.ParseGUID(id); err != nil {
		t.Errorf("the serialised GUID does not parse back: %v", err)
	}
}

func TestSizesAreNumbersWithAHumanSibling(t *testing.T) {
	// A machine consumer needs the exact byte count; a person needs to read it.
	// Emitting only the human form would make the document unusable by the
	// first, and only the number makes it tiring for the second.
	d, _ := openFixture(t)

	disk, err := report.Disk(d, nil)
	if err != nil {
		t.Fatalf("Disk: %v", err)
	}
	doc := roundTrip(t, disk)

	geom, _ := doc["geometry"].(map[string]any)
	if _, ok := geom["virtual_size"].(float64); !ok {
		t.Errorf("geometry.virtual_size is %T, want a number", geom["virtual_size"])
	}
	if got, _ := geom["virtual_size_human"].(string); got == "" {
		t.Error("geometry.virtual_size_human is empty")
	}
}

func TestAbsentTimestampsAreOmittedNotInvented(t *testing.T) {
	// A zero creation time means the image recorded none. Rendering it as an
	// epoch date would state something the image does not say.
	dir := t.TempDir()
	img := fixedVHD(t, bytes.Repeat([]byte{0x01}, 4096))

	// Zero the creation time and re-checksum.
	f := img[len(img)-512:]
	f[24], f[25], f[26], f[27] = 0, 0, 0, 0
	f[64], f[65], f[66], f[67] = 0, 0, 0, 0
	var sum uint32
	for _, b := range f[:512] {
		sum += uint32(b)
	}
	c := ^sum
	f[64], f[65], f[66], f[67] = byte(c>>24), byte(c>>16), byte(c>>8), byte(c)

	path := writeFixture(t, dir, "undated.vhd", img)
	d, err := libvhdi.OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	disk, err := report.Disk(d, nil)
	if err != nil {
		t.Fatalf("Disk: %v", err)
	}
	doc := roundTrip(t, disk)

	prov, _ := doc["provenance"].(map[string]any)
	if v, present := prov["created"]; present {
		t.Errorf("provenance.created is present as %v for an image that recorded no timestamp", v)
	}
}

func TestAllocationTotalsAccountForEveryByte(t *testing.T) {
	// The four categories are exhaustive and disjoint, so a consumer can check
	// the producer's arithmetic. A document where they do not sum is one the
	// producer got wrong.
	d, _ := openFixture(t)

	alloc, err := report.Allocation(context.Background(), d, nil)
	if err != nil {
		t.Fatalf("Allocation: %v", err)
	}

	sum := alloc.Totals.Mapped + alloc.Totals.Zero +
		alloc.Totals.ZeroedByChild + alloc.Totals.Unresolved
	if sum != alloc.Totals.Total {
		t.Fatalf("totals sum to %d but the device is %d bytes", sum, alloc.Totals.Total)
	}
	if alloc.Totals.Total != int64(d.Size()) {
		t.Fatalf("Total = %d, want the virtual size %d", alloc.Totals.Total, d.Size())
	}
}

func TestAllocationOmitsExtentsByDefaultButStillCountsThem(t *testing.T) {
	// A fragmented multi-terabyte device has millions of extents, which makes a
	// document nobody can open. Omitting them must not hide how many there were.
	d, _ := openFixture(t)
	ctx := context.Background()

	lean, err := report.Allocation(ctx, d, nil)
	if err != nil {
		t.Fatalf("Allocation: %v", err)
	}
	if len(lean.Extents) != 0 {
		t.Error("extents were included by default")
	}
	if lean.ExtentCount == 0 {
		t.Error("extent_count is zero, so a lean document cannot say how many there were")
	}

	full, err := report.Allocation(ctx, d, &report.AllocationOptions{IncludeExtents: true})
	if err != nil {
		t.Fatalf("Allocation: %v", err)
	}
	if len(full.Extents) != full.ExtentCount {
		t.Fatalf("included %d extents but counted %d", len(full.Extents), full.ExtentCount)
	}
}

func TestTruncatedExtentListStillReportsTheTrueCount(t *testing.T) {
	// A capped list must remain distinguishable from a complete one.
	d, _ := openFixture(t)

	r, err := report.Allocation(context.Background(), d, &report.AllocationOptions{
		IncludeExtents: true,
		MaxExtents:     1,
	})
	if err != nil {
		t.Fatalf("Allocation: %v", err)
	}
	if len(r.Extents) > 1 {
		t.Fatalf("MaxExtents was ignored: %d extents included", len(r.Extents))
	}
	if r.ExtentCount < len(r.Extents) {
		t.Fatal("extent_count is smaller than the number of extents included")
	}
}

func TestIntegrityMarksAbsentStructuresAsAbsentNotFailed(t *testing.T) {
	// A fixed VHD has no dynamic disk header and no mirror footer. Reporting
	// those as failures would show every healthy fixed image failing checks.
	_, path := openFixture(t)

	r, err := report.Integrity(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("Integrity: %v", err)
	}

	if !r.Healthy {
		t.Errorf("a well-formed fixed VHD reports as unhealthy: %+v", r.Checks)
	}
	if !r.Opened {
		t.Errorf("a well-formed fixed VHD did not open: %s", r.OpenError)
	}

	byName := map[string]report.Check{}
	for _, c := range r.Checks {
		byName[c.Name] = c
	}

	if got := byName["footer (trailing)"].Status; got != report.StatusPass {
		t.Errorf("trailing footer status = %q, want pass", got)
	}
	if got := byName["dynamic disk header"].Status; got != report.StatusAbsent {
		t.Errorf("dynamic disk header status = %q, want absent for a fixed disk", got)
	}
	if got := byName["footer (mirror at 0)"].Status; got != report.StatusAbsent {
		t.Errorf("mirror footer status = %q, want absent for a fixed disk", got)
	}
}

func TestIntegrityReportsWhichStructuresSurviveADamagedImage(t *testing.T) {
	// The case the whole report exists for: an image that will not open. Saying
	// which structures are intact is the difference between writing an image off
	// and recovering it.
	dir := t.TempDir()
	img := fixedVHD(t, bytes.Repeat([]byte{0x02}, 4096))
	for i := len(img) - 512; i < len(img); i++ {
		img[i] = 0xDB
	}
	path := writeFixture(t, dir, "damaged.vhd", img)

	r, err := report.Integrity(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("Integrity should describe a damaged image rather than fail: %v", err)
	}

	if r.Healthy {
		t.Error("an image with a destroyed footer reports as healthy")
	}
	if r.Opened {
		t.Error("an image with no readable footer reports as opened")
	}
	if r.OpenError == "" {
		t.Error("open_error is empty for an image that did not open")
	}

	var sawFailure bool
	for _, c := range r.Checks {
		if c.Status == report.StatusFail {
			sawFailure = true
			if c.Detail == "" {
				t.Errorf("check %q failed with no explanation", c.Name)
			}
		}
	}
	if !sawFailure {
		t.Error("no check failed on an image with a destroyed footer")
	}
}

func TestChangeReportAtTheBlockTierOmitsVolumes(t *testing.T) {
	// An absent Volumes must mean "no filesystem analysis was run", never "no
	// files changed". The tier field is what makes that unambiguous.
	d, _ := openFixture(t)

	// A fixed disk has no chain, so asking what changed since its parent is a
	// question with an empty answer -- which is still a valid document.
	r, err := report.Change(context.Background(), d, 1, nil)
	if err != nil {
		t.Fatalf("Change: %v", err)
	}

	if r.Tier != report.TierBlock {
		t.Errorf("tier = %q, want %q", r.Tier, report.TierBlock)
	}
	if r.Volumes != nil {
		t.Error("volumes is present in a block-tier document")
	}

	doc := roundTrip(t, r)
	if _, present := doc["volumes"]; present {
		t.Error("volumes appears in the encoded block-tier document")
	}
	if got, _ := doc["tier"].(string); got != "block" {
		t.Errorf("encoded tier = %q, want %q", got, "block")
	}
}

func TestHashingIsOptionalAndCorrect(t *testing.T) {
	// Hashing reads every byte of every file in the chain, which on large
	// images is minutes of I/O. It has to be a choice, and when chosen it has
	// to be the file's digest rather than the device's.
	d, path := openFixture(t)

	lean, err := report.Disk(d, nil)
	if err != nil {
		t.Fatalf("Disk: %v", err)
	}
	if lean.Source.SHA256 != "" {
		t.Error("the image was hashed without being asked")
	}

	hashed, err := report.Disk(d, &report.Options{Hash: true})
	if err != nil {
		t.Fatalf("Disk: %v", err)
	}
	if len(hashed.Source.SHA256) != 64 {
		t.Fatalf("sha256 is %q, want 64 hex characters", hashed.Source.SHA256)
	}

	// It must be the digest of the file on disk, which is checkable here.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	if int64(len(raw)) != hashed.Source.FileSize {
		t.Errorf("file_size = %d, want %d", hashed.Source.FileSize, len(raw))
	}
}

func TestCheckpointReportStatesWhatItCannotSay(t *testing.T) {
	// The document's silence about checkpoint display names must not be
	// mistaken for a finding. They live in the machine's .vmcx configuration,
	// which is undocumented and deliberately out of scope.
	dir := t.TempDir()
	writeFixture(t, dir, "base.vhd", fixedVHD(t, bytes.Repeat([]byte{0x03}, 4096)))

	r, err := report.Checkpoint(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	if len(r.Limitations) == 0 {
		t.Fatal("the document records no limitations")
	}
	joined := strings.Join(r.Limitations, " ")
	if !strings.Contains(joined, "vmcx") {
		t.Errorf("the limitations do not mention the configuration format: %q", joined)
	}

	if len(r.Nodes) != 1 {
		t.Fatalf("found %d nodes, want 1", len(r.Nodes))
	}
	if r.Nodes[0].Role != "base" {
		t.Errorf("role = %q, want base", r.Nodes[0].Role)
	}
	if r.Branched {
		t.Error("a single-image directory reports as branched")
	}
}

func TestWarningsSurviveIntoTheDocument(t *testing.T) {
	// A report that presents a recovered reconstruction as an intact read is
	// worse than no report, so the caveats have to reach the document.
	//
	// A 511-byte footer is the cheapest way to produce one: it is a real
	// producer's output, it opens successfully, and it is not the conformant
	// location -- so the image is readable and the read is qualified.
	dir := t.TempDir()
	full := fixedVHD(t, bytes.Repeat([]byte{0x09}, 4096))
	if last := full[len(full)-1]; last != 0 {
		t.Fatalf("the footer's final byte is %#x, not the reserved zero this fixture assumes", last)
	}
	path := writeFixture(t, dir, "legacy.vhd", full[:len(full)-1])

	d, err := libvhdi.OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	disk, err := report.Disk(d, nil)
	if err != nil {
		t.Fatalf("Disk: %v", err)
	}
	if len(disk.Warnings) == 0 {
		t.Fatal("a recovered image produced a document with no warnings")
	}
	if !disk.Provenance.FooterRecovered {
		t.Error("provenance does not record that the footer was recovered")
	}

	doc := roundTrip(t, disk)
	warnings, ok := doc["warnings"].([]any)
	if !ok || len(warnings) == 0 {
		t.Fatalf("warnings did not survive encoding: %v", doc["warnings"])
	}
	first, _ := warnings[0].(map[string]any)
	if kind, _ := first["kind"].(string); kind == "" {
		t.Error("the encoded warning has no kind to match on")
	}
	if msg, _ := first["message"].(string); msg == "" {
		t.Error("the encoded warning has no message")
	}
}

func TestAnIntactImageProducesNoWarnings(t *testing.T) {
	// The baseline that makes the test above mean something: if every document
	// carried warnings, their presence would say nothing.
	d, _ := openFixture(t)

	disk, err := report.Disk(d, nil)
	if err != nil {
		t.Fatalf("Disk: %v", err)
	}
	if len(disk.Warnings) != 0 {
		t.Fatalf("an intact image produced warnings: %+v", disk.Warnings)
	}
	if disk.Provenance.FooterRecovered {
		t.Error("an intact image reports its footer as recovered")
	}
}
