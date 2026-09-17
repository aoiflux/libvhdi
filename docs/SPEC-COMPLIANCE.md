# Specification compliance

Where libvhdi follows the VHD and VHDX specifications, where it deliberately
departs from them, and where it disagrees with another implementation.

Written against the Microsoft *VHD Image Format Specification* (October 2006)
and *[MS-VHDX]: Virtual Hard Disk v2 File Format*. This is an independent
implementation sharing no code with any C library.

Every deviation below is deliberate and tested. Undeliberate ones are bugs;
please report them.

## Deliberate deviations

### 1. A fixed VHD's virtual size excludes the footer

**The disagreement.** qemu-img's `vpc` driver reports a fixed VHD's virtual size
as the *file* length, which includes the trailing 512-byte footer. Converting
such an image to raw with qemu-img therefore yields 512 extra bytes ending in
the `conectix` signature, presented as disk contents.

**What libvhdi does.** Takes the virtual size from the footer's own size field
and clamps reads to it, so the footer never enters the data stream.

**Why.** The specification defines the *Current Size* field as the size of the
disk the guest sees. The footer is metadata appended to the file, not part of
the device. A guest reading its own disk never sees those bytes, so an image
reader that emits them is inventing data — and in a forensic context, inventing
512 bytes of recognisable structure at the end of every fixed image is a
material error.

This library is the conformant one here. Guarded by
`TestFixedVHD_ReadAtDoesNotLeakFooter`. Verified against qemu-img 11.0.0.

### 2. `parent_linkage2` is not parsed

**The specification** defines an optional `parent_linkage2` key in the VHDX
parent locator alongside the required `parent_linkage`.

**What libvhdi does.** Parses `parent_linkage` and verifies the parent against
it. `parent_linkage2` is ignored.

**Why.** Accepting a second identity could only ever *widen* what counts as an
acceptable parent. v0.3.0 exists in part to fix the opposite defect — before it,
a VHDX parent was accepted on virtual size alone, which could silently produce a
wrong reconstruction of a disk. Widening the check that fixes that, on a reading
of the specification this implementation has no producer output to test against,
is not a trade worth making.

The consequence is narrow: a chain whose child records only `parent_linkage2`
would not verify. No producer is known to do that. If one is found, this becomes
a bug rather than a deviation.

### 3. VHDX region and metadata *lengths* are not required to be 1 MB aligned

**The specification** requires region `FileOffset` **and** `Length` to be
multiples of 1 MB.

**What libvhdi does.** Enforces the offset requirement strictly. Accepts any
length that fits the file and does not overlap another region.

**Why.** The offset requirement is load-bearing: the first megabyte holds the
file identifier, both headers and both region tables, so a misaligned offset can
place a region on top of them. A non-conforming *length* cannot corrupt
anything — every structure inside the region is located by its own offset, and
the length is only an upper bound.

A reader that refuses an image it can decode correctly destroys evidence for a
rule violation with no consequence. Strictness where it protects correctness,
leniency where it only protects tidiness.

### 4. VHD sector bitmap sizing is floored at one sector

**The specification** describes the per-block sector bitmap as one bit per
sector, padded to a sector boundary. It does not state a minimum.

**What libvhdi does.** Rounds up rather than truncating, and floors the result
at 512 bytes.

**Why.** A block smaller than 4096 bytes needs fewer than eight bits, and the
unfloored arithmetic yields zero. A zero-sized bitmap places every block's data
one sector early — the parser reads the bitmap itself as payload — and leaves a
differencing disk with no sector-presence information at all, so every sector
appears to come from the parent. No real producer emits such a block, but a
crafted image can.

### 5. A footer is never accepted at offset 0 for a fixed disk

**The specification** says dynamic and differencing disks mirror the footer at
offset 0. It does not say what to do with a fixed disk whose payload happens to
begin with footer-shaped bytes.

**What libvhdi does.** Refuses to recover a *fixed* disk from offset 0, even
when a structurally valid footer with a correct checksum is present there.

**Why.** On a fixed disk offset 0 is user data. A footer that parses there is
either a coincidence or an embedded image; either way it does not describe the
file it sits in, and decoding the file against it would produce a plausible
wrong answer. `TestFixedVHDIsNotRecoveredFromOffsetZero` plants a genuine
parseable fixed-disk footer at offset 0 as ordinary payload and asserts the
image is still refused.

## Recovery paths beyond the specification

These accept images the specification does not describe. All are strictly
additive: they run only after the conformant path has failed, and the result is
always marked as recovered.

### The 511-byte legacy footer

