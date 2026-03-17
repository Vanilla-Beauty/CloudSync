#Requires -Version 5.1
<#
.SYNOPSIS
    Register or remove cloudsyncd as a per-user autostart task.

.DESCRIPTION
    Creates a Task Scheduler task that launches cloudsyncd.exe when the
    current user logs in.  The task runs as the current user (not SYSTEM),
    so it has access to %APPDATA%\cloudsync\config.json and the Named Pipe
    is created in the interactive user session.

    No administrator privileges are required.

.PARAMETER Uninstall
    Stop the running instance and remove the scheduled task.

.PARAMETER Status
    Show the current task status.

.EXAMPLE
    .\enable-autostart.ps1              # register & start
    .\enable-autostart.ps1 -Uninstall   # remove
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

$TaskName = 'CloudSync Daemon'
$TaskPath = '\CloudSync\'

# ── colour helpers ────────────────────────────────────────────────────────────

function Write-OK   { param($msg) Write-Host "[OK]  $msg" -ForegroundColor Green  }
function Write-Warn { param($msg) Write-Host "[!]   $msg" -ForegroundColor Yellow }
function Write-Err  { param($msg) Write-Host "[ERR] $msg" -ForegroundColor Red    }
function Write-Head { param($msg) Write-Host "`n$msg" -ForegroundColor Cyan       }

# ── status ────────────────────────────────────────────────────────────────────

if ($Status) {
    Write-Head "CloudSync autostart task status"
    $task = Get-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath -ErrorAction SilentlyContinue
    if (-not $task) {
        Write-Warn "Task '$TaskName' is not registered."
    } else {
        $info = Get-ScheduledTaskInfo -TaskName $TaskName -TaskPath $TaskPath
        Write-Host "  Task:          $TaskPath$TaskName"
        Write-Host "  State:         $($task.State)"
        Write-Host "  Last run:      $($info.LastRunTime)"
        Write-Host "  Last result:   0x$($info.LastTaskResult.ToString('X'))"
        Write-Host "  Next run:      $($info.NextRunTime)"
    }

    # Also show whether the daemon is currently responding
    $socketPath = '\\.\pipe\cloudsyncd'
    $csExe = (Get-Command cloudsync.exe -ErrorAction SilentlyContinue)?.Source
    if (-not $csExe) {
        $csExe = Join-Path $env:LOCALAPPDATA 'CloudSync\bin\cloudsync.exe'
    }
    if (Test-Path $csExe) {
        $result = & $csExe status 2>&1
        Write-Host ""
        Write-Host $result
    }
    exit 0
}

# ── locate cloudsyncd.exe ─────────────────────────────────────────────────────

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
$candidates = @(
    (Join-Path $ScriptDir 'cloudsyncd.exe'),
    (Join-Path $env:LOCALAPPDATA 'CloudSync\bin\cloudsyncd.exe'),
    (Get-Command 'cloudsyncd.exe' -ErrorAction SilentlyContinue |
        Select-Object -ExpandProperty Source -ErrorAction SilentlyContinue)
) | Where-Object { $_ -and (Test-Path $_) }

if (-not $candidates) {
    Write-Err "cloudsyncd.exe not found. Run .\install.ps1 first."
    exit 1
}
$DaemonPath = $candidates[0]

# ── uninstall ─────────────────────────────────────────────────────────────────

if ($Uninstall) {
    Write-Head "Removing CloudSync autostart task..."
    $task = Get-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath -ErrorAction SilentlyContinue
    if (-not $task) {
        Write-Warn "Task '$TaskName' is not registered. Nothing to do."
        exit 0
    }
    Unregister-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath -Confirm:$false
    Write-OK "Task removed."

    # Stop the running daemon if any
    $csExe = Join-Path (Split-Path $DaemonPath) 'cloudsync.exe'
    if (Test-Path $csExe) {
        & $csExe stop 2>&1 | Out-Null
        Write-OK "Daemon stopped."
    }
    exit 0
}

# ── install scheduled task ────────────────────────────────────────────────────

Write-Head "Registering autostart task for current user..."
Write-OK "Daemon:  $DaemonPath"
Write-OK "User:    $env:USERDOMAIN\$env:USERNAME"

# Remove stale task if present
$existing = Get-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath -ErrorAction SilentlyContinue
if ($existing) {
    Write-Warn "Task already exists — re-registering."
    Unregister-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath -Confirm:$false
}

# Build task components
$action  = New-ScheduledTaskAction -Execute $DaemonPath

# Trigger: at logon of the current user
$trigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"

# Settings: allow on battery, don't stop if idle, restart on failure
$settings = New-ScheduledTaskSettingsSet `
    -ExecutionTimeLimit     (New-TimeSpan -Days 3650) `
    -DisallowStartIfOnBatteries:$false `
    -StopIfGoingOnBatteries:$false `
    -RestartCount           3 `
    -RestartInterval        (New-TimeSpan -Minutes 1) `
    -MultipleInstances      IgnoreNew

# Principal: run as current user, highest available privilege, interactive session
$principal = New-ScheduledTaskPrincipal `
    -UserId   "$env:USERDOMAIN\$env:USERNAME" `
    -LogonType Interactive `
    -RunLevel Highest

Register-ScheduledTask `
    -TaskName  $TaskName `
    -TaskPath  $TaskPath `
    -Action    $action `
    -Trigger   $trigger `
    -Settings  $settings `
    -Principal $principal `
    -Force | Out-Null

Write-OK "Task registered: $TaskPath$TaskName"

# ── start now ────────────────────────────────────────────────────────────────

Write-Head "Starting daemon now..."
$csExe = Join-Path (Split-Path $DaemonPath) 'cloudsync.exe'
if (Test-Path $csExe) {
    # Stop any stale instance first
    & $csExe stop 2>&1 | Out-Null
    Start-Sleep -Milliseconds 500
    $out = & $csExe start 2>&1
    if ($LASTEXITCODE -eq 0) {
        Write-OK $out
    } else {
        Write-Warn "cloudsync start: $out"
    }
} else {
    # Fall back to starting cloudsyncd directly
    Start-Process -FilePath $DaemonPath -WindowStyle Hidden
    Write-OK "Daemon launched."
}

# ── summary ──────────────────────────────────────────────────────────────────

Write-Host ""
Write-OK "Done. cloudsyncd will start automatically when you log in."
Write-Host ""
Write-Host "  Check status:    .\enable-autostart.ps1 -Status"
Write-Host "  Remove task:     .\enable-autostart.ps1 -Uninstall"
Write-Host "  View logs:       $env:APPDATA\cloudsync\cloudsyncd.log"
Write-Host ""
