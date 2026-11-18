package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

// TestDebugTTLResolver pins the two documented ways to end a debug session
// (LOGGING.md §5): a relative duration and an absolute RFC 3339 instant. The
// one-hour ceiling is the same one the API enforces.
func TestDebugTTLResolver(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		forDur  time.Duration
		until   string
		want    time.Duration
		wantErr string
	}{
		{name: "duration", forDur: 10 * time.Minute, want: 10 * time.Minute},
		{name: "instant", until: "2026-11-17T12:15:00Z", want: 15 * time.Minute},
		{name: "both", forDur: time.Minute, until: "2026-11-17T12:15:00Z", wantErr: "not both"},
		{name: "neither", wantErr: "is required"},
		{name: "past", until: "2026-11-17T11:00:00Z", wantErr: "in the past"},
		{name: "too far", until: "2026-11-17T14:00:00Z", wantErr: "maximum is 1h"},
		{name: "bad instant", until: "tomorrow", wantErr: "not RFC 3339"},
		{name: "duration over the cap", forDur: 90 * time.Minute, wantErr: "maximum is 1h"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := debugTTL(tc.forDur, tc.until, now)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("debugTTL: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ttl = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDebugCLIEnableUntil proves the flag reaches the API as a ttl, and that
// asking for both forms is refused before any request is made.
func TestDebugCLIEnableUntil(t *testing.T) {
	api, srv := newFakeDebugAPI(t)
	defer srv.Close()
	setupDebugCLI(t, srv, api.token)

	until := time.Now().UTC().Add(15 * time.Minute).Format(time.RFC3339)
	var out strings.Builder
	if err := run([]string{"debug", "enable", "--target", "chase.com", "--until", until}, &out); err != nil {
		t.Fatalf("debug enable --until: %v", err)
	}
	if !strings.Contains(out.String(), "scope dbg-") {
		t.Fatalf("output = %q", out.String())
	}
	api.mu.Lock()
	created := api.created
	api.mu.Unlock()
	if created != 1 {
		t.Fatalf("the API created %d scopes, want 1", created)
	}

	// Both forms together is ambiguous, so it is refused client-side.
	if err := run([]string{
		"debug", "enable", "--target", "chase.com",
		"--for", "10m", "--until", until,
	}, io.Discard); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("both flags error = %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.created != 1 {
		t.Fatalf("a refused request still reached the API: created = %d", api.created)
	}
}
