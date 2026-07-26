<#
.SYNOPSIS
    Generates a corpus of real VHD/VHDX images using qemu-img, with raw
    ground-truth files. Requires no elevation.

.DESCRIPTION
    The Go test suite builds its own images from the published specifications.
    That is what surfaced the VHD checksum defect fixed in v0.2.0, but it cannot
    prove agreement with images other implementations actually emit — a parser and
    a fixture written by the same author can be wrong together.

    This script uses qemu-img as an independent producer. Each image is made by
    converting a known raw pattern into VHD or VHDX, so the pattern *is* the
    ground truth: if this library decodes the image back to the original bytes,
    the two implementations agree.

    Coverage: fixed and dynamic subformats in both formats, several VHDX block
    sizes, CHS-rounded VHD sizes, block-unaligned sizes, and all-sparse disks.

    NOT covered: differencing chains. qemu-img reports "Backing file not
    supported" for both vpc and vhdx, so no differencing image can be produced
    this way. Use gen-corpus.ps1, which drives Hyper-V's New-VHD and does support
    -ParentPath, for that case. It needs an elevated session.

.PARAMETER OutputDir
    Where to write the corpus. Created if absent.

.PARAMETER QemuImg
    Path to qemu-img. Defaults to whatever is on PATH.

.EXAMPLE
    pwsh -File scripts/gen-corpus-qemu.ps1 -OutputDir C:\corpus
    $env:LIBVHDI_CORPUS = "C:\corpus"
    go test ./reader/ -run TestCorpus -v
#>

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$OutputDir,

    [string]$QemuImg
)

$ErrorActionPreference = 'Stop'

if (-not $QemuImg) {
    $cmd = Get-Command qemu-img -ErrorAction SilentlyContinue
    if (-not $cmd) {
        throw "qemu-img not found on PATH. Install it (scoop install qemu, winget install qemu) or pass -QemuImg."
    }
    $QemuImg = $cmd.Source
}
Write-Host "qemu-img: $QemuImg"
& $QemuImg --version | Select-Object -First 1 | Write-Host

New-Item -ItemType Directory -Force $OutputDir | Out-Null
$OutputDir = (Resolve-Path $OutputDir).Path
Write-Host "corpus directory: $OutputDir`n"

# ------------------------------------------------------------------- utilities

# New-Pattern writes a sparse, position-dependent raw file.
#
# The pattern must not be a constant fill: a constant would still compare equal
# after an offset error, which is the main class of bug this corpus exists to
# catch. Leaving most of the disk zero keeps dynamic images genuinely sparse, so
# both allocated and unallocated blocks get exercised.
function New-Pattern {
    param(
        [Parameter(Mandatory)] [string]$Path,
        [Parameter(Mandatory)] [int64]$Size,
        [int64[]]$DataOffsets = @(),
        [int]$Seed = 0
    )

    $fs = [System.IO.File]::Create($Path)
    try {
        $fs.SetLength($Size)
        $chunk = New-Object byte[] 4096
        foreach ($offset in $DataOffsets) {
            if ($offset -lt 0 -or $offset + $chunk.Length -gt $Size) { continue }
            for ($i = 0; $i -lt $chunk.Length; $i++) {
                # Mix the offset in so identical chunks never appear twice.
                $chunk[$i] = [byte]((($offset / 4096) * 31 + $i * 7 + $Seed) % 251)
            }
            $fs.Seek($offset, 'Begin') | Out-Null
            $fs.Write($chunk, 0, $chunk.Length)
        }
        $fs.Flush()
    }
    finally {
        $fs.Dispose()
    }
}

$results = @()

