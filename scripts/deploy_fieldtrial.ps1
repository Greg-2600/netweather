# scripts/deploy_fieldtrial.ps1
#
# Deploys the early field trial: production-sized target fleet (1000 targets),
# direct + Tor vantages, hardened runtime, health gate, and rollback.
#
# The default stack starts the unpinned tor sidecar from the signed release
# image. The --profile tor stack adds the six pinned tor-de/jp/au/sg/cl/mx
# services, which stay idle until the field-trial overlay sets
# NETWEATHER_TOR_VANTAGES. The deploy script seeds secrets/ui_token as mode
# 600/UID 65532:65532 before startup.
#
#   .\scripts\deploy_fieldtrial.ps1               # build + deploy
#   .\scripts\deploy_fieldtrial.ps1 -DryRun       # print the commands, touch nothing
#   .\scripts\deploy_fieldtrial.ps1 -NoBuild      # reuse the current local images
param(
    [switch]$DryRun,
    [switch]$NoBuild
)

$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

$compose  = @('--profile', 'tor', '-f', 'compose.yaml', '-f', 'compose.fieldtrial.yaml')
$image    = 'netweather:local'
$rollback = 'netweather:fieldtrial-rollback'
$secret   = 'nw-fieldtrial-secret'

function Step([string]$msg) { Write-Host "==> $msg" }

function Invoke-Docker {
    param([string[]]$DockerArgs)
    if ($DryRun) {
        Write-Host "DRY: docker $($DockerArgs -join ' ')"
        return
    }
    & docker @DockerArgs
    if ($LASTEXITCODE -ne 0) {
        throw "docker $($DockerArgs -join ' ') failed (exit $LASTEXITCODE)"
    }
}

function Invoke-Compose {
    param([string[]]$ComposeArgs)
    $full = @('compose') + $compose + $ComposeArgs
    if ($DryRun) {
        Write-Host "DRY: docker $($full -join ' ')"
        return
    }
    & docker @full
    if ($LASTEXITCODE -ne 0) {
        throw "docker $($full -join ' ') failed (exit $LASTEXITCODE)"
    }
}

function Test-Health {
    if ($DryRun) { return $true }
    try {
        $res = Invoke-WebRequest -Uri 'http://localhost:2600/healthz' -UseBasicParsing -TimeoutSec 3
        return $res.StatusCode -eq 200
    } catch {
        return $false
    }
}

function Wait-Health([int]$Attempts) {
    if ($DryRun) { return $true }
    for ($i = 0; $i -lt $Attempts; $i++) {
        if (Test-Health) { return $true }
        Start-Sleep -Seconds 2
    }
    return $false
}

function Show-Result {
    if ($DryRun) { return }
    docker compose @compose ps
    Write-Host ''
    Step 'startup summary'
    docker compose @compose logs netweather 2>&1 |
        Select-String -Pattern 'tor vantage enabled', 'listening' | Select-Object -Last 2
    $token = (docker run --rm -v "${secret}:/s:ro" alpine:3.20 cat /s/ui_token 2>$null)
    Write-Host ''
    Write-Host 'field trial: http://localhost:2600'
    if ($token) { Write-Host "token:       $($token.Trim())" }
    Write-Host "stop:        docker compose $($compose -join ' ') down"
}

# The standalone demo containers from earlier sessions would fight for :2600.
# Missing containers are normal, so this call deliberately ignores its exit code.
Step 'stopping earlier standalone containers'
if ($DryRun) {
    Write-Host 'DRY: docker rm -f netweather-demo tor-sidecar'
} else {
    docker rm -f netweather-demo tor-sidecar *> $null
}

# --- rollback point ---------------------------------------------------------
if (-not $DryRun -and -not $NoBuild) {
    docker tag $image $rollback *> $null
    if ($LASTEXITCODE -eq 0) {
        Write-Host "  rollback image tagged $rollback"
    }
}

# --- images -----------------------------------------------------------------
if (-not $NoBuild) {
    # Each regional vantage is a separate image because its exit pin is baked
    # into torrc. Build them here so a torrc or Dockerfile edit cannot leave an
    # old pinned sidecar running after `up -d`.
    foreach ($cc in @('de', 'jp', 'au', 'sg', 'cl', 'mx')) {
        Step "building the Tor client sidecar image for $($cc.ToUpperInvariant())"
        Invoke-Docker @('build', '-f', 'Dockerfile.tor', '--build-arg', "TOR_EXIT_COUNTRY=$($cc.ToUpperInvariant())",
            '-t', "netweather-tor-$cc`:local", '.')
    }
    Step "building $image"
    Invoke-Docker @('build', '-t', $image, '.')
} else {
    Step 'reusing existing images'
}

# --- secret -----------------------------------------------------------------
# A Windows bind mount cannot carry Unix mode 0600, which the app requires of
# a token file. Keep the production token in a named volume instead.
Step 'preparing the token secret volume'
Invoke-Docker @('volume', 'create', $secret)
if (-not $DryRun) {
    $absToken = (Resolve-Path '.\secrets\ui_token').Path
    Invoke-Docker @('run', '--rm', '-v', "${secret}:/sec", '-v', "${absToken}:/tmp/token:ro",
        'alpine:3.20', 'sh', '-c',
        'cp /tmp/token /sec/ui_token && chown 65532:65532 /sec/ui_token && chmod 600 /sec/ui_token')
}

# --- recreate ---------------------------------------------------------------
Step 'recreating the field trial'
Invoke-Compose @('down', '--remove-orphans')
Invoke-Compose @('up', '-d')

# --- health gate ------------------------------------------------------------
Step 'waiting for /healthz'
if (-not (Wait-Health 60)) {
    Write-Host 'ERROR: field trial did not become healthy; attempting rollback'
    if (-not $DryRun) {
        Invoke-Compose @('down', '--remove-orphans')
        docker tag $rollback $image *> $null
        if ($LASTEXITCODE -eq 0) {
            Invoke-Compose @('up', '-d')
            if (Wait-Health 30) {
                Write-Host "rolled back to $rollback"
            } else {
                Write-Host 'rollback also failed to become healthy; leaving the stack down'
            }
        } else {
            Write-Host 'no rollback image available; leaving the stack down'
        }
    }
    exit 1
}

Show-Result
