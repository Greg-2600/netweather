package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/logging"
)

// metaCapabilities builds a handler with the given deps and returns the
// capability list /meta reports.
func metaCapabilities(t *testing.T, deps api.Deps) []string {
	t.Helper()

	deps.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	deps.Version = "test"
	deps.Commit = "deadbeef"
	if deps.StartedAt.IsZero() {
		deps.StartedAt = time.Now()
	}

	h, err := api.New(api.Config{Listen: "127.0.0.1:2600", Token: testToken}, deps)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	rec := do(h, http.MethodGet, "/api/v1/meta", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(env.Data, &meta); err != nil {
		t.Fatal(err)
	}
	return meta.Capabilities
}

func hasCapability(caps []string, name string) bool {
	for _, c := range caps {
		if c == name {
			return true
		}
	}
	return false
}

// TestCapabilitiesAreRuntimeTruth pins the API.md §6.2 claim that `capabilities`
// is what is actually wired, not a fixed list. A client feature-detects here,
// so a disabled action must not be advertised.
func TestCapabilitiesAreRuntimeTruth(t *testing.T) {
	t.Parallel()

	// A bare instance still answers health and meta; nothing else is wired.
	bare := metaCapabilities(t, api.Deps{})
	for _, want := range []string{"health", "meta"} {
		if !hasCapability(bare, want) {
			t.Errorf("bare instance is missing %q: %v", want, bare)
		}
	}
	for _, want := range []string{"trace", "target_control", "incidents", "feeds", "export", "debug"} {
		if hasCapability(bare, want) {
			t.Errorf("bare instance advertises %q it does not serve: %v", want, bare)
		}
	}

	// Trace is advertised only with the reader, the resolver, and the opt-in.
	withTrace := metaCapabilities(t, api.Deps{
		Tracer:       &fakeTracer{},
		Resolver:     &fakeResolver{},
		TraceEnabled: true,
	})
	if !hasCapability(withTrace, "trace") {
		t.Errorf("trace is wired but not advertised: %v", withTrace)
	}

	// The socket capability alone is not consent: the reader without the
	// opt-in stays off the list, matching the endpoint's 503.
	noOptIn := metaCapabilities(t, api.Deps{
		Tracer:   &fakeTracer{},
		Resolver: &fakeResolver{},
	})
	if hasCapability(noOptIn, "trace") {
		t.Errorf("trace advertised without the opt-in: %v", noOptIn)
	}

	// A wired ring and controls show up, because their endpoints work.
	withLogs := metaCapabilities(t, api.Deps{
		Ring:          logging.NewRing(50, 0),
		Control:       &fakeControl{},
		TargetControl: &fakeTargetControl{},
	})
	for _, want := range []string{"logs", "control", "target_control"} {
		if !hasCapability(withLogs, want) {
			t.Errorf("wired %q is missing from capabilities: %v", want, withLogs)
		}
	}
}
