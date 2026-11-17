package api //nolint:testpackage // white-box tests cover writer internals

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveCompressed(t *testing.T, contentType, body string, accept string) *httptest.ResponseRecorder {
	t.Helper()

	h := withCompression(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = io.WriteString(w, body)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
	if accept != "" {
		req.Header.Set("Accept-Encoding", accept)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCompressionLeavesSmallBodiesAlone(t *testing.T) {
	t.Parallel()

	rec := serveCompressed(t, "application/json", `{"ok":true}`, "gzip")
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("small body was compressed: %q", rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("body = %q", rec.Body)
	}
	if rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("Vary = %q", rec.Header().Get("Vary"))
	}
}

func TestCompressionGzipsLargeJSON(t *testing.T) {
	t.Parallel()

	body := `{"data":"` + strings.Repeat("x", 4096) + `"}`
	rec := serveCompressed(t, "application/json", body, "gzip")

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", rec.Header().Get("Content-Encoding"))
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Fatal("a compressed response kept Content-Length")
	}

	reader, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if string(got) != body {
		t.Fatalf("round-trip mismatch: %d vs %d bytes", len(got), len(body))
	}
}

func TestCompressionHonoursAcceptEncoding(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("a", 4096)

	for _, accept := range []string{"", "identity", "br"} {
		rec := serveCompressed(t, "application/json", body, accept)
		if rec.Header().Get("Content-Encoding") != "" {
			t.Errorf("Accept-Encoding %q compressed the body", accept)
		}
		if rec.Body.Len() != len(body) {
			t.Errorf("Accept-Encoding %q changed the body length", accept)
		}
	}

	rec := serveCompressed(t, "application/json", body, "br, gzip;q=0.8")
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Error("gzip was not selected from a mixed Accept-Encoding")
	}
}

func TestCompressionNeverTouchesEventStreams(t *testing.T) {
	t.Parallel()

	h := withCompression(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, "event: state\ndata: "+strings.Repeat("x", 4096)+"\n\n")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("SSE was compressed: %q", rec.Header().Get("Content-Encoding"))
	}
	if !strings.HasPrefix(rec.Body.String(), "event: state") {
		t.Fatalf("SSE body was altered: %q", rec.Body.String()[:20])
	}
}

func TestCompressionFlushesStreamingBodies(t *testing.T) {
	t.Parallel()

	h := withCompression(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "retry: 1000\n\n")
		w.(http.Flusher).Flush()
	}))

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("a flushed stream was compressed")
	}
	if rec.Body.String() != "retry: 1000\n\n" {
		t.Fatalf("flushed body = %q", rec.Body.String())
	}
}

func TestCompressionSkipsHeadAndEncodedResponses(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("b", 4096)

	h := withCompression(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	req := httptest.NewRequest(http.MethodHead, "/api/v1/thing", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" {
		t.Error("HEAD response was compressed")
	}

	pre := withCompression(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		_, _ = io.WriteString(w, body)
	}))
	req = httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	pre.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("an already-encoded response changed encoding to %q", got)
	}
}

func TestCompressibleType(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"application/json":                true,
		"application/json; charset=utf-8": true,
		"application/problem+json":        true,
		"text/plain":                      true,
		"text/event-stream":               false,
		"text/csv":                        false,
		"application/octet-stream":        false,
		"":                                false,
	}
	for contentType, want := range tests {
		if got := compressibleType(contentType); got != want {
			t.Errorf("compressibleType(%q) = %v, want %v", contentType, got, want)
		}
	}
}
