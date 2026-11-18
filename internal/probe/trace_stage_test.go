package probe_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/probe"
)

// TestHTTPEmitsTheHTTPTraceStage pins LOGGING.md §4: the HTTP instrument reports
// one TRACE line for the exchange with the status and size, and never the body.
func TestHTTPEmitsTheHTTPTraceStage(t *testing.T) {
	t.Parallel()

	secret := "a body the log must not carry"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(secret))
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: logging.LevelTrace}))
	ctx := logging.WithLogger(context.Background(), logger)

	res := probe.NewHTTPWithTransport(srv.Client().Transport).Do(ctx, srv.URL)
	if res.Failed() {
		t.Fatalf("probe failed: %s: %v", res.ErrorClass, res.Err)
	}

	out := buf.String()
	for _, want := range []string{"stage=http", "status=200", "event=probe_stage"} {
		if !strings.Contains(out, want) {
			t.Errorf("trace output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, secret) {
		t.Fatalf("the trace line leaked the body:\n%s", out)
	}
}

func TestHTTPTraceIsSilentAtInfo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil)) // INFO
	ctx := logging.WithLogger(context.Background(), logger)

	if res := probe.NewHTTPWithTransport(srv.Client().Transport).Do(ctx, srv.URL); res.Failed() {
		t.Fatalf("probe failed: %s: %v", res.ErrorClass, res.Err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a normal level emitted trace output:\n%s", buf.String())
	}
}
