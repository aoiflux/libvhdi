# Support matrix

What libvhdi reads, what it does not, and what is not a VHD/VHDX concept at all.
Current as of v0.3.0.

Three symbols are used throughout: **✔** supported, **~** partial (with the
limit named), **✘** not supported, **–** not applicable to the format.

## VHD

| Capability | Status | Notes |
| --- | :--: | --- |
| Fixed disk (type 2) | ✔ | Payload at offset 0, footer clamped out of the data stream |
| Dynamic disk (type 3) | ✔ | |
| Differencing disk (type 4) | ✔ | Sector-granular resolution against the parent chain |
| Disk types 0, 1, 5, 6 | ✘ | Reserved or deprecated by the specification. Rejected with `ErrUnsupportedFeature`, not `ErrCorruptImage` — the image is well-formed, the library does not implement it |
| Footer checksum | ✔ | One's complement of the footer bytes, per the specification. **Not** a CRC; CRC-32C is a VHDX construct |
| Dynamic header checksum | ✔ | Same algorithm |
| Mirror footer at offset 0 | ✔ | Read as a recovery path when the trailing footer fails. `FooterSource()` reports which copy was used |
| 511-byte legacy footer | ✔ | Written by Microsoft Virtual PC before the format was documented. Recovered by zero-padding the absent reserved byte |
| Parent locators `W2ru` / `W2ku` | ✔ | Both platform codes decode. `W2ku` was undecodable before v0.3.0 |
| Parent GUID verification | ✔ | |
| Parent timestamp comparison | ✔ | Reported as a warning, never an error — filesystem timestamps do not survive a copy |
| CHS geometry | ✔ | Exposed through `Geometry()`. `HasCHS()` distinguishes a recorded zero geometry from a format that has no such field |
| Creator application, version, host OS | ✔ | Through `Provenance()` |
| Saved-state flag | ✔ | Marks an image whose filesystem is crash-consistent rather than cleanly unmounted |
| Original vs current size | ✔ | Their difference is the only record that a disk was expanded |
| Timestamps | ✔ | Decoded against the VHD epoch (1 January 2000), not the Unix epoch |
| Explicit zero on a differencing disk | ✘ | **A format limitation, not a library one.** See below |
| 4096-byte logical sectors | – | VHD is defined on 512-byte sectors throughout |
| Writing | ✘ | By design. No code path writes to an image |

### VHD cannot express an explicit zero

A VHD differencing disk has no block state meaning "zero". Its per-block sector
bitmap distinguishes only *this sector is mine* from *read this sector from the
parent*. A region cleared on a VHD chain is therefore recorded as belonging to
the parent, and reads back as the parent's old contents.

The clearing did not happen at the format level. This is not something libvhdi
fails to detect — there is nothing to detect. **Deletion-aware change tracking
requires VHDX.** `TestVHDCannotExpressAnExplicitZero` pins the behaviour so it
is not later mistaken for a defect.

## VHDX

| Capability | Status | Notes |
| --- | :--: | --- |
| Fixed, dynamic, differencing | ✔ | |
| Both header slots, sequence selection | ✔ | Higher sequence number wins, per the specification |
| CRC-32C over headers and region tables | ✔ | Computed over all 4096 bytes with the checksum field zeroed |
| Both region tables | ✔ | Primary first, secondary as a fallback |
| Unknown **required** region entry | ✔ | Refused, as the specification requires. Checked before structural validation, so a malformed one cannot be downgraded to "damaged table" and skipped |
| 1 MB region alignment | ✔ | The first megabyte holds the file identifier, both headers and both region tables |
| Region overlap and duplicate types | ✔ | Unknown region types participate: the library does not know what such a region holds, but it knows nothing else may occupy that space |
| Metadata items: file parameters, virtual disk size, logical and physical sector size, virtual disk identifier, parent locator | ✔ | |
| Unknown **required** metadata item | ✔ | Refused with `ErrUnsupportedFeature` |
| Metadata item bounds | ✔ | Items must fit the region, must not overlap the metadata table, and must not exceed the format's 1 MB maximum |
| `parent_linkage` | ✔ | The parent's **data-write** GUID, not its virtual disk identifier. Conflating the two is what makes a chain fail to link up |
| `parent_linkage2` | ✘ | Deliberately unparsed — see `docs/SPEC-COMPLIANCE.md` |
| Creator string | ✔ | Decoded as UTF-16LE. Read as a byte string it stops at the first character |
| BAT states 0, 1, 2, 3, 6, 7 | ✔ | |
| Chunk ratio and sector bitmaps | ✔ | |
| 512-byte and 4096-byte logical sectors | ✔ | |
| Log replay | ✔ | Circular buffer, torn-write detection, zero descriptors, strict sequence. Applied into a **read-only in-memory overlay**, so the file stays byte-identical |
| Unreplayable log | ~ | Refused with `ErrDirtyImage` unless `Options.AllowDirtyImage` is set. `IsDirty()` reports the state |
| Explicit zero on a differencing disk | ✔ | `PAYLOAD_BLOCK_ZERO` and `PAYLOAD_BLOCK_UNMAPPED` surface as `ExtentZeroedByChild` |
| Writing | ✘ | By design |

