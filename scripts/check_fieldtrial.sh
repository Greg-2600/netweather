#!/usr/bin/env bash
# scripts/check_fieldtrial.sh
#
# Verifies the field trial after a deploy, or soaks it for a while. Every check
# reads the running instance; nothing here invents a healthy answer.
#
#   ./scripts/check_fieldtrial.sh                  # one pass
#   CYCLES=12 SLEEP=300 ./scripts/check_fieldtrial.sh   # soak for ~1 hour
#
# Token resolution (first match wins): NETWEATHER_UI_TOKEN, TOKEN_FILE, then the
# nw-fieldtrial-secret Docker volume written by deploy_fieldtrial.sh.
#   EXPECTED_TARGETS=1000 EXPECTED_CORRIDORS=10 EXPECTED_HUBS=42 BASE_URL=http://localhost:2600  (overridable)

set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

BASE_URL="${BASE_URL:-http://localhost:2600}"
EXPECTED_TARGETS="${EXPECTED_TARGETS:-1000}"
EXPECTED_CORRIDORS="${EXPECTED_CORRIDORS:-10}"
EXPECTED_HUBS="${EXPECTED_HUBS:-42}"
SECRET="${SECRET:-nw-fieldtrial-secret}"
CYCLES="${CYCLES:-1}"
SLEEP="${SLEEP:-60}"
TOKEN_FILE="${TOKEN_FILE:-}"

failures=0
check() {
    local name="$1" ok="$2" detail="${3:-}"
    if [ "$ok" = "0" ]; then
        printf 'PASS  %-28s %s\n' "$name" "$detail"
    else
        printf 'FAIL  %-28s %s\n' "$name" "$detail"
        failures=$((failures + 1))
    fi
}

TOKEN="${NETWEATHER_UI_TOKEN:-}"
if [ -z "$TOKEN" ] && [ -n "$TOKEN_FILE" ] && [ -f "$TOKEN_FILE" ]; then
    TOKEN="$(tr -d '\r\n' < "$TOKEN_FILE")"
fi
if [ -z "$TOKEN" ]; then
    TOKEN="$(docker run --rm -v "${SECRET}:/s:ro" alpine:3.20 cat /s/ui_token 2>/dev/null || true)"
fi

api() {
    curl -fsS --max-time 10 -H "Authorization: Bearer ${TOKEN}" "$BASE_URL$1" 2>/dev/null
}

# The UI uses a session cookie, not a bearer token. Log in once per cycle and
# reuse the jar for every page: logging in separately for /map, /corridors and /
# would trip the login rate limiter on a short check.
ui_login() {
    local jar="$1" page csrf
    page="$(curl -fsS --max-time 10 -c "$jar" "$BASE_URL/login" 2>/dev/null)" || return 1
    csrf="$(printf '%s' "$page" | grep -oE 'name="csrf" value="[^"]+"' | head -1 \
        | sed -E 's/.*value="([^"]+)".*/\1/')"
    [ -n "$csrf" ] || return 1
    curl -fsS --max-time 10 -b "$jar" -c "$jar" -o /dev/null -X POST \
        --data-urlencode "csrf=$csrf" --data-urlencode "token=$TOKEN" \
        --data-urlencode "next=/" "$BASE_URL/login" 2>/dev/null
}

ui_page() {
    curl -fsS --max-time 10 -b "$1" "$BASE_URL$2" 2>/dev/null
}

