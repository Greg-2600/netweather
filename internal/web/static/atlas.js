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
