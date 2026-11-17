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

    // Update only the text node. Rewriting the whole .state element would
    // delete the glyph span (DESIGN.md 10.7 requires a non-colour marker) and
    // the "your connection" label.
    const stateText = el.querySelector(".state-text");
    if (stateText) stateText.textContent = d.state_text;

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

  // One full state per connection, sent before any delta. The page HTML was
  // rendered before the stream connected, so a change in that gap would
  // otherwise never reach the browser.
  es.addEventListener("snapshot", (ev) => {
    let d;
    try {
      d = JSON.parse(ev.data);
    } catch {
      return;
    }
    if (!d || !Array.isArray(d.targets)) return;
    for (const t of d.targets) applyState(t);
    recount();
  });

  // The incident pages are small and attribution is stored at open time, so a
  // reload is safer than patching rows and risking a stale reason.
  es.addEventListener("incident", () => {
    if (location.pathname === "/incidents" || location.pathname.startsWith("/incidents/")) {
      location.reload();
    }
  });

  // WARN and above arrive on the same stream (LOGGING.md §9). They are advisory
  // and transient, so the board keeps the newest three quietly and lets each
  // expire; the full ring is on /diagnostics.
  const logBox = document.createElement("div");
  logBox.className = "log-notice";
  logBox.dataset.testid = "log-notice";
  logBox.setAttribute("role", "status");
  logBox.setAttribute("aria-live", "polite");
  document.body.appendChild(logBox);

  es.addEventListener("log", (ev) => {
    let d;
    try {
      d = JSON.parse(ev.data);
    } catch {
      return;
    }
    const line = document.createElement("p");
    line.className = "log-line level-" + String(d.level || "").toLowerCase();
    line.dataset.testid = "log-line";
    line.textContent = `${d.time} ${d.level} ${d.message}`;
    logBox.appendChild(line);
    while (logBox.children.length > 3) logBox.firstElementChild.remove();
    setTimeout(() => line.remove(), 20000);
  });
})();
