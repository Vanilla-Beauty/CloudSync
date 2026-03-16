#Requires -Version 5.1
<#
.SYNOPSIS
    Register, start, stop, or remove the cloudsyncd Windows service.

.DESCRIPTION
    Registers cloudsyncd.exe as a Windows service so it starts automatically
    at boot (no login required). Requires administrator privileges.

.PARAMETER Uninstall
    Stop and remove the Windows service.

.PARAMETER Status
    Show the current service status.

.EXAMPLE
    # Run as Administrator:
    .\enable-autostart.ps1              # install & start service
    .\enable-autostart.ps1 -Uninstall   # remove service
    .\enable-autostart.ps1 -Status      # show status
#>
[CmdletBinding(DefaultParameterSetName = 'Install')]
param(
    [Parameter(ParameterSetName = 'Uninstall')]
    [switch]$Uninstall,

    [Parameter(ParameterSetName = 'Status')]
    [switch]$Status
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ServiceName        = 'cloudsyncd'
$ServiceDisplayName = 'CloudSync Daemon'
$ServiceDescription = 'CloudSync file synchronisation daemon — syncs local folders to Tencent Cloud COS'

# ── colour helpers ────────────────────────────────────────────────────────────

function Write-OK   { param($msg) Write-Host "[OK]  $msg" -ForegroundColor Green  }
function Write-Warn { param($msg) Write-Host "[!]   $msg" -ForegroundColor Yellow }
function Write-Err  { param($msg) Write-Host "[ERR] $msg" -ForegroundColor Red    }
function Write-Head { param($msg) Write-Host "`n$msg" -ForegroundColor Cyan       }

# ── admin check ───────────────────────────────────────────────────────────────

$currentPrincipal = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
$isAdmin = $currentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)

if (-not $isAdmin) {
    Write-Err "This script must be run as Administrator."
    Write-Host "Right-click PowerShell and choose 'Run as administrator', then re-run."
    exit 1
}

# ── status ────────────────────────────────────────────────────────────────────

if ($Status) {
    Write-Head "cloudsyncd service status"
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $svc) {
        Write-Warn "Service '$ServiceName' is not installed."
    } else {
        Write-Host "  Name:        $($svc.Name)"
        Write-Host "  DisplayName: $($svc.DisplayName)"
        Write-Host "  Status:      $($svc.Status)"
        Write-Host "  StartType:   $($svc.StartType)"
    }
    exit 0
}

# ── uninstall ─────────────────────────────────────────────────────────────────

if ($Uninstall) {
    Write-Head "Removing cloudsyncd Windows service..."
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $svc) {
        Write-Warn "Service '$ServiceName' is not installed. Nothing to do."
        exit 0
    }
    if ($svc.Status -eq 'Running') {
        Stop-Service -Name $ServiceName -Force
        Write-OK "Stopped service"
    }
    sc.exe delete $ServiceName | Out-Null
    if ($LASTEXITCODE -eq 0) {
        Write-OK "Removed service '$ServiceName'"
    } else {
        Write-Err "sc.exe delete returned $LASTEXITCODE"
        exit 1
    }
    exit 0
}

# ── locate cloudsyncd.exe ─────────────────────────────────────────────────────

Write-Head "Locating cloudsyncd.exe..."

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
$candidates = @(
    (Join-Path $ScriptDir 'cloudsyncd.exe'),
    (Join-Path $env:LOCALAPPDATA 'CloudSync\bin\cloudsyncd.exe'),
    (Get-Command 'cloudsyncd.exe' -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Source -ErrorAction SilentlyContinue)
) | Where-Object { $_ -and (Test-Path $_) }

if (-not $candidates) {
    Write-Err "cloudsyncd.exe not found."
    Write-Host ""
    Write-Host "  Build and install it first:"
    Write-Host "    .\install.ps1"
    exit 1
}

$DaemonPath = $candidates[0]
Write-OK "Found: $DaemonPath"

# ── install service ───────────────────────────────────────────────────────────

Write-Head "Registering Windows service..."

$existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existing) {
    Write-Warn "Service '$ServiceName' already exists (status: $($existing.Status))."
    $answer = Read-Host "Re-register it? [Y/n]"
    if ($answer -ne '' -and $answer -notmatch '^[Yy]') {
        Write-Warn "Skipped."
        exit 0
    }
    if ($existing.Status -eq 'Running') {
        Stop-Service -Name $ServiceName -Force
        Write-OK "Stopped existing service"
    }
    sc.exe delete $ServiceName | Out-Null
    Start-Sleep -Milliseconds 500
}

New-Service `
    -Name        $ServiceName `
    -DisplayName $ServiceDisplayName `
    -Description $ServiceDescription `
    -BinaryPathName $DaemonPath `
    -StartupType Automatic | Out-Null

Write-OK "Service '$ServiceName' registered (StartupType: Automatic)"

# ── start the service now ─────────────────────────────────────────────────────

Write-Head "Starting service..."
Start-Service -Name $ServiceName
$svc = Get-Service -Name $ServiceName
if ($svc.Status -eq 'Running') {
    Write-OK "Service is running"
} else {
    Write-Warn "Service status: $($svc.Status) — check Event Viewer for details"
}

# ── summary ───────────────────────────────────────────────────────────────────

Write-Host ""
Write-OK "Done."
Write-Host ""
Write-Host "  Check status:    Get-Service $ServiceName"
Write-Host "  View logs:       Get-EventLog -LogName Application -Source $ServiceName -Newest 20"
Write-Host "  Stop service:    Stop-Service $ServiceName"
Write-Host "  Remove service:  .\enable-autostart.ps1 -Uninstall"
Write-Host ""