# New-Image converts a raw pattern into one image, leaving the pattern beside it
# as <name>.raw for the corpus tests to compare against.
function New-Image {
    param(
        [Parameter(Mandatory)] [string]$Name,      # e.g. dynamic-vhdx.vhdx
        [Parameter(Mandatory)] [string]$Format,    # vpc | vhdx
        [Parameter(Mandatory)] [int64]$Size,
        [string]$Options = '',
        [int64[]]$DataOffsets = @(),
        [int]$Seed = 0
    )

    $image = Join-Path $OutputDir $Name
    $raw = [IO.Path]::ChangeExtension($image, '.raw')

    New-Pattern -Path $raw -Size $Size -DataOffsets $DataOffsets -Seed $Seed

    $qemuArgs = @('convert', '-f', 'raw', '-O', $Format)
    if ($Options) { $qemuArgs += @('-o', $Options) }
    $qemuArgs += @('--', $raw, $image)

    & $QemuImg @qemuArgs 2>&1 | Out-String | Write-Host -NoNewline
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path $image)) {
        Write-Warning "  $Name : qemu-img failed (exit $LASTEXITCODE)"
        Remove-Item $raw -ErrorAction SilentlyContinue
        return
    }

    # Ask qemu what virtual size it recorded. A mismatch against the pattern is
    # expected for CHS-rounded VHDs and is handled by the corpus tests, which
    # require the extra tail to decode as zeroes.
    $info = & $QemuImg info --output=json -- $image | ConvertFrom-Json

    $script:results += [pscustomobject]@{
        Name        = $Name
        Format      = $Format
        Options     = $Options
        PatternSize = $Size
        VirtualSize = $info.'virtual-size'
        FileSize    = (Get-Item $image).Length
    }
    Write-Host ("  {0,-26} pattern {1,11:N0}  virtual {2,11:N0}  file {3,11:N0}" -f `
        $Name, $Size, $info.'virtual-size', (Get-Item $image).Length)
}

# --------------------------------------------------------------------- fixtures

$MB = 1MB

# Offsets chosen to straddle block boundaries at every block size used below, so
# a block-indexing error shows up regardless of geometry.
#
# Every arithmetic element is parenthesised deliberately. PowerShell binds the
# comma operator tighter than '*', so @(0, 4096, 1 * $MB) parses as
# @((0, 4096, 1) * $MB) -- array repetition, not a three-element list.
$scattered = @(0, 4096, (1 * $MB), (2 * $MB - 4096), (3 * $MB), (7 * $MB), (15 * $MB))

Write-Host "-- fixed and dynamic, both formats --"
New-Image -Name 'fixed-vhd.vhd'      -Format vpc  -Size (16 * $MB) -Options 'subformat=fixed,force_size=on'   -DataOffsets $scattered -Seed 1
New-Image -Name 'dynamic-vhd.vhd'    -Format vpc  -Size (16 * $MB) -Options 'subformat=dynamic,force_size=on' -DataOffsets $scattered -Seed 2
New-Image -Name 'fixed-vhdx.vhdx'    -Format vhdx -Size (16 * $MB) -Options 'subformat=fixed'                 -DataOffsets $scattered -Seed 3
New-Image -Name 'dynamic-vhdx.vhdx'  -Format vhdx -Size (16 * $MB) -Options 'subformat=dynamic'               -DataOffsets $scattered -Seed 4

Write-Host "`n-- CHS-rounded VHD (force_size off, as real tools write it) --"
# Without force_size the virtual size is rounded up to a CHS-representable value,
# so the disk is larger than the pattern and the tail must decode as zeroes.
New-Image -Name 'chs-dynamic-vhd.vhd' -Format vpc -Size (10 * $MB) -Options 'subformat=dynamic' -DataOffsets @(0, (1 * $MB), (9 * $MB)) -Seed 5
New-Image -Name 'chs-fixed-vhd.vhd'   -Format vpc -Size (10 * $MB) -Options 'subformat=fixed'   -DataOffsets @(0, (1 * $MB), (9 * $MB)) -Seed 6

Write-Host "`n-- VHDX block sizes --"
# The block size changes the BAT chunk ratio and the sector bitmap layout.
foreach ($bs in '1M', '2M', '32M') {
    New-Image -Name "vhdx-block-$bs.vhdx" -Format vhdx -Size (64 * $MB) `
        -Options "subformat=dynamic,block_size=$bs" `
        -DataOffsets @(0, (1 * $MB), (5 * $MB), (33 * $MB), (63 * $MB)) -Seed 7
}

Write-Host "`n-- block-unaligned virtual size --"
# A virtual size that is not a whole number of blocks, so the final block extends
# past the end of the device.
New-Image -Name 'unaligned-vhdx.vhdx' -Format vhdx -Size (5 * $MB + 512) -Options 'subformat=dynamic,block_size=2M' -DataOffsets @(0, (4 * $MB)) -Seed 8
New-Image -Name 'unaligned-vhd.vhd'   -Format vpc  -Size (5 * $MB + 512) -Options 'subformat=dynamic,force_size=on' -DataOffsets @(0, (4 * $MB)) -Seed 9

Write-Host "`n-- all-sparse disks (every block unallocated) --"
New-Image -Name 'empty-vhdx.vhdx' -Format vhdx -Size (32 * $MB) -Options 'subformat=dynamic'               -DataOffsets @()
New-Image -Name 'empty-vhd.vhd'   -Format vpc  -Size (32 * $MB) -Options 'subformat=dynamic,force_size=on' -DataOffsets @()

Write-Host "`n-- larger disk, many blocks --"
New-Image -Name 'large-vhdx.vhdx' -Format vhdx -Size (512 * $MB) -Options 'subformat=dynamic,block_size=1M' `
    -DataOffsets @(0, (1 * $MB), (100 * $MB), (255 * $MB), (511 * $MB)) -Seed 10

# ----------------------------------------------------------------------- report

Write-Host "`n================ summary ================"
$results | Format-Table -AutoSize

$images = @(Get-ChildItem $OutputDir -File | Where-Object { $_.Extension -in '.vhd', '.vhdx' })
$dumps = @(Get-ChildItem $OutputDir -File | Where-Object { $_.Extension -eq '.raw' })
Write-Host ("{0} image(s), {1} ground-truth file(s)" -f $images.Count, $dumps.Count)

Write-Host @"

NOT COVERED BY THIS SCRIPT
  Differencing chains. qemu-img cannot create them for vpc or vhdx
  ("Backing file not supported"). Use scripts/gen-corpus.ps1, which drives
  Hyper-V New-VHD -ParentPath, from an elevated session.

RUN THE TESTS
  `$env:LIBVHDI_CORPUS = "$OutputDir"
  go test ./reader/ -run TestCorpus -v
"@
