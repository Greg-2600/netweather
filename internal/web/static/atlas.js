// Atlas pin interaction. Every detail string is rendered by the server into
// data-* attributes, so this file only copies text into one panel: no fetches,
// no DOM construction for each marker, and no inline script. Hover and focus
// are handled by CSS; click (or Enter/Space on a focusable corridor line) pins.
(() => {
  "use strict";

  const panel = document.querySelector('[data-testid="atlas-pin"]');
  if (!panel) return;

  const title = panel.querySelector('[data-testid="atlas-pin-title"]');
  const detail = panel.querySelector('[data-testid="atlas-pin-detail"]');
  const link = panel.querySelector('[data-testid="atlas-pin-link"]');
  const unpin = panel.querySelector('[data-testid="atlas-unpin"]');
  if (!title || !detail || !link || !unpin) return;

  const defaultTitle = title.textContent;
  const defaultDetail = detail.textContent;

  // Only same-origin relative links are accepted. A server-rendered href is
  // expected to be a path or a fragment; anything else is dropped rather than
  // becoming a navigation target.
  function safeHref(raw) {
    if (!raw) return "";
    if (raw.startsWith("#") || (/^\/(?!\/)/.test(raw) && !/[\\\r\n]/.test(raw))) return raw;
    return "";
  }

  function clear() {
    panel.dataset.pinned = "false";
    delete panel.dataset.pinnedTitle;
    title.textContent = defaultTitle;
    detail.textContent = defaultDetail;
    link.hidden = true;
    link.removeAttribute("href");
    unpin.hidden = true;
  }

  function pin(source) {
    const d = source.dataset;
    const label = d.atlasKind || "Marker";
    const name = d.atlasTitle || "unknown";
    title.textContent = label + " · " + name;
    detail.textContent = d.atlasDetail || defaultDetail;
    panel.dataset.pinned = "true";
    panel.dataset.pinnedTitle = name;
    const href = safeHref(d.atlasHref);
    if (href) {
      link.setAttribute("href", href);
      link.hidden = false;
    } else {
      link.removeAttribute("href");
      link.hidden = true;
    }
    unpin.hidden = false;
  }

  function sourceAt(target) {
    return target instanceof Element ? target.closest("[data-atlas-kind]") : null;
  }

  document.addEventListener("click", (ev) => {
    const source = sourceAt(ev.target);
    if (!source) return;
    if (!panel.contains(source)) ev.preventDefault();
    pin(source);
  });

  document.addEventListener("keydown", (ev) => {
    if (ev.key !== "Enter" && ev.key !== " ") return;
    const source = sourceAt(ev.target);
    if (!source || !source.matches("svg g")) return;
    ev.preventDefault();
    pin(source);
  });

  unpin.addEventListener("click", () => clear());
})();

// Atlas-wide search and in-page detail panels. Detail HTML is fetched from the
// existing same-origin routes and injected as inert markup; scripts and remote
// references are never executed from the fetched document.
(() => {
  "use strict";

  const input = document.querySelector('[data-testid="atlas-search"]');
  if (input) {
    const filtered = () => Array.from(document.querySelectorAll(
      '[data-testid="atlas-group"], [data-testid="atlas-country"],' +
      '[data-testid="map-tile"], [data-testid="atlas-feed"],' +
      '[data-testid="corridor-row"], [data-testid="atlas-incident"],' +
      '[data-testid="tile"]'
    ));
    const apply = () => {
      const q = input.value.trim().toLowerCase();
      for (const el of filtered()) {
        const match = !q || (el.textContent || "").toLowerCase().includes(q);
        el.dataset.filtered = match ? "visible" : "hidden";
      }
    };
    input.addEventListener("input", apply);
    document.addEventListener("keydown", (ev) => {
      const tag = document.activeElement ? document.activeElement.tagName : "";
      if (ev.key === "/" && !/^(INPUT|TEXTAREA|SELECT)$/.test(tag)) {
        ev.preventDefault();
        input.focus();
      }
    });
  }

  const dialog = document.querySelector('[data-testid="atlas-detail"]');
  const body = dialog && dialog.querySelector('[data-testid="atlas-detail-body"]');
  const close = dialog && dialog.querySelector('[data-testid="atlas-detail-close"]');
  if (!dialog || !body || !close) return;

  const hide = () => {
    if (typeof dialog.close === "function") dialog.close();
    else dialog.removeAttribute("open");
  };
  close.addEventListener("click", hide);
  dialog.addEventListener("click", (ev) => {
    if (ev.target === dialog) hide();
  });

  document.addEventListener("click", async (ev) => {
    const link = ev.target instanceof Element
      ? ev.target.closest('a[href^="/targets/"], a[href^="/incidents/"]')
      : null;
    if (!link || ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey) return;
    const url = new URL(link.getAttribute("href"), location.origin);
    if (url.origin !== location.origin) return;
    ev.preventDefault();
    try {
      const res = await fetch(url.pathname + url.search, { credentials: "same-origin" });
      if (!res.ok) {
        location.assign(url.pathname + url.search);
        return;
      }
      const doc = new DOMParser().parseFromString(await res.text(), "text/html");
      const main = doc.querySelector("main");
      const nodes = main
        ? Array.from(main.childNodes).map((n) => document.importNode(n, true))
        : [document.createTextNode("No detail is available for this item.")];
      body.replaceChildren(...nodes);
      if (typeof dialog.showModal === "function") dialog.showModal();
      else dialog.setAttribute("open", "");
    } catch (err) {
      location.assign(url.pathname + url.search);
    }
  });
})();

// Corridor history is rendered from the server's 24-point RTT shape and
// baseline markers. No extra fetch, no inline script, and no page reload.
(() => {
  "use strict";

  const heights = { "▁": 12.5, "▂": 25, "▃": 37.5, "▄": 50,
    "▅": 62.5, "▆": 75, "▇": 87.5, "█": 100 };

  for (const graph of document.querySelectorAll('[data-testid="atlas-corridor-graph"]')) {
    const spark = graph.dataset.spark || "";
    const band = graph.dataset.band || "";
    if (!spark) continue;

    const bars = document.createElement("div");
    bars.className = "corridor-bars";
    for (const ch of Array.from(spark)) {
      const bar = document.createElement("span");
      bar.className = "corridor-bar";
      bar.style.height = (heights[ch] || 0) + "%";
      bars.appendChild(bar);
    }

    const marks = document.createElement("div");
    marks.className = "corridor-band";
    for (const ch of Array.from(band)) {
      const mark = document.createElement("span");
      mark.textContent = ch;
      if (ch === "▲") mark.className = "above";
      else if (ch === "▼") mark.className = "below";
      marks.appendChild(mark);
    }
    graph.replaceChildren(bars, marks);
  }

  const conn = document.querySelector('[data-testid="connection"]');
  const pill = document.querySelector('[data-testid="atlas-live"]');
  if (conn && pill && typeof MutationObserver === "function") {
    const sync = () => {
      const state = conn.dataset.state || "connecting";
      pill.dataset.state = state;
      pill.textContent = state === "live" ? "LIVE"
        : state === "retrying" ? "RECONNECTING" : "CONNECTING";
    };
    new MutationObserver(sync).observe(conn, {
      attributes: true, childList: true, characterData: true, subtree: true,
    });
    sync();
  }
})();
