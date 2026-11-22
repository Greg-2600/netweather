// Map and hub hover cards. The server renders every explanation into data-*
// attributes, so this file only positions and shows one shared card — no
// network calls and no per-tile DOM construction for hundreds of boxes.
(() => {
  "use strict";

  const treemap = document.querySelector('[data-testid="treemap"]');
  const card = document.querySelector('[data-testid="map-tip"]');
  if (!treemap || !card) return;

  const fields = {};
  card.querySelectorAll("[data-tip-field]").forEach((el) => {
    fields[el.dataset.tipField] = el;
  });

  function set(field, value) {
    const el = fields[field];
    if (el) el.textContent = value || "—";
  }

  function fill(tile) {
    const d = tile.dataset;
    set("name", d.tipName);
    set("state", d.tipState);
    set("kind", d.tipKind);
    set("endpoint", d.tipEndpoint);
    set("detail", d.tipDetail);
    set("duration", d.tipDuration);
    set("group", d.tipGroup);
    set("why", d.tipWhy);
    card.dataset.state = (d.tipState || "").toLowerCase().replace(/\s+/g, "_");
  }

  function place(x, y) {
    const pad = 12;
    const gap = 16;
    const w = card.offsetWidth;
    const h = card.offsetHeight;
    let left = x + gap;
    let top = y + gap;
    if (left + w + pad > window.innerWidth) left = x - w - gap;
    if (top + h + pad > window.innerHeight) top = window.innerHeight - h - pad;
    card.style.left = Math.max(pad, left) + "px";
    card.style.top = Math.max(pad, top) + "px";
  }

  function show() {
    card.hidden = false;
    card.setAttribute("aria-hidden", "false");
  }

  function hide() {
    card.hidden = true;
    card.setAttribute("aria-hidden", "true");
  }

  function tileAt(target) {
    return target instanceof Element ? target.closest('[data-testid="map-tile"]') : null;
  }

  treemap.addEventListener("pointermove", (ev) => {
    const tile = tileAt(ev.target);
    if (!tile) {
      hide();
      return;
    }
    fill(tile);
    // The card must be measurable before it can be kept inside the viewport.
    if (card.hidden) show();
    place(ev.clientX, ev.clientY);
  });

  treemap.addEventListener("pointerleave", hide);

  treemap.addEventListener("focusin", (ev) => {
    const tile = tileAt(ev.target);
    if (!tile) return;
    fill(tile);
    show();
    const r = tile.getBoundingClientRect();
    place(r.left + r.width / 2, r.top + r.height / 2);
  });
  treemap.addEventListener("focusout", hide);

  window.addEventListener("scroll", hide, { passive: true });
  window.addEventListener("resize", hide);
  window.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") hide();
  });
})();