Microsoft Virtual PC wrote a 511-byte footer before the format was documented:
the 512-byte structure with its final reserved byte omitted. Reserved bytes are
defined to be zero, so zero-padding the missing byte reproduces the structure
exactly, checksum included.

Tried after the conformant trailing footer fails. `FooterSource()` reports
`trailing-legacy-511`.

### The mirror footer as a recovery source

The specification describes the mirror at offset 0 as a copy. libvhdi reads it
when the trailing footer cannot be parsed, which makes a dynamic VHD with a
damaged final sector openable.

`FooterSource()` reports `mirror-at-zero` and a `footer_recovered` warning is
raised. An image opened this way is damaged, and a report that presented it as
an intact read would be worse than one that refused to open it.

### Falling through to the secondary region table

The specification describes two region tables without prescribing recovery
behaviour. libvhdi tries the primary, and on *any* structural failure tries the
secondary — the two are copies, so a damaged primary is recoverable.

One failure is not retried: an unknown **required** region entry. That is not
damage but a statement about what the image needs, which the secondary copy
makes too. It is checked before any structural validation precisely so a
malformed unknown-required region cannot be downgraded to "damaged table" and
silently skipped.

## Checks the specification requires and libvhdi enforces

- VHD footer and dynamic header checksums, using the specification's
  one's-complement algorithm. **Not** CRC-32; that is a VHDX construct, and
  `TestVHDSpecChecksumIsNotCRC32C` exists so the distinction is not tested
  tautologically.
- VHD format version must be `0x00010000`.
- VHD `Data Offset` must be `0xFFFFFFFFFFFFFFFF` on a fixed disk.
- VHDX CRC-32C over all 4096 bytes of each header, with the checksum field
  zeroed.
- VHDX header selection by the higher sequence number.
- VHDX region offsets on 1 MB boundaries; regions must not overlap; no region
  type may appear twice.
- **Unknown required region entries and metadata items are refused.** This is
  the forward-compatibility guard the specification exists to provide: an image
  that marks something required cannot be understood without it, and decoding
  the rest while ignoring it yields a device that is wrong in a way nothing
  downstream can detect.
- VHDX metadata items must fit their region, must not overlap the metadata table
  itself, and must not exceed 1 MB.
- VHDX block size between 1 MB and 256 MB and a multiple of 512.
- VHDX logical and physical sector size of exactly 512 or 4096.
- Virtual disk size a whole multiple of the logical sector size.
- The block allocation table must hold one entry per block of the device.

## Things that are not in either specification

**Split or segmented multi-file disks.** Neither specification defines one. In
VHD and VHDX a multi-file disk is always a differencing chain; split images are
a VMDK and VDI concept. A file named like a VMDK extent is rejected with
`ErrSplitImage` and an explanation, rather than reported as an invalid
signature — it is a different format, not a VHD this library is failing to read.

**AVHDX as a format.** `.avhdx` is a VHDX differencing disk with a Hyper-V
naming convention. There is no separate format and no separate parser. The
extension is treated as a corroborating signal, never as the deciding one: a
renamed differencing disk is still a checkpoint, and a base disk given the
extension is not one.

**Hyper-V configuration.** `.vmcx` and `.vmrs` are undocumented proprietary
formats and are out of scope. The practical consequence is that checkpoint
*display names*, and which leaf of a branched tree a machine is currently using,
cannot be recovered from the disks alone. `CheckpointReport` states this in its
`limitations` field so the document's silence is not mistaken for a finding.

**Resilient change tracking.** Hyper-V's `.rct` and `.mrt` sidecars are
undocumented and out of scope. libvhdi derives changed ranges from the
differencing chain itself, which needs no sidecar.

## Validation against real images

`scripts/gen-corpus-qemu.ps1` uses **qemu-img as an independent producer** and
needs no elevation. A known raw pattern is converted into an image and the
pattern becomes ground truth: if libvhdi decodes the image back to the original
bytes, two independently written implementations agree.

Coverage: VHD fixed and dynamic, VHDX fixed and dynamic, VHDX block sizes from
1 MB to 32 MB, CHS-rounded VHD sizes, block-unaligned virtual sizes, all-sparse
disks and a 512 MB disk. All decode byte for byte, sequentially and by random
access across block and sector boundaries.

Differencing chains remain synthetic-only: qemu-img cannot create them for
either format, reporting "Backing file not supported". `scripts/gen-corpus.ps1`
drives Hyper-V's `New-VHD` and is the only route to a producer-generated chain,
but it needs an elevated session. Chain correctness otherwise rests on
spec-derived fixtures.
