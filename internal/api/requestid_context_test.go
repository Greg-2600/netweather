package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/logging"
)

// requestIDControl captures the request id the handler put on the context, so a
// runtime action can name the request that caused it (API.md §6.9.1).
type requestIDControl struct {
	id string
}

func (c *requestIDControl) DisableTarget(ctx context.Context, _ string) (bool, error) {
	c.id = logging.RequestIDFrom(ctx)
	return true, nil
}

func TestTargetDisablePassesTheRequestIDToTheRuntime(t *testing.T) {
	t.Parallel()

	control := &requestIDControl{}
	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			TargetControl: control,
			Version:       "test",
			StartedAt:     time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	rec := do(h, http.MethodPost, "/api/v1/targets/svc-chase/disable", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	headerID := rec.Header().Get("X-Request-Id")
	if headerID == "" {
		t.Fatal("no X-Request-Id on the response")
	}
	if control.id != headerID {
		t.Fatalf("runtime saw request id %q, want the response's %q", control.id, headerID)
	}
}
