# scripts/install-schedule.ps1
#
# Registers a Windows Task Scheduler job that runs scripts/deploy.ps1 every two
# hours, so a fresh build lands in the container on a cadence. The task runs as
# the current user with highest available privileges and only when the network
# is available.
#
#   .\scripts\install-schedule.ps1
#   .\scripts\install-schedule.ps1 -Uninstall

param([switch]$Uninstall)

$taskName = 'netweather-deploy'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$script = Join-Path $root 'scripts\deploy.ps1'
$workDir = $root

if ($Uninstall) {
    schtasks /delete /tn $taskName /f 2>&1 | Out-Null
    Write-Host "removed scheduled task $taskName"
    exit 0
}

if (-not (Test-Path $script)) {
    throw "deploy script not found: $script"
}

# schtasks arguments: /create, /tn name, /tr action, /sc hourly /mo 2, /st start.
$action = "powershell.exe -NoProfile -ExecutionPolicy Bypass -File `"$script`""
& schtasks /create /tn $taskName /tr $action /sc HOURLY /mo 2 /st "00:00" /ru $env:USERNAME /rl HIGHEST /f
if ($LASTEXITCODE -ne 0) { throw "schtasks create failed (exit $LASTEXITCODE)" }

Write-Host "scheduled '$taskName' every 2 hours"
Write-Host "working dir: $workDir"
Write-Host "verify with: schtasks /query /tn $taskName /v"
Write-Host "remove with: .\scripts\install-schedule.ps1 -Uninstall"

# On Linux/macOS, use cron instead:
#   0 */2 * * * /path/to/netweather/scripts/deploy.sh >> /var/log/netweather-deploy.log 2>&1
