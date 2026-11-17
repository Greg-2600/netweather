// Live board updates over SSE. Deliberately small: the server renders the grid,
// the browser only applies deltas.
(() => {
  "use strict";

  const conn = document.querySelector('[data-testid="connection"]');
  const tiles = new Map();

  document.querySelectorAll('[data-testid="tile"]').forEach((el) => {
    tiles.set(el.dataset.target, el);
  });

  function setConnection(state, text) {
    if (!conn) return;
    conn.dataset.state = state;
    conn.textContent = text;
  }

  function recount() {
    const counts = new Map();
    for (const el of tiles.values()) {
      const s = el.dataset.state;
      counts.set(s, (counts.get(s) || 0) + 1);
    }
    document.querySelectorAll("[data-count-state]").forEach((chip) => {
      const n = counts.get(chip.dataset.countState) || 0;
      const b = chip.querySelector("b");
      if (b) b.textContent = String(n);
      chip.hidden = n === 0;
    });
  }

  function applyState(d) {
    const el = tiles.get(d.target);
    if (!el) return false;

    el.dataset.state = d.state;
    el.className = "tile " + d.class;

    const glyph = el.querySelector(".glyph");
    if (glyph) glyph.textContent = d.glyph;

    const state = el.querySelector(".state");
    if (state) state.textContent = d.state_text;

    el.setAttribute(
      "aria-label",
      `${d.name} (${d.target}): ${d.state_text}${d.since ? ", since " + d.since : ""}`,
    );
    return true;
  }

  if (!window.EventSource) {
    setConnection("retrying", "Live updates unsupported");
    return;
  }

  const es = new EventSource("/events");

  es.onopen = () => setConnection("live", "Live");
  es.onerror = () => setConnection("retrying", "Reconnecting…");

  es.addEventListener("state", (ev) => {
    let d;
    try {
      d = JSON.parse(ev.data);
    } catch {
      return;
    }
    if (applyState(d)) recount();
  });

  // The incident pages are small and attribution is stored at open time, so a
  // reload is safer than patching rows and risking a stale reason.
  es.addEventListener("incident", () => {
    if (location.pathname === "/incidents" || location.pathname.startsWith("/incidents/")) {
      location.reload();
    }
  });
})();
