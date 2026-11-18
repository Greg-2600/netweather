package logging_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/logging"
)

func TestFromContextFallsBackToDefault(t *testing.T) {
	t.Parallel()

	if got := logging.FromContext(context.Background()); got != slog.Default() {
		t.Fatal("FromContext(background) should return slog.Default()")
	}
}

func TestWithAttachesFieldsToContextLogger(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	cfg := logging.DefaultConfig()
	cfg.Format = "text"

	lg, err := logging.New(cfg, &out)
	if err != nil {
		t.Fatal(err)
	}

	ctx := logging.WithLogger(context.Background(), lg.Logger)
	ctx = logging.With(ctx, "target", "chase.com")
	logging.FromContext(ctx).Info("probe done")

	_ = lg.Close()

	if !strings.Contains(out.String(), "target=chase.com") {
		t.Fatalf("context attribute missing: %s", out.String())
	}
}

func TestWithDoesNotMutateParent(t *testing.T) {
	t.Parallel()

	parent := context.Background()
	_ = logging.With(parent, "a", 1)

	if got := logging.FromContext(parent); got != slog.Default() {
		t.Fatal("With mutated the parent context")
	}
}

func TestNewTraceIDIsHexAndUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := logging.NewTraceID()
		if len(id) != 16 {
			t.Fatalf("trace id %q has length %d, want 16", id, len(id))
		}
		for _, r := range id {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				t.Fatalf("trace id %q is not hex", id)
			}
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate trace id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestRequestIDContextRoundTrip(t *testing.T) {
	t.Parallel()

	if id := logging.RequestIDFrom(context.Background()); id != "" {
		t.Fatalf("background has a request id: %q", id)
	}

	ctx := logging.WithRequestID(context.Background(), "9f2c1a7b4e8d0356")
	if id := logging.RequestIDFrom(ctx); id != "9f2c1a7b4e8d0356" {
		t.Fatalf("request id = %q", id)
	}

	// An empty id is not stored, so a caller cannot erase an outer id by
	// passing an empty value in an inner wrapper.
	ctx = logging.WithRequestID(ctx, "")
	if id := logging.RequestIDFrom(ctx); id != "9f2c1a7b4e8d0356" {
		t.Fatalf("an empty id replaced the outer one: %q", id)
	}
}
