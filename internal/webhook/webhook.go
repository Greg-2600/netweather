// Package webhook delivers opt-in alert notifications (ROADMAP "Later").
// It is best-effort: a failed or slow delivery is logged and never blocks
// incident detection or reconciliation. The project is not a paging system;
// this is a convenience for operators who want a push instead of polling
// /api/v1/events.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/Greg-2600/netweather/internal/incidents"
	"github.com/Greg-2600/netweather/internal/store"
)

// Notifier posts incident open/close events to a URL.
type Notifier struct {
	URL     string
	Secret  string
	Client  *http.Client
	Logger  *slog.Logger
}

// New builds a Notifier. A nil or empty URL disables delivery.
func New(url, secret string, logger *slog.Logger) *Notifier {
	if url == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{
		URL:    url,
		Secret: secret,
		Client: &http.Client{Timeout: 10 * time.Second},
		Logger: logger,
	}
}

type payload struct {
	Event     string         `json:"event"`
	Incident  store.Incident `json:"incident"`
	Timestamp string         `json:"timestamp"`
}

func (n *Notifier) send(ctx context.Context, event string, inc store.Incident) {
	if n == nil || n.URL == "" {
		return
	}
	body, err := json.Marshal(payload{
		Event:     event,
		Incident:  inc,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		n.Logger.Warn("webhook marshal failed", "event", "webhook_marshal_failed", "error", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		n.Logger.Warn("webhook request build failed", "event", "webhook_build_failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Secret != "" {
		req.Header.Set("X-Netweather-Secret", n.Secret)
	}
	resp, err := n.Client.Do(req)
	if err != nil {
		n.Logger.Warn("webhook delivery failed", "event", "webhook_failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		n.Logger.Warn("webhook rejected", "event", "webhook_rejected", "status", resp.StatusCode)
	}
}

// WrapSink wraps an incidents.Sink so an incident open fires a webhook.
func WrapSink(sink incidents.Sink, n *Notifier) incidents.Sink {
	if n == nil {
		return sink
	}
	return &sinkWrapper{sink: sink, notifier: n}
}

type sinkWrapper struct {
	sink     incidents.Sink
	notifier *Notifier
}

func (w *sinkWrapper) HasOpenIncident(ctx context.Context, scope, value string) (bool, error) {
	return w.sink.HasOpenIncident(ctx, scope, value)
}

func (w *sinkWrapper) OpenIncident(ctx context.Context, inc store.Incident, targetIDs []string, attrs []store.Attribution) error {
	if err := w.sink.OpenIncident(ctx, inc, targetIDs, attrs); err != nil {
		return err
	}
	w.notifier.send(ctx, "incident_opened", inc)
	return nil
}

// WrapReconciler wraps an incidents.Reconciler so a close fires a webhook.
func WrapReconciler(r incidents.Reconciler, n *Notifier) incidents.Reconciler {
	if n == nil {
		return r
	}
	return &reconcilerWrapper{r: r, notifier: n}
}

type reconcilerWrapper struct {
	r        incidents.Reconciler
	notifier *Notifier
}

func (w *reconcilerWrapper) OpenIncidents(ctx context.Context) ([]store.Incident, error) {
	return w.r.OpenIncidents(ctx)
}

func (w *reconcilerWrapper) CloseIncident(ctx context.Context, id, reason string, at time.Time) error {
	var inc store.Incident
	if list, err := w.r.OpenIncidents(ctx); err == nil {
		for _, c := range list {
			if c.ID == id {
				inc = c
				break
			}
		}
	}
	if err := w.r.CloseIncident(ctx, id, reason, at); err != nil {
		return err
	}
	if inc.ID != "" {
		inc.ClosedAt = at
		w.notifier.send(ctx, "incident_closed", inc)
	}
	return nil
}