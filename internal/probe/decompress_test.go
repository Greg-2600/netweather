package probe_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/probe"
)

// TestDecompressionBombIsCapped pins THREAT_MODEL.md's invariant 15: a 10 MB
// decompression bomb is cut at the 32 KB body cap. The transport decompresses
// gzip transparently, so the limit has to be on the bytes read from the
// decoded stream, not on the compressed length.
func TestDecompressionBombIsCapped(t *testing.T) {
	t.Parallel()

	const rawBytes = 10 << 20
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	chunk := bytes.Repeat([]byte("a"), 64<<10)
	for written := 0; written < rawBytes; written += len(chunk) {
		if _, err := zw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Error("the transport did not offer gzip; the test is not exercising decompression")
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(compressed.Bytes())
	}))
	t.Cleanup(srv.Close)

	res := probe.NewHTTPWithTransport(srv.Client().Transport).Do(context.Background(), srv.URL)
	if res.Failed() {
		t.Fatalf("probe failed: %s: %v", res.ErrorClass, res.Err)
	}
	if int64(len(res.Body)) != probe.DefaultMaxBytes {
		t.Fatalf("body = %d bytes, want the %d-byte cap", len(res.Body), probe.DefaultMaxBytes)
	}
}
