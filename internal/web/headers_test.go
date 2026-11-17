package web_test

import (
	"strings"
	"testing"
)

// TestBoardCarriesTheSecurityHeaders pins THREAT_MODEL.md T12: the browser
// surface sets the same baseline headers as the API, not only on the API.
func TestBoardCarriesTheSecurityHeaders(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"X-Robots-Tag":           "noindex, nofollow",
		"Cache-Control":          "no-store",
	}
	for header, value := range want {
		if got := resp.Header.Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
}

// TestIconKeepsItsCacheControl proves a handler can replace the default: the
// generated icon is immutable for a day, and the shared middleware must not
// turn that into no-store.
func TestIconKeepsItsCacheControl(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/icon/1.svg") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)

	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Fatalf("icon Cache-Control = %q, want the private max-age", cc)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("icon nosniff = %q", got)
	}
}