pass() {
    local cycle=1
    while [ "$cycle" -le "$CYCLES" ]; do
        echo "==> field-trial check $cycle/$CYCLES ($(date -u +%FT%TZ))"
        failures=0

        # 1. Liveness.
        health_code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$BASE_URL/healthz" 2>/dev/null || true)"
        check "/healthz" "$([ "$health_code" = "200" ] && echo 0 || echo 1)" "HTTP $health_code"

        # 2. Fleet size. Count target ids in the authenticated API response.
        targets="$(api /api/v1/targets)"
        target_count="$(printf '%s' "$targets" | grep -oE '"id"[[:space:]]*:' | wc -l | tr -d ' ')"
        check "target fleet" "$([ "$target_count" = "$EXPECTED_TARGETS" ] && echo 0 || echo 1)" \
            "$target_count targets (want $EXPECTED_TARGETS)"

        # 3. Auth actually gates the API; an anonymous read must not be a 200.
        anon_code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$BASE_URL/api/v1/targets" 2>/dev/null || true)"
        check "API auth" "$([ "$anon_code" != "200" ] && echo 0 || echo 1)" "anonymous HTTP $anon_code"

        # One authenticated UI session per cycle, shared by every page read.
        jar="$(mktemp)"
        if ! ui_login "$jar"; then echo "WARN  UI login failed; page checks may be empty"; fi

        # 4. The treemap still renders the whole fleet (session-authenticated).
        rendered_map="$(ui_page "$jar" /map || true)"
        tiles="$(printf '%s' "$rendered_map" | grep -o 'data-testid="map-tile"' | wc -l | tr -d ' ')"
        check "/map tiles" "$([ "$tiles" = "$EXPECTED_TARGETS" ] && echo 0 || echo 1)" "$tiles tiles"

        # 4a. The board's region lens is derived from country codes, with no
        #     schema field. Confirm the toggle exists and produces real buckets.
        rendered_board="$(ui_page "$jar" '/?group=region' || true)"
        region_toggle="$(printf '%s' "$rendered_board" | grep -o 'data-group="region"' | wc -l | tr -d ' ')"
        region_buckets="$(printf '%s' "$rendered_board" | grep -o 'data-sector="[a-z-]*"' | wc -l | tr -d ' ')"
        check "board region lens" "$([ "$region_toggle" = "1" ] && [ "$region_buckets" -gt 1 ] && echo 0 || echo 1)" \
            "$region_toggle toggle, $region_buckets region buckets"

        # 4b. US hubs are named targets, not a schema column; confirm they are
        #     grouped into the expected city tiles rather than an empty page.
        rendered_hubs="$(ui_page "$jar" /hubs || true)"
        hub_tiles="$(printf '%s' "$rendered_hubs" | grep -o 'data-testid="hub"' | wc -l | tr -d ' ')"
        check "/hubs cities" "$([ "$hub_tiles" -le "$EXPECTED_HUBS" ] && [ "$hub_tiles" -gt 0 ] && echo 0 || echo 1)" \
            "$hub_tiles city tiles (at most $EXPECTED_HUBS hubs)"
        hub_map_markers="$(printf '%s' "$rendered_hubs" | grep -o 'data-testid="hub-map-marker"' | wc -l | tr -d ' ')"
        check "/hubs projection" "$([ "$hub_map_markers" -gt 0 ] && echo 0 || echo 1)" \
            "$hub_map_markers US hub map markers"
        hub_projection_line="$(printf '%s' "$rendered_hubs" | grep -n -m1 'data-testid="hub-projection"' | cut -d: -f1 || true)"
        hub_treemap_line="$(printf '%s' "$rendered_hubs" | grep -n -m1 'data-testid="treemap"' | cut -d: -f1 || true)"
        check "/hubs map first" "$([ -n "$hub_projection_line" ] && [ -n "$hub_treemap_line" ] && [ "$hub_projection_line" -lt "$hub_treemap_line" ] && echo 0 || echo 1)" \
            "projection line $hub_projection_line, treemap line $hub_treemap_line"
        hub_asset_code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$BASE_URL/static/us-projection.svg" 2>/dev/null || true)"
        check "/hubs base map" "$([ "$hub_asset_code" = "200" ] && echo 0 || echo 1)" \
            "same-origin SVG HTTP $hub_asset_code"

        # 4a-2. The world view keeps its country tiles and adds a geographic
        # projection with the same health plus approximate hub label anchors.
        rendered_world="$(ui_page "$jar" /world || true)"
        world_projection="$(printf '%s' "$rendered_world" | grep -c 'data-testid="world-projection"' || true)"
        world_country_markers="$(printf '%s' "$rendered_world" | grep -o 'data-testid="world-map-country"' | wc -l | tr -d ' ')"
        world_hub_markers="$(printf '%s' "$rendered_world" | grep -o 'data-testid="world-map-hub"' | wc -l | tr -d ' ')"
        check "/world projection" "$([ "$world_projection" -gt 0 ] && [ "$world_country_markers" -gt 0 ] && [ "$world_hub_markers" -gt 0 ] && echo 0 || echo 1)" \
            "$world_country_markers country markers, $world_hub_markers hub label anchors"
        world_projection_line="$(printf '%s' "$rendered_world" | grep -n -m1 'data-testid="world-projection"' | cut -d: -f1 || true)"
        world_grid_line="$(printf '%s' "$rendered_world" | grep -n -m1 'data-testid="world"' | cut -d: -f1 || true)"
        check "/world map first" "$([ -n "$world_projection_line" ] && [ -n "$world_grid_line" ] && [ "$world_projection_line" -lt "$world_grid_line" ] && echo 0 || echo 1)" \
            "projection line $world_projection_line, grid line $world_grid_line"
        world_asset_code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$BASE_URL/static/world-projection.svg" 2>/dev/null || true)"
        check "/world base map" "$([ "$world_asset_code" = "200" ] && echo 0 || echo 1)" \
            "same-origin SVG HTTP $world_asset_code"
        world_scripts="$(printf '%s' "$rendered_world" | grep -ci '<script' || true)"
        check "/world no scripts" "$([ "$world_scripts" = "0" ] && echo 0 || echo 1)" \
            "$world_scripts script tags"

        # 4a-3. /atlas is the combined map-first view. It must carry the whole
        # fleet, all three projection layers, every tab section, and only
        # same-origin assets; the pin script is the one deliberate addition.
        rendered_atlas="$(ui_page "$jar" /atlas || true)"
        atlas_subtitle="$(printf '%s' "$rendered_atlas" | grep -c 'Everything, all tabs' || true)"
        check "/atlas subtitle" "$([ "$atlas_subtitle" -gt 0 ] && echo 0 || echo 1)" \
            "$atlas_subtitle 'Everything, all tabs' label(s)"
        atlas_tiles="$(printf '%s' "$rendered_atlas" | grep -o 'data-testid="map-tile"' | wc -l | tr -d ' ')"
        check "/atlas fleet tiles" "$([ "$atlas_tiles" = "$EXPECTED_TARGETS" ] && echo 0 || echo 1)" \
            "$atlas_tiles tiles (want $EXPECTED_TARGETS)"
        atlas_countries="$(printf '%s' "$rendered_atlas" | grep -o 'data-testid="atlas-map-country"' | wc -l | tr -d ' ')"
        atlas_hubs="$(printf '%s' "$rendered_atlas" | grep -o 'data-testid="atlas-map-hub"' | wc -l | tr -d ' ')"
        atlas_lines="$(printf '%s' "$rendered_atlas" | grep -o 'data-testid="atlas-map-line"' | wc -l | tr -d ' ')"
        check "/atlas hero layers" "$([ "$atlas_countries" -gt 0 ] && [ "$atlas_hubs" -gt 0 ] && [ "$atlas_lines" = "$EXPECTED_CORRIDORS" ] && echo 0 || echo 1)" \
            "$atlas_countries countries, $atlas_hubs hubs, $atlas_lines lines (want $EXPECTED_CORRIDORS)"
        atlas_sections="$(printf '%s' "$rendered_atlas" | grep -oE 'data-testid="atlas-(glance|board|map|world|hubs|corridors|coverage|incidents|feeds|ihi|diagnostics)"' | wc -l | tr -d ' ')"
        check "/atlas all tabs" "$([ "$atlas_sections" -ge 11 ] && echo 0 || echo 1)" \
            "$atlas_sections detail sections"
        atlas_projection_line="$(printf '%s' "$rendered_atlas" | grep -n -m1 'data-testid="atlas-projection"' | cut -d: -f1 || true)"
        atlas_detail_line="$(printf '%s' "$rendered_atlas" | grep -n -m1 'data-testid="atlas-glance"' | cut -d: -f1 || true)"
        check "/atlas map first" "$([ -n "$atlas_projection_line" ] && [ -n "$atlas_detail_line" ] && [ "$atlas_projection_line" -lt "$atlas_detail_line" ] && echo 0 || echo 1)" \
            "projection line $atlas_projection_line, detail line $atlas_detail_line"
        atlas_pin="$(printf '%s' "$rendered_atlas" | grep -c 'data-testid="atlas-pin"' || true)"
        check "/atlas pin panel" "$([ "$atlas_pin" -gt 0 ] && echo 0 || echo 1)" \
            "$atlas_pin pin panel(s)"
        atlas_script_tags="$(printf '%s' "$rendered_atlas" | grep -ci '<script' || true)"
        atlas_script_src="$(printf '%s' "$rendered_atlas" | grep -ci '<script[^>]*src=' || true)"
        atlas_inline=$((atlas_script_tags - atlas_script_src))
        if [ "$atlas_inline" -lt 0 ]; then atlas_inline=0; fi
        atlas_external="$(printf '%s' "$rendered_atlas" | grep -ciE '(src|href)="https?://' || true)"
        check "/atlas no inline/external" "$([ "$atlas_inline" = "0" ] && [ "$atlas_external" = "0" ] && echo 0 || echo 1)" \
            "$atlas_inline inline scripts ($atlas_script_tags tags, $atlas_script_src with src), $atlas_external external refs"
        atlas_asset_code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$BASE_URL/static/atlas.js" 2>/dev/null || true)"
        check "/atlas pin asset" "$([ "$atlas_asset_code" = "200" ] && echo 0 || echo 1)" \
            "same-origin JS HTTP $atlas_asset_code"

        # 4b. Corridors are operator-supplied; confirm the configured pairs are
        #     actually sampled and rendered rather than leaving an empty page.
        rendered_corridors="$(ui_page "$jar" /corridors || true)"
        corridor_rows="$(printf '%s' "$rendered_corridors" | grep -o 'data-testid="corridor-row"' | wc -l | tr -d ' ')"
        check "/corridors rows" "$([ "$corridor_rows" = "$EXPECTED_CORRIDORS" ] && echo 0 || echo 1)" \
            "$corridor_rows rows (want $EXPECTED_CORRIDORS)"
        corridor_names="$(printf '%s' "$rendered_corridors" | grep -o 'class="corridor-name"' | wc -l | tr -d ' ')"
        check "/corridor names" "$([ "$corridor_names" = "$EXPECTED_CORRIDORS" ] && echo 0 || echo 1)" \
            "$corridor_names readable names (want $EXPECTED_CORRIDORS)"
        corridor_hovers="$(printf '%s' "$rendered_corridors" | grep -o 'title="Configured endpoints:' | wc -l | tr -d ' ')"
        check "/corridor endpoint hovers" "$([ "$corridor_hovers" = "$EXPECTED_CORRIDORS" ] && echo 0 || echo 1)" \
            "$corridor_hovers endpoint hovers (want $EXPECTED_CORRIDORS)"
        corridor_projection="$(printf '%s' "$rendered_corridors" | grep -c 'data-testid="corridor-projection"' || true)"
        corridor_markers="$(printf '%s' "$rendered_corridors" | grep -o 'data-testid="corridor-map-marker"' | wc -l | tr -d ' ')"
        corridor_lines="$(printf '%s' "$rendered_corridors" | grep -o 'data-testid="corridor-map-line"' | wc -l | tr -d ' ')"
        check "/corridor projection" "$([ "$corridor_projection" -gt 0 ] && [ "$corridor_markers" -gt 0 ] && [ "$corridor_lines" = "$EXPECTED_CORRIDORS" ] && echo 0 || echo 1)" \
            "$corridor_markers label anchors, $corridor_lines lines (want $EXPECTED_CORRIDORS)"
        corridor_projection_line="$(printf '%s' "$rendered_corridors" | grep -n -m1 'data-testid="corridor-projection"' | cut -d: -f1 || true)"
        corridor_table_line="$(printf '%s' "$rendered_corridors" | grep -n -m1 'data-testid="corridors"' | cut -d: -f1 || true)"
        check "/corridor map first" "$([ -n "$corridor_projection_line" ] && [ -n "$corridor_table_line" ] && [ "$corridor_projection_line" -lt "$corridor_table_line" ] && echo 0 || echo 1)" \
            "projection line $corridor_projection_line, table line $corridor_table_line"
        corridor_scripts="$(printf '%s' "$rendered_corridors" | grep -ci '<script' || true)"
        check "/corridor no scripts" "$([ "$corridor_scripts" = "0" ] && echo 0 || echo 1)" \
            "$corridor_scripts script tags"

        # 5. Bounded IODA feed has a recorded success and no repeated failures.
        feeds="$(api /api/v1/feeds)"
        if printf '%s' "$feeds" | grep -q '"feed":"ioda-level"'; then
            level_line="$(printf '%s' "$feeds" | tr '}' '\n' | grep '"feed":"ioda-level"' | head -1)"
            if printf '%s' "$level_line" | grep -q '"last_ok_at"'; then
                check "ioda-level feed" 0 "success recorded"
            else
                check "ioda-level feed" 1 "configured, no success yet: $level_line"
            fi
        else
            check "ioda-level feed" 1 "not present in /api/v1/feeds"
        fi

        # 5b. Zero-infrastructure telephony-adjacent evidence feeds. Statuspage
        # rows are provider-reported; RIS rows are IP-routing corroboration.
        # A configured feed with no success yet is still a gap, like ioda-level.
        for feed_name in statuspage ris-live; do
            feed_line="$(printf '%s' "$feeds" | tr '}' '\n' | grep "\"feed\":\"$feed_name\"" | head -1)"
            if [ -n "$feed_line" ] && printf '%s' "$feed_line" | grep -q '"last_ok_at"'; then
                check "$feed_name feed" 0 "success recorded"
            elif [ -n "$feed_line" ]; then
                check "$feed_name feed" 1 "configured, no success yet: $feed_line"
            else
                check "$feed_name feed" 1 "not present in /api/v1/feeds"
            fi
        done

        # 6. Tor vantages came up. Prefer the doctor's live SOCKS probes; fall
        #    back to the board's budget when docker is not reachable from this
        #    shell (e.g. a Git Bash client on Windows). Each pinned exit gets
        #    its own doctor row: tor socks tor-de (optional), etc.
        doctor="$(docker compose --profile tor -f compose.yaml -f compose.fieldtrial.yaml \
            exec -T netweather /netweather doctor 2>/dev/null || true)"
        ok_exits="$(printf '%s' "$doctor" | grep -E 'tor socks tor-[a-z]{2} \(optional\).*ok' || true)"
        if [ -n "$ok_exits" ]; then
            check "tor vantages" 0 "$(printf '%s' "$ok_exits" | head -1 | tr -s ' ') (and $(printf '%s' "$ok_exits" | wc -l | tr -d ' ') pinned exits)"
        elif [ "$(printf '%s' "$(ui_page "$jar" / || true)" | grep -c 'data-testid="tor-budget"')" -gt 0 ]; then
            check "tor vantages" 0 "board shows the Tor budget (doctor unavailable here)"
        else
            check "tor vantages" 1 "no Tor evidence from doctor or the board"
        fi
        rm -f "$jar"

        if [ "$failures" -ne 0 ]; then
            echo "==> $failures check(s) failed"
            return 1
        fi
        echo "==> all checks passed"
        [ "$cycle" -lt "$CYCLES" ] && sleep "$SLEEP"
        cycle=$((cycle + 1))
    done
    return 0
}

pass
