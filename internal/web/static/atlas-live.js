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

  const stream = new EventSource("/events");
  stream.onopen = () => {
    pill.dataset.state = "live";
    pill.textContent = "LIVE";
  };
  stream.onerror = () => {
    pill.dataset.state = "retrying";
    pill.textContent = "RECONNECTING";
  };
  for (const type of ["state", "snapshot", "incident", "log"]) {
    stream.addEventListener(type, refresh);
  }
})();
