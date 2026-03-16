#Requires -Version 5.1
<#
.SYNOPSIS
    Build and install CloudSync on Windows.

.DESCRIPTION
    Builds cloudsync.exe and cloudsyncd.exe then installs them to a
    directory on your PATH.

.PARAMETER InstallDir
    Directory to install binaries into.
    Default: $env:LOCALAPPDATA\CloudSync\bin

.PARAMETER GoPath
    Use 'go env GOBIN' (or GOPATH\bin) as the install directory.

.PARAMETER Uninstall
    Remove installed binaries and optional PATH entry.

.EXAMPLE
    .\install.ps1
    .\install.ps1 -InstallDir "C:\Tools"
    .\install.ps1 -GoPath
    .\install.ps1 -Uninstall
#>
[CmdletBinding(DefaultParameterSetName = 'Install')]
param(
    [Parameter(ParameterSetName = 'Install')]
    [string]$InstallDir = "",

    [Parameter(ParameterSetName = 'GoPath')]
    [switch]$GoPath,

    [Parameter(ParameterSetName = 'Uninstall')]
    [switch]$Uninstall
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# ── colour helpers ────────────────────────────────────────────────────────────

function Write-OK    { param($msg) Write-Host "[OK]  $msg" -ForegroundColor Green  }
function Write-Warn  { param($msg) Write-Host "[!]   $msg" -ForegroundColor Yellow }
function Write-Err   { param($msg) Write-Host "[ERR] $msg" -ForegroundColor Red    }
function Write-Head  { param($msg) Write-Host "`n$msg" -ForegroundColor Cyan       }

# ── resolve script dir ────────────────────────────────────────────────────────

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
Set-Location $ScriptDir

if (-not (Test-Path "$ScriptDir\go.mod")) {
    Write-Err "go.mod not found. Run this script from the CloudSync project root."
    exit 1
}

# ── uninstall ─────────────────────────────────────────────────────────────────

if ($Uninstall) {
    Write-Head "Uninstalling CloudSync..."
    $binaries = @('cloudsync.exe', 'cloudsyncd.exe')
    $found = $false
    foreach ($dir in ($env:PATH -split ';')) {
        foreach ($bin in $binaries) {
            $p = Join-Path $dir $bin
            if (Test-Path $p) {
                Remove-Item $p -Force
                Write-OK "Removed $p"
                $found = $true
            }
        }
    }
    if (-not $found) { Write-Warn "No binaries found in PATH." }

    # Stop and remove Windows service if registered
    $svc = Get-Service -Name 'cloudsyncd' -ErrorAction SilentlyContinue
    if ($svc) {
        Stop-Service -Name 'cloudsyncd' -Force -ErrorAction SilentlyContinue
        sc.exe delete cloudsyncd | Out-Null
        Write-OK "Removed Windows service 'cloudsyncd'"
    }
    Write-OK "Uninstall complete."
    exit 0
}

# ── check prerequisites ───────────────────────────────────────────────────────

Write-Head "Checking prerequisites..."

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Err "Go is not installed or not in PATH. Download from https://go.dev/dl/"
    exit 1
}

$goVer = (go version) -replace 'go version go([0-9]+\.[0-9]+).*','$1'
$parts = $goVer -split '\.'
if ([int]$parts[0] -lt 1 -or ([int]$parts[0] -eq 1 -and [int]$parts[1] -lt 21)) {
    Write-Err "Go 1.21+ required. Found: go version $goVer"
    exit 1
}
Write-OK "Go $goVer"

# ── resolve install directory ─────────────────────────────────────────────────

if ($GoPath) {
    $gobin = (go env GOBIN).Trim()
    if (-not $gobin) { $gobin = Join-Path (go env GOPATH).Trim() 'bin' }
    $InstallDir = $gobin
} elseif (-not $InstallDir) {
    $InstallDir = Join-Path $env:LOCALAPPDATA 'CloudSync\bin'
}

Write-OK "Install directory: $InstallDir"

# ── build ─────────────────────────────────────────────────────────────────────

Write-Head "Building..."

$TmpDir = Join-Path $env:TEMP "cloudsync-build-$(Get-Random)"
New-Item -ItemType Directory -Path $TmpDir | Out-Null

try {
    foreach ($bin in @('cloudsync', 'cloudsyncd')) {
        Write-Host "  Building $bin.exe ..." -NoNewline
        $out = Join-Path $TmpDir "$bin.exe"
        & go build -ldflags="-s -w" -o $out "./cmd/$bin/"
        if ($LASTEXITCODE -ne 0) {
            Write-Host " FAILED" -ForegroundColor Red
            Write-Err "Build failed for $bin"
            exit 1
        }
        Write-Host " done" -ForegroundColor Green
    }

    # ── install ───────────────────────────────────────────────────────────────

    Write-Head "Installing to $InstallDir ..."

    if (-not (Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
        Write-OK "Created $InstallDir"
    }

    foreach ($bin in @('cloudsync', 'cloudsyncd')) {
        $src = Join-Path $TmpDir "$bin.exe"
        $dst = Join-Path $InstallDir "$bin.exe"
        Copy-Item -Path $src -Destination $dst -Force
        Write-OK "Installed $dst"
    }

} finally {
    Remove-Item -Path $TmpDir -Recurse -Force -ErrorAction SilentlyContinue
}

# ── PATH check ────────────────────────────────────────────────────────────────

$pathDirs = $env:PATH -split ';' | ForEach-Object { $_.TrimEnd('\') }
$installDirNorm = $InstallDir.TrimEnd('\')

if ($installDirNorm -notin $pathDirs) {
    Write-Warn "$InstallDir is not in your PATH."
    $answer = Read-Host "Add it to your user PATH now? [Y/n]"
    if ($answer -eq '' -or $answer -match '^[Yy]') {
        $currentUser = [System.Environment]::GetEnvironmentVariable('PATH', 'User')
        $newPath = if ($currentUser) { "$currentUser;$InstallDir" } else { $InstallDir }
        [System.Environment]::SetEnvironmentVariable('PATH', $newPath, 'User')
        $env:PATH = "$env:PATH;$InstallDir"
        Write-OK "Added $InstallDir to user PATH (restart your terminal to take effect)"
    } else {
        Write-Warn "Skipped. Manually add to PATH: $InstallDir"
    }
}

# ── verify ────────────────────────────────────────────────────────────────────

Write-Head "Verifying..."
foreach ($bin in @('cloudsync', 'cloudsyncd')) {
    $p = Join-Path $InstallDir "$bin.exe"
    if (Test-Path $p) { Write-OK $p } else { Write-Err "$p not found!" }
}

# ── done ─────────────────────────────────────────────────────────────────────

Write-Host ""
Write-OK "Installation complete!"
Write-Host ""
Write-Host "  Quick start:"
Write-Host "    cloudsync init          # configure COS credentials"
Write-Host "    cloudsync start         # start the daemon"
Write-Host "    cloudsync mount <path>  # start syncing a directory"
Write-Host ""
Write-Host "  To register cloudsyncd as a Windows service that starts at boot:"
Write-Host "    .\enable-autostart.ps1"
Write-Host ""
