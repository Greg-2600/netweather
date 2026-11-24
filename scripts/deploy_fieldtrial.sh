#!/usr/bin/env bash
# scripts/deploy_fieldtrial.sh
#
# POSIX companion to scripts/deploy_fieldtrial.ps1. Deploys the early field
# trial from Linux/CI: production-sized fleet, direct + Tor vantages, hardened
# runtime, a bounded IODA level feed, health gate, and rollback.
#
# The --profile tor stack starts one unpinned tor service and six pinned
# tor-de/jp/au/sg/cl/mx services. The pinned services stay idle until the
# field-trial overlay sets NETWEATHER_TOR_VANTAGES. The deploy script seeds
# secrets/ui_token as mode 600/UID 65532:65532 before startup.
#
#   ./scripts/deploy_fieldtrial.sh                 # build + deploy
#   ./scripts/deploy_fieldtrial.sh --dry-run       # print the commands, touch nothing
#   ./scripts/deploy_fieldtrial.sh --no-build      # reuse the current local images
#
# Schedule it every two hours with cron:
#   0 */2 * * * /path/to/netweather/scripts/deploy_fieldtrial.sh >> /var/log/netweather-fieldtrial.log 2>&1

set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="netweather:local"
ROLLBACK="netweather:fieldtrial-rollback"
SECRET="nw-fieldtrial-secret"
TOKEN_FILE="secrets/ui_token"
BASE_URL="http://localhost:2600"

COMPOSE_ARGS=(--profile tor -f compose.yaml -f compose.fieldtrial.yaml)
DRY_RUN=0
NO_BUILD=0
for arg in "$@"; do
    case "$arg" in
        --dry-run) DRY_RUN=1 ;;
        --no-build) NO_BUILD=1 ;;
        *) echo "unknown argument: $arg" >&2; exit 2 ;;
    esac
done

step() { echo "==> $1"; }
run() { if [ "$DRY_RUN" = "1" ]; then echo "DRY: $*"; else "$@"; fi; }
compose() { run docker compose "${COMPOSE_ARGS[@]}" "$@"; }

healthy() {
    [ "$DRY_RUN" = "1" ] && return 0
    curl -fsS --max-time 3 "$BASE_URL/healthz" >/dev/null 2>&1
}

wait_health() {
    local attempts="$1" i=0
    [ "$DRY_RUN" = "1" ] && return 0
    while [ "$i" -lt "$attempts" ]; do
        healthy && return 0
        sleep 2
        i=$((i + 1))
    done
    return 1
}

# Legacy standalone containers would fight for :2600. Missing is normal.
step "stopping earlier standalone containers"
run docker rm -f netweather-demo tor-sidecar >/dev/null 2>&1 || true

# --- rollback point ---------------------------------------------------------
if [ "$DRY_RUN" = "0" ] && [ "$NO_BUILD" = "0" ] && docker image inspect "$IMAGE" >/dev/null 2>&1; then
    docker tag "$IMAGE" "$ROLLBACK"
    echo "  rollback image tagged $ROLLBACK"
fi

# --- images -----------------------------------------------------------------
if [ "$NO_BUILD" = "0" ]; then
    step "building the Tor client sidecar image"
    run docker build -f Dockerfile.tor -t netweather-tor:local .
    # Each exit pin is baked into its own image, so rebuild all six or `up -d`
    # may keep running a sidecar built before a torrc edit.
    for cc in DE JP AU SG CL MX; do
        lower=$(printf '%s' "$cc" | tr '[:upper:]' '[:lower:]')
        step "building the Tor client sidecar image for $cc"
        run docker build -f Dockerfile.tor --build-arg "TOR_EXIT_COUNTRY=$cc" \
            -t "netweather-tor-$lower:local" .
    done
    step "building $IMAGE"
    run docker build -t "$IMAGE" .
else
    step "reusing existing images"
fi

# --- secret -----------------------------------------------------------------
# A named volume carries Unix mode 0600, which a Windows bind mount cannot.
step "preparing the token secret volume"
run docker volume create "$SECRET"
if [ "$DRY_RUN" = "0" ]; then
    if [ ! -f "$TOKEN_FILE" ]; then
        echo "token file not found: $TOKEN_FILE" >&2
        exit 1
    fi
    ABS_TOKEN="$(cd "$(dirname "$TOKEN_FILE")" && pwd)/$(basename "$TOKEN_FILE")"
    run docker run --rm -v "${SECRET}:/sec" -v "${ABS_TOKEN}:/tmp/token:ro" alpine:3.20 \
        sh -c 'cp /tmp/token /sec/ui_token && chown 65532:65532 /sec/ui_token && chmod 600 /sec/ui_token'
fi

# --- recreate ---------------------------------------------------------------
step "recreating the field trial"
compose down --remove-orphans
compose up -d

# --- health gate ------------------------------------------------------------
step "waiting for /healthz"
if ! wait_health 60; then
    echo "ERROR: field trial did not become healthy; attempting rollback" >&2
    if [ "$DRY_RUN" = "0" ]; then
        compose down --remove-orphans
        if docker image inspect "$ROLLBACK" >/dev/null 2>&1; then
            docker tag "$ROLLBACK" "$IMAGE"
            compose up -d
            if wait_health 30; then
                echo "rolled back to $ROLLBACK"
            else
                echo "rollback also failed to become healthy; leaving the stack down" >&2
            fi
        else
            echo "no rollback image available; leaving the stack down" >&2
        fi
    fi
    exit 1
fi

if [ "$DRY_RUN" = "1" ]; then
    exit 0
fi

compose ps
echo ""
step "startup summary"
docker compose "${COMPOSE_ARGS[@]}" logs netweather 2>&1 | grep -E 'tor vantage enabled|listening|ioda country level feed enabled' | tail -5 || true
TOKEN="$(docker run --rm -v "${SECRET}:/s:ro" alpine:3.20 cat /s/ui_token 2>/dev/null || true)"
echo ""
echo "field trial: $BASE_URL"
[ -n "$TOKEN" ] && echo "token:       $TOKEN"
echo "verify:      ./scripts/check_fieldtrial.sh"
echo "stop:        docker compose ${COMPOSE_ARGS[*]} down"
