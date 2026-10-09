package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipMiddlewareCompressesLargeResponses(t *testing.T) {
	big := strings.Repeat("x", 4096)
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, big)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip encoding, got %q", rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != big {
		t.Fatal("decompressed body mismatch")
	}
}

func TestGzipMiddlewarePassthroughWhenNotAccepted(t *testing.T) {
	big := strings.Repeat("x", 4096)
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, big)
	}))
	req := httptest.NewRequest("GET", "/", nil) // no Accept-Encoding
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("must not compress without Accept-Encoding: gzip")
	}
	if rec.Body.String() != big {
		t.Fatalf("body should pass through untouched (no truncation/double-write), got %d bytes want %d", rec.Body.Len(), len(big))
	}
}

// TestGzipMiddlewareBelowThresholdStaysUncompressed proves the 1KB minimum size gate.
func TestGzipMiddlewareBelowThresholdStaysUncompressed(t *testing.T) {
	small := strings.Repeat("x", 100)
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, small)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("must not compress a response below the 1KB threshold")
	}
	if rec.Body.String() != small {
		t.Fatalf("body mismatch: got %q", rec.Body.String())
	}
}

// TestGzipMiddlewareExcludesSSERoutes proves the SSE-exclusion short-circuit: /events and
// /public/events stream text/event-stream and must never be buffered/gzipped.
func TestGzipMiddlewareExcludesSSERoutes(t *testing.T) {
	big := strings.Repeat("x", 4096)
	streamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, big)
	})

	for _, path := range []string{"/events", "/public/events"} {
		t.Run(path, func(t *testing.T) {
			h := gzipMiddleware(streamHandler)
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Header().Get("Content-Encoding") == "gzip" {
				t.Fatalf("%s must never be gzipped (SSE stream)", path)
			}
			if rec.Body.String() != big {
				t.Fatalf("%s body should pass through untouched, got %d bytes", path, rec.Body.Len())
			}
		})
	}
}

// TestIsEventsStreamPath is a direct unit test for isEventsStreamPath: every prior test
// only exercised it indirectly through gzipMiddleware's exclusion behavior.
func TestIsEventsStreamPath(t *testing.T) {
	positive := []string{"/events", "/public/events", "/n/abc/events"}
	for _, p := range positive {
		if !isEventsStreamPath(p) {
			t.Errorf("isEventsStreamPath(%q) = false, want true", p)
		}
	}
	negative := []string{"/n/x/eventsfoo", "/eventsx", "/n/x/events/extra"}
	for _, p := range negative {
		if isEventsStreamPath(p) {
			t.Errorf("isEventsStreamPath(%q) = true, want false", p)
		}
	}
}

// countingResponseWriter wraps an httptest.ResponseRecorder to count WriteHeader calls --
// httptest.ResponseRecorder itself silently swallows a second WriteHeader call.
type countingResponseWriter struct {
	rec         *httptest.ResponseRecorder
	headerCalls int
}

func (c *countingResponseWriter) Header() http.Header { return c.rec.Header() }

func (c *countingResponseWriter) Write(p []byte) (int, error) { return c.rec.Write(p) }

func (c *countingResponseWriter) WriteHeader(code int) {
	c.headerCalls++
	c.rec.WriteHeader(code)
}

// TestGzipMiddlewareNoDoubleWriteHeaderOnPreexistingContentEncoding is the regression test
// for the double-WriteHeader bug: a handler that pre-sets its own Content-Encoding.
func TestGzipMiddlewareNoDoubleWriteHeaderOnPreexistingContentEncoding(t *testing.T) {
	big := strings.Repeat("x", 4096)
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulates a handler that already serves a specific encoding (e.g. pre-compressed
		// content) -- startGzip must back off via flushPlain rather than double-encode.
		w.Header().Set("Content-Encoding", "identity")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, big)
	}))

	rec := httptest.NewRecorder()
	cw := &countingResponseWriter{rec: rec}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(cw, req)

	if cw.headerCalls != 1 {
		t.Fatalf("expected exactly 1 WriteHeader call, got %d", cw.headerCalls)
	}
	if rec.Body.String() != big {
		t.Fatalf("body should pass through un-double-encoded, got %d bytes want %d", rec.Body.Len(), len(big))
	}
	if got := rec.Header().Get("Content-Encoding"); got != "identity" {
		t.Fatalf("handler's own Content-Encoding must be preserved, got %q", got)
	}
}
