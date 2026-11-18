# scripts/deploy.ps1
#
# Builds the netweather release image and runs it as a hardened container,
# replacing the previous one only after the new one answers /healthz.
#
# This is the "working software deployed" step. Run it manually, or schedule it
# every two hours (see scripts/install-schedule.ps1).
#
#   .\scripts\deploy.ps1               # build + deploy
#   .\scripts\deploy.ps1 -DryRun       # print the commands, touch nothing
#   .\scripts\deploy.ps1 -NoBuild      # reuse the already-built image
param(
    [string]$ImageName    = 'ghcr.io/greg-2600/netweather:latest',
    [string]$ContainerName = 'netweather-demo',
    [string]$TokenFile    = '.\secrets\ui_token',
    [string]$DataVolume   = 'netweather-demo-data',
    [string]$SecretVolume = 'netweather-demo-secret',
    [switch]$DryRun,
    [switch]$NoBuild
)

$ErrorActionPreference = 'Stop'

function Step([string]$msg) { Write-Host "==> $msg" }
function Invoke-Docker {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Args)
    if ($DryRun) {
        Write-Host "DRY: docker $($Args -join ' ')"
        return
    }
    & docker @Args
    if ($LASTEXITCODE -ne 0) { throw "docker $($Args -join ' ') failed (exit $LASTEXITCODE)" }
}

# --- preflight -------------------------------------------------------------
if (-not $DryRun) {
    if (-not (Test-Path $TokenFile)) {
        throw "token file not found: $TokenFile  (create it first; see README)"
    }
}

# --- volumes (Docker initialises ownership from the image on first use) ------
Step 'ensuring volumes'
Invoke-Docker volume create $DataVolume
Invoke-Docker volume create $SecretVolume

if (-not $DryRun) {
    $absToken = (Resolve-Path $TokenFile).Path
    docker run --rm -v "${SecretVolume}:/run/secrets" -v "${absToken}:/src/token:ro" alpine:3.20 `
        sh -c 'cp /src/token /run/secrets/ui_token && chmod 600 /run/secrets/ui_token && chown 65532:65532 /run/secrets/ui_token' 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'could not seed the secret volume' }
}

# --- rollback point: keep the current image under a known tag ----------------
$rollback = 'netweather:rollback'
if (-not $DryRun) {
    $existing = docker image inspect $ImageName 2>&1
    if ($LASTEXITCODE -eq 0) {
        docker tag $ImageName $rollback 2>&1 | Out-Null
        Write-Host "  rollback image tagged $rollback"
    }
}

# --- build -----------------------------------------------------------------
if (-not $NoBuild) {
    Step "building $ImageName"
    Invoke-Docker build -t $ImageName .
}

# --- stop the previous instance --------------------------------------------
Step 'stopping previous instance'
if (-not $DryRun) {
    docker rm -f $ContainerName 2>&1 | Out-Null
}

# --- run the new instance ---------------------------------------------------
Step 'starting new instance'
$runArgs = @(
    'run', '-d', '--name', $ContainerName,
    '-p', '2600:2600',
    '-v', "$DataVolume`:/data",
    '-v', "$SecretVolume`:/run/secrets:ro",
    '-e', 'NETWEATHER_UI_LISTEN=0.0.0.0:2600',
    '-e', 'NETWEATHER_UI_TOKEN_FILE=/run/secrets/ui_token',
    '-e', 'NETWEATHER_DATA=/data',
    '-e', 'NETWEATHER_LOG_LEVEL=info',
    '--read-only', '--tmpfs', '/tmp',
    '--cap-drop', 'ALL',
    '--security-opt', 'no-new-privileges:true',
    '--sysctl', 'net.ipv4.ping_group_range=0 2147483647',
    '--restart', 'unless-stopped',
    $ImageName
)
Invoke-Docker @runArgs

# --- health gate ------------------------------------------------------------
Step 'waiting for /healthz'
$healthy = $false
if ($DryRun) {
    $healthy = $true
} else {
    for ($i = 0; $i -lt 30; $i++) {
        Start-Sleep -Seconds 1
        $null = docker exec $ContainerName /netweather healthcheck 2>&1
        if ($LASTEXITCODE -eq 0) { $healthy = $true; break }
    }
}
if (-not $healthy) {
    Write-Host 'ERROR: new instance did not become healthy; attempting rollback'
    if (-not $DryRun) {
        docker rm -f $ContainerName 2>&1 | Out-Null
        $rb = docker image inspect $rollback 2>&1
        if ($LASTEXITCODE -eq 0) {
            docker run -d --name $ContainerName `
                -p 2600:2600 -v "$DataVolume`:/data" -v "$SecretVolume`:/run/secrets:ro" `
                -e NETWEATHER_UI_LISTEN=0.0.0.0:2600 -e NETWEATHER_UI_TOKEN_FILE=/run/secrets/ui_token -e NETWEATHER_DATA=/data `
                --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges:true `
                --sysctl 'net.ipv4.ping_group_range=0 2147483647' --restart unless-stopped `
                $rollback 2>&1 | Out-Null
            Write-Host "rolled back to $rollback"
        } else {
            Write-Host 'no rollback image available; leaving the instance stopped'
        }
    }
    exit 1
}

if ($DryRun) {
    Write-Host ''
    Write-Host 'dry run complete: no container was touched'
    exit 0
}

$token = ''
try { $token = (docker run --rm -v "${SecretVolume}:/s" alpine:3.20 cat /s/ui_token 2>$null) } catch { $token = '' }
Write-Host ''
Write-Host 'deployed: http://localhost:2600'
if ($token) { Write-Host "token:    $token" }
Write-Host "logs:     docker logs -f $ContainerName"
Write-Host "stop:     docker rm -f $ContainerName"
