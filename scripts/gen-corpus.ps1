<#
.SYNOPSIS
    Generates a corpus of real VHD/VHDX images with raw ground-truth dumps.

.DESCRIPTION
    The Go test suite builds its own images from the published specifications,
    which is what surfaced the VHD checksum defect fixed in v0.2.0. Those
    fixtures cannot prove agreement with the images real producers emit, so this
    script creates authentic ones and a raw dump of each for byte-for-byte
    comparison.

    Images come from Hyper-V's New-VHD, i.e. the reference producer. Raw dumps
    come from qemu-img, an independent implementation, so a dump agreeing with
    this library is genuine cross-validation rather than self-confirmation.

    Point the test suite at the output:

        $env:LIBVHDI_CORPUS = "<OutputDir>"
        go test ./reader/ -run TestCorpus -v

.PARAMETER OutputDir
    Where to write the corpus. Created if absent.

.PARAMETER SizeMB
    Virtual size of each generated disk, in MB. Default 64.

.PARAMETER KeepMounted
    Skip dismounting on failure, for debugging.

.NOTES
    Requirements:
      - Hyper-V PowerShell module, and an elevated session. New-VHD talks to the
        Hyper-V VMMS service and fails with an authorization error otherwise.
      - qemu-img on PATH, for the raw dumps.

    Both are checked up front. Without qemu-img the images are still written but
    have no .raw counterparts, and the corpus tests will skip them.
#>

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$OutputDir,

    [int]$SizeMB = 64,

    [switch]$KeepMounted
)

$ErrorActionPreference = 'Stop'

function Test-Elevated {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    (New-Object Security.Principal.WindowsPrincipal $id).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)
}

# ---------------------------------------------------------------- prerequisites

if (-not (Get-Module -ListAvailable -Name Hyper-V)) {
    throw "The Hyper-V PowerShell module is not installed. Enable the 'Hyper-V Module for Windows PowerShell' feature."
}
if (-not (Test-Elevated)) {
    throw "New-VHD requires an elevated session. Re-run this script as Administrator."
}

$qemu = Get-Command qemu-img -ErrorAction SilentlyContinue
if (-not $qemu) {
    Write-Warning "qemu-img not found on PATH. Images will be generated without raw dumps, and the corpus tests will skip them."
    Write-Warning "Install it (e.g. 'winget install SoftwareFreedomConservancy.QEMU' or 'scoop install qemu') and re-run to produce ground truth."
}

New-Item -ItemType Directory -Force $OutputDir | Out-Null
$OutputDir = (Resolve-Path $OutputDir).Path
Write-Host "corpus directory: $OutputDir"

$sizeBytes = $SizeMB * 1MB

# ------------------------------------------------------------------- utilities

# Write-Pattern fills a mounted disk with a position-dependent, non-repeating
# pattern. Constant fills would mask offset errors, which is exactly the class of
# bug this corpus exists to catch.
function Write-Pattern {
    param(
        [Parameter(Mandatory)] [string]$VhdPath,
        [Parameter(Mandatory)] [int]$Seed
    )

    $disk = Mount-VHD -Path $VhdPath -PassThru
    try {
        $number = $disk.Number
        $raw = "\\.\PhysicalDrive$number"

        $stream = New-Object System.IO.FileStream($raw, 'Open', 'ReadWrite')
        try {
            # Write a few scattered sectors so the result is sparse: dynamic disks
            # then have both allocated and unallocated blocks, and differencing
            # children have partially-written blocks.
            $sector = New-Object byte[] 4096
            foreach ($blockIndex in 0, 1, 3, 7) {
                for ($i = 0; $i -lt $sector.Length; $i++) {
                    $sector[$i] = [byte](($blockIndex * 31 + $i * 7 + $Seed) % 251)
                }
                $offset = [int64]$blockIndex * 1MB
                if ($offset + $sector.Length -le $stream.Length) {
                    $stream.Seek($offset, 'Begin') | Out-Null
                    $stream.Write($sector, 0, $sector.Length)
                }
            }
            $stream.Flush()
        }
        finally {
            $stream.Dispose()
        }
    }
    finally {
        if (-not $KeepMounted) {
            Dismount-VHD -Path $VhdPath
        }
    }
}