## Chains and checkpoints

| Capability | Status | Notes |
| --- | :--: | --- |
| Automatic parent resolution | ✔ | Searches the image's own directory by default. `Open` and `OpenFile` behave identically from v0.3.0 |
| Parent identity verification | ✔ | A parent is never accepted on virtual size alone |
| Mixed-format chains | ~ | A parent's format is always detected from its own signature and never inherited from its child, so a VHDX child over a VHD parent is structurally supported. No cross-format fixture exists yet, so this is not covered end to end |
| Cycle detection | ✔ | Keyed on the resolved path, falling back to the disk GUID. A path key is used because two VHDX images that omit the virtual disk identifier both report the zero GUID |
| Chain depth bound | ✔ | `Options.MaxChainDepth`, default 32 |
| Fail-closed on a missing parent | ✔ | Reads into unbacked ranges return `ErrParentRequired`. Zeroes would be indistinguishable from genuine contents |
| Checkpoint tree discovery | ✔ | `DiscoverChain` builds the full parent→children tree from a directory, reading headers only — never the block allocation table, which is the part that scales with disk size |
| Branched trees | ✔ | Reported as several leaves. Which one is current is **not** determinable from the disks |
| Checkpoint display names | ✘ | Recorded in the machine's `.vmcx`, an undocumented proprietary format. Out of scope |
| `.rct` / `.mrt` resilient change tracking | ✘ | Undocumented Hyper-V sidecars. Out of scope |
| Split / segmented multi-file disks | – | **Not a VHD or VHDX concept.** Neither specification defines one; a multi-file disk in these formats is always a differencing chain. Files named like VMDK extents are rejected with `ErrSplitImage` |

## Reading and analysis

| Capability | Status | Notes |
| --- | :--: | --- |
| `io.ReaderAt` over the decoded device | ✔ | `*Disk` is itself an `io.ReaderAt` |
| `io.SectionReader` | ✔ | `SectionReader()`, for handing the device to a hasher or a filesystem parser |
| Sparse streaming | ✔ | `Stream` reads only what is backed. A 4 TB device holding 8 GB costs 8 GB of reads |
| Extent map with provenance | ✔ | Each mapped extent names its backing file and chain position |
| Block-level change tracking | ✔ | `ChangedExtents`, `ChangedSince`, `ChangedBytes` |
| File-level change tracking | ✘ | Ships in the separate `libvhdi/change` module. Not importing it costs nothing |
| Concurrent reads on one handle | ✔ | Covered by the race detector in CI |
| Cancellation | ✔ | On every whole-disk operation. `ReadAt` keeps its `io.ReaderAt` signature |
| JSON reports | ✔ | Six document types, each schema-versioned |

## Filesystems

libvhdi parses **no filesystems**. It presents a decoded block device, and what
lives on that device is another library's concern.

This is the library's central design constraint rather than an omission. The
core module has no dependencies and must never acquire one, so a consumer who
wants only VHD/VHDX parsing can take libvhdi without absorbing a filesystem
library into their module graph. The `vhdimap` package declares what a
filesystem parser must provide; the `libvhdi/change` module implements the
bridge and is opt-in by importing.

ReFS is explicitly unsupported and no Go implementation is known to exist.
Windows Server hosts commonly format VM storage as ReFS, so an examiner may
meet a disk this library can decode and no available tool can interpret.
