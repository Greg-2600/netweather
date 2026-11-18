#!/usr/bin/env bash
# scripts/deploy.sh
#
# Builds the netweather release image and runs it as a hardened container,
# replacing the previous one only after the new one answers /healthz.
#
# Usage:
#   ./scripts/deploy.sh                  # build + deploy
#   ./scripts/deploy.sh --dry-run        # print the commands, touch nothing
#   ./scripts/deploy.sh --no-build       # reuse the already-built image
#   IMAGE_NAME=... ./scripts/deploy.sh
#
# Schedule it every two hours with cron:
#   0 */2 * * * /path/to/netweather/scripts/deploy.sh >> /var/log/netweather-deploy.log 2>&1

set -euo pipefail

IMAGE_NAME="${IMAGE_NAME:-ghcr.io/greg-2600/netweather:latest}"
CONTAINER_NAME="${CONTAINER_NAME:-netweather-demo}"
TOKEN_FILE="${TOKEN_FILE:-./secrets/ui_token}"
DATA_VOLUME="${DATA_VOLUME:-netweather-demo-data}"
SECRET_VOLUME="${SECRET_VOLUME:-netweather-demo-secret}"
DRY_RUN=0
NO_BUILD=0
for arg in "$@"; do
    case "$arg" in
        --dry-run) DRY_RUN=1 ;;
        --no-build) NO_BUILD=1 ;;
        *) echo "unknown argument: $arg" >&2; exit 2 ;;
    esac
done

run() {
    if [ "$DRY_RUN" = "1" ]; then
        echo "DRY: $*"
    else
        "$@"
    fi
}

# --- preflight --------------------------------------------------------------
if [ "$DRY_RUN" = "0" ]; then
    if [ ! -f "$TOKEN_FILE" ]; then
        echo "token file not found: $TOKEN_FILE" >&2
        exit 1
    fi
    if [ "$(stat -c %a "$TOKEN_FILE" 2>/dev/null || stat -f %Lp "$TOKEN_FILE")" != "600" ]; then
        echo "WARNING: $TOKEN_FILE is not 0600; the app refuses group/world-readable token files" >&2
    fi
fi

# --- volumes ---------------------------------------------------------------
echo "==> ensuring volumes"
run docker volume create "$DATA_VOLUME"
run docker volume create "$SECRET_VOLUME"

if [ "$DRY_RUN" = "0" ]; then
    docker run --rm -v "${SECRET_VOLUME}:/run/secrets" -v "${TOKEN_FILE}:/src/token:ro" alpine:3.20 \
        sh -c 'cp /src/token /run/secrets/ui_token && chmod 600 /run/secrets/ui_token && chown 65532:65532 /run/secrets/ui_token'
fi

# --- rollback point ---------------------------------------------------------
ROLLBACK="netweather:rollback"
if [ "$DRY_RUN" = "0" ]; then
    if docker image inspect "$IMAGE_NAME" >/dev/null 2>&1; then
        docker tag "$IMAGE_NAME" "$ROLLBACK"
        echo "  rollback image tagged $ROLLBACK"
    fi
fi

# --- build -----------------------------------------------------------------
if [ "$NO_BUILD" = "0" ]; then
    echo "==> building $IMAGE_NAME"
    run docker build -t "$IMAGE_NAME" .
fi

# --- stop previous ---------------------------------------------------------
echo "==> stopping previous instance"
if [ "$DRY_RUN" = "0" ]; then
    docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
fi

# --- run -------------------------------------------------------------------
echo "==> starting new instance"
run docker run -d --name "$CONTAINER_NAME" \
    -p 2600:2600 \
    -v "${DATA_VOLUME}:/data" \
    -v "${SECRET_VOLUME}:/run/secrets:ro" \
    -e NETWEATHER_UI_LISTEN=0.0.0.0:2600 \
    -e NETWEATHER_UI_TOKEN_FILE=/run/secrets/ui_token \
    -e NETWEATHER_DATA=/data \
    -e NETWEATHER_LOG_LEVEL=info \
    --read-only --tmpfs /tmp \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --sysctl "net.ipv4.ping_group_range=0 2147483647" \
    --restart unless-stopped \
    "$IMAGE_NAME"

# --- health gate -----------------------------------------------------------
echo "==> waiting for /healthz"
healthy=0
if [ "$DRY_RUN" = "1" ]; then
    healthy=1
fi
i=0
while [ "$healthy" = "0" ] && [ "$i" -lt 30 ]; do
    sleep 1
    i=$((i + 1))
    if docker exec "$CONTAINER_NAME" /netweather healthcheck >/dev/null 2>&1; then
        healthy=1
    fi
done

if [ "$healthy" = "0" ]; then
    echo "ERROR: new instance did not become healthy; attempting rollback" >&2
    if [ "$DRY_RUN" = "0" ]; then
        docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
        if docker image inspect "$ROLLBACK" >/dev/null 2>&1; then
            docker run -d --name "$CONTAINER_NAME" \
                -p 2600:2600 -v "${DATA_VOLUME}:/data" -v "${SECRET_VOLUME}:/run/secrets:ro" \
                -e NETWEATHER_UI_LISTEN=0.0.0.0:2600 -e NETWEATHER_UI_TOKEN_FILE=/run/secrets/ui_token -e NETWEATHER_DATA=/data \
                --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges:true \
                --sysctl "net.ipv4.ping_group_range=0 2147483647" --restart unless-stopped \
                "$ROLLBACK" >/dev/null
            echo "rolled back to $ROLLBACK"
        else
            echo "no rollback image available; leaving the instance stopped" >&2
        fi
    fi
    exit 1
fi

token=""
if [ "$DRY_RUN" = "0" ]; then
    token=$(docker run --rm -v "${SECRET_VOLUME}:/s" alpine:3.20 cat /s/ui_token 2>/dev/null || true)
fi

echo ""
echo "deployed: http://localhost:2600"
[ -n "$token" ] && echo "token:    $token"
echo "logs:     docker logs -f $CONTAINER_NAME"
echo "stop:     docker rm -f $CONTAINER_NAME"
