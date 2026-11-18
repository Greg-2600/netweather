# scripts/install-schedule.ps1
#
# Registers a Windows Task Scheduler job that runs scripts/deploy.ps1 every two
# hours, so a fresh build lands in the container on a cadence.
#
#   .\scripts\install-schedule.ps1
#   .\scripts\install-schedule.ps1 -Uninstall

param([switch]$Uninstall)

$taskName = 'netweather-deploy'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$script = Join-Path $root 'scripts\deploy.ps1'

if ($Uninstall) {
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
    Write-Host "removed scheduled task $taskName"
    exit 0
}

if (-not (Test-Path $script)) {
    throw "deploy script not found: $script"
}

$action = New-ScheduledTaskAction -Execute 'powershell.exe' `
    -Argument "-NoProfile -ExecutionPolicy Bypass -File `"$script`"" `
    -WorkingDirectory $root
$trigger = New-ScheduledTaskTrigger -Once -At (Get-Date).Date -RepetitionInterval (New-TimeSpan -Hours 2) -RepetitionDuration (New-TimeSpan -Days 3650)
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -DontStopOnIdleEnd -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Hours 1)

# Try the always-on principal first; fall back to an interactive one without
# elevation so a non-admin user still gets a working schedule.
$registered = $false
$mode = ''
$lastErr = ''
foreach ($spec in @(
    @{ Logon = 'S4U';         Elevated = $true },
    @{ Logon = 'Interactive'; Elevated = $false }
)) {
    try {
        if ($spec.Elevated) {
            $principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType $spec.Logon -RunLevel Highest
        } else {
            $principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType $spec.Logon
        }
        Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Settings $settings -Principal $principal -Force -ErrorAction Stop | Out-Null
        $registered = $true
        $mode = $spec.Logon
        break
    } catch {
        $lastErr = $_.Exception.Message
    }
}
if (-not $registered) {
    throw "could not register '$taskName': $lastErr`nRun this script from an elevated PowerShell prompt."
}

Write-Host "scheduled '$taskName' every 2 hours (logon: $mode)"
Write-Host "working dir: $root"
Write-Host "verify with: schtasks /query /tn $taskName /v"
Write-Host "remove with: .\scripts\install-schedule.ps1 -Uninstall"
if ($mode -eq 'Interactive') {
    Write-Host "NOTE: runs only while you are logged in. For runs while logged out, re-run as Administrator."
}

# On Linux/macOS, use cron instead:
#   0 */2 * * * /path/to/netweather/scripts/deploy.sh >> /var/log/netweather-deploy.log 2>&1
