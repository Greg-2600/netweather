package api

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
)

// compressionThreshold is the documented cut-off: JSON under 1 KiB is not worth
// the header and CPU, and a small response compressed is usually larger.
const compressionThreshold = 1024

// compressibleType limits compression to bodies where gzip reliably helps and
// the client expects it. SSE is explicitly excluded: compression buffers, and a
// stream that buffers is not a stream.
func compressibleType(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch ct {
	case "application/json", "text/plain":
		return true
	}
	return strings.HasSuffix(ct, "+json")
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		encoding := strings.TrimSpace(strings.Split(part, ";")[0])
		if strings.EqualFold(encoding, "gzip") {
			return true
		}
	}
	return false
}

// withCompression gzips JSON bodies above the threshold. It buffers until the
// threshold is crossed, so a small response keeps its exact bytes and length.
func withCompression(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r) || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Add("Vary", "Accept-Encoding")
		cw := &compressWriter{ResponseWriter: w}
		defer func() { _ = cw.Close() }()
		next.ServeHTTP(cw, r)
	})
}

type compressWriter struct {
	http.ResponseWriter
	status    int
	headerSet bool

	buf      bytes.Buffer
	gz       *gzip.Writer
	disabled bool
	closed   bool
}

func (w *compressWriter) WriteHeader(status int) {
	if w.headerSet {
		return
	}
	w.headerSet = true
	w.status = status

	// Streaming and already-encoded responses pass through untouched.
	if ct := w.Header().Get("Content-Type"); !compressibleType(ct) {
		w.disabled = true
	}
	if w.Header().Get("Content-Encoding") != "" {
		w.disabled = true
	}
	if !w.disabled {
		// The compressed length is unknown until the stream ends.
		w.Header().Del("Content-Length")
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *compressWriter) Write(p []byte) (int, error) {
	if !w.headerSet {
		w.WriteHeader(http.StatusOK)
	}
	if w.disabled || w.gz != nil {
		return w.writeNow(p)
	}

	w.buf.Write(p)
	if w.buf.Len() < compressionThreshold {
		return len(p), nil
	}

	w.Header().Set("Content-Encoding", "gzip")
	w.gz = gzip.NewWriter(w.ResponseWriter)
	if _, err := w.gz.Write(w.buf.Bytes()); err != nil {
		return 0, err
	}
	w.buf.Reset()
	return len(p), nil
}

func (w *compressWriter) writeNow(p []byte) (int, error) {
	if w.gz != nil {
		return w.gz.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// Flush makes the buffered decision before forwarding: a handler that flushes
// an SSE or streaming response must not have its bytes stuck in this buffer.
func (w *compressWriter) Flush() {
	if !w.headerSet {
		w.WriteHeader(http.StatusOK)
	}
	if w.gz == nil && !w.disabled {
		// Flushing before the threshold means this response is a stream or a
		// small body; either way, send what is buffered as identity.
		w.disabled = true
		if w.buf.Len() > 0 {
			_, _ = w.ResponseWriter.Write(w.buf.Bytes())
			w.buf.Reset()
		}
	}
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Close finishes the response. It is safe to call more than once because the
// outermost handler defers it and a handler may have flushed.
func (w *compressWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true

	if w.gz != nil {
		return w.gz.Close()
	}
	if w.disabled || w.buf.Len() == 0 {
		return nil
	}
	if !w.headerSet {
		w.ResponseWriter.WriteHeader(http.StatusOK)
	}
	_, err := w.ResponseWriter.Write(w.buf.Bytes())
	return err
}

func (w *compressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
