// Atlas live pulse. The SSE endpoint is already session-protected and the
// server owns the state meaning; this file only marks the connection and
// refreshes the server-rendered page when a relevant event arrives.
(() => {
  "use strict";

  const pill = document.querySelector('[data-testid="atlas-live"]');
  if (!pill || typeof EventSource !== "function") return;

  let timer = 0;
  const refresh = () => {
    window.clearTimeout(timer);
    timer = window.setTimeout(() => window.location.reload(), 1500);
  };

  const connection = document.querySelector('[data-testid="connection"]');

  const stream = new EventSource("/events");
  stream.onopen = () => {
    pill.dataset.state = "live";
    pill.textContent = "LIVE";
    if (connection) {
      connection.dataset.state = "live";
      connection.textContent = "Live";
    }
  };
  stream.onerror = () => {
    pill.dataset.state = "retrying";
    pill.textContent = "RECONNECTING";
    if (connection) {
      connection.dataset.state = "retrying";
      connection.textContent = "Reconnecting…";
    }
  };
  for (const type of ["state", "snapshot", "incident"]) {
    stream.addEventListener(type, refresh);
  }

  // WARN and above are advisory and transient: keep the newest three without
  // reloading the page, matching the board's log strip.
  const logBox = document.createElement("div");
  logBox.className = "log-notice";
  logBox.dataset.testid = "log-notice";
  logBox.setAttribute("role", "status");
  logBox.setAttribute("aria-live", "polite");
  document.body.appendChild(logBox);
  stream.addEventListener("log", (ev) => {
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
