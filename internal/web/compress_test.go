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

// TestGzipMiddlewareBelowThresholdStaysUncompressed proves the 1KB minimum
// size gate: a response under gzipMinBytes must be served as-is even when
// the client advertises Accept-Encoding: gzip, so tiny responses (most JSON
// API replies) don't pay the CPU/framing cost of gzip for no bandwidth win.
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

// TestGzipMiddlewareExcludesSSERoutes proves the SSE-exclusion short-circuit:
// /events and /public/events stream text/event-stream and must never be
// buffered/gzipped, even when the client sends Accept-Encoding: gzip and the
// streamed body exceeds the compression threshold -- buffering would defeat
// the whole point of a live push stream (the browser would see nothing until
// the handler returns).
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

// TestIsEventsStreamPath is a direct unit test for isEventsStreamPath: every
// prior test only exercised it indirectly through gzipMiddleware's exclusion
// behavior (TestGzipMiddlewareExcludesSSERoutes above, and node_scope_test.go's
// node-routing tests) -- this pins the predicate itself, positive and negative,
// independent of the middleware wrapping it.
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

// countingResponseWriter wraps an httptest.ResponseRecorder to count
// WriteHeader calls -- httptest.ResponseRecorder itself silently swallows a
// second WriteHeader call (only its FIRST call's code/headers stick), which
// is exactly why the double-WriteHeader bug below wasn't caught by the
// original test suite: asserting against a bare ResponseRecorder can't see
// it. This wrapper is deliberately NOT embedding ResponseRecorder -- it
// implements Header/Write/WriteHeader explicitly so WriteHeader's counter
// increment can't be bypassed by promoted-method resolution.
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

// TestGzipMiddlewareNoDoubleWriteHeaderOnPreexistingContentEncoding is the
// regression test for the double-WriteHeader bug: a handler that pre-sets
// its own Content-Encoding (so gzipMiddleware must back off, per
// startGzip's "caller already set an encoding" guard) and then writes a
// body >= gzipMinBytes must still result in exactly ONE WriteHeader call on
// the underlying ResponseWriter -- not two. Before the fix, startGzip's
// flushPlain call (which writes the deferred status) left g.gz nil, so
// Close's `g.gz != nil` check couldn't tell "already finalized via
// flushPlain" apart from "never finalized" and called flushPlain a second
// time, calling WriteHeader again ("http: superfluous response.WriteHeader
// call" against a real ResponseWriter; httptest.ResponseRecorder hides this,
// hence the custom counting wrapper here instead of a bare Recorder).
func TestGzipMiddlewareNoDoubleWriteHeaderOnPreexistingContentEncoding(t *testing.T) {
	big := strings.Repeat("x", 4096)
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulates a handler that already serves a specific encoding (e.g.
		// pre-compressed content) -- startGzip must back off via flushPlain
		// rather than double-encode, exactly the path that triggered the bug.
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