function Convert-ToRaw {
    param([Parameter(Mandatory)] [string]$Path)

    if (-not $qemu) { return }

    $raw = [IO.Path]::ChangeExtension($Path, '.raw')
    Write-Host "  -> raw dump via qemu-img"
    & $qemu.Source convert -O raw -- $Path $raw
    if ($LASTEXITCODE -ne 0) {
        Write-Warning "  qemu-img failed for $Path (exit $LASTEXITCODE); no ground truth written"
        Remove-Item $raw -ErrorAction SilentlyContinue
    }
}

function New-Image {
    param(
        [Parameter(Mandatory)] [string]$Name,
        [Parameter(Mandatory)] [scriptblock]$Create,
        [int]$Seed = 0,
        [switch]$NoPattern
    )

    $path = Join-Path $OutputDir $Name
    Remove-Item $path, ([IO.Path]::ChangeExtension($path, '.raw')) -ErrorAction SilentlyContinue

    Write-Host "creating $Name"
    & $Create $path | Out-Null

    if (-not $NoPattern) {
        Write-Pattern -VhdPath $path -Seed $Seed
    }
    Convert-ToRaw -Path $path
}

# --------------------------------------------------------------------- fixtures

# Fixed and dynamic, both formats. Sector size 512 and 4096 for VHDX, since the
# logical sector size changes the BAT chunk ratio.
New-Image 'fixed.vhd'        { param($p) New-VHD -Path $p -SizeBytes $sizeBytes -Fixed }               -Seed 1
New-Image 'dynamic.vhd'      { param($p) New-VHD -Path $p -SizeBytes $sizeBytes -Dynamic }             -Seed 2
New-Image 'fixed.vhdx'       { param($p) New-VHD -Path $p -SizeBytes $sizeBytes -Fixed }               -Seed 3
New-Image 'dynamic.vhdx'     { param($p) New-VHD -Path $p -SizeBytes $sizeBytes -Dynamic }             -Seed 4
New-Image 'dynamic-4k.vhdx'  { param($p) New-VHD -Path $p -SizeBytes $sizeBytes -Dynamic -LogicalSectorSizeBytes 4096 } -Seed 5

# An empty dynamic disk: every block unallocated, so the whole device must decode
# as zeroes.
New-Image 'empty.vhdx' { param($p) New-VHD -Path $p -SizeBytes $sizeBytes -Dynamic } -NoPattern

# Differencing chains. The child is what the tests open; parents must sit beside
# it so they resolve automatically. Writing to each level in turn is what produces
# partially-written blocks, where sector-level parent fallthrough matters.
foreach ($ext in 'vhd', 'vhdx') {
    $base  = Join-Path $OutputDir "chain-base.$ext"
    $mid   = Join-Path $OutputDir "chain-mid.$ext"
    $child = Join-Path $OutputDir "chain-child.$ext"

    Remove-Item $base, $mid, $child -ErrorAction SilentlyContinue
    Remove-Item ([IO.Path]::ChangeExtension($child, '.raw')) -ErrorAction SilentlyContinue

    Write-Host "creating chain-{base,mid,child}.$ext"
    New-VHD -Path $base -SizeBytes $sizeBytes -Dynamic | Out-Null
    Write-Pattern -VhdPath $base -Seed 10

    New-VHD -Path $mid -ParentPath $base -Differencing | Out-Null
    Write-Pattern -VhdPath $mid -Seed 20

    New-VHD -Path $child -ParentPath $mid -Differencing | Out-Null
    Write-Pattern -VhdPath $child -Seed 30

    # Only the child gets a raw dump: it is the view the chain should present.
    Convert-ToRaw -Path $child
}

# ----------------------------------------------------------------------- report

$images = Get-ChildItem $OutputDir -Include *.vhd, *.vhdx -File
$dumps  = Get-ChildItem $OutputDir -Include *.raw -File
Write-Host ""
Write-Host ("generated {0} image(s), {1} raw dump(s)" -f $images.Count, $dumps.Count)
Write-Host ""
Write-Host "run the corpus tests with:"
Write-Host "    `$env:LIBVHDI_CORPUS = `"$OutputDir`""
Write-Host "    go test ./reader/ -run TestCorpus -v"
