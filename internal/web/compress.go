package web

import (
	"bufio"
	"compress/gzip"
	"net"
	"net/http"
	"strings"
)

// gzipMinBytes is the minimum response size gzipMiddleware will compress.
// Below this, gzip's per-response framing/CPU cost isn't worth paying --
// most JSON API replies (e.g. /api/downtime, small config responses) never
// cross it, only the larger history/series payloads this task targets do.
const gzipMinBytes = 1024

// isEventsStreamPath reports whether p is a live SSE stream path that must
// never be buffered by gzipMiddleware: the two unprefixed streams
// (/events, /public/events) or a master's node-scoped counterpart
// (/n/{id}/events, Task 4/fleet-web-a) -- the exact same request
// withNodeRouter (node_scope.go) re-dispatches internally as a plain
// /events once it resolves the node scope. The node-scoped match is
// intentionally loose (any /n/.../events path, not a validated node id):
// worst case a malformed /n/.../events path that withNodeRouter itself
// would 404 just skips gzip too, which is harmless -- never a correctness
// or security concern, only a missed compression opportunity on a path
// that was never going to succeed anyway.
func isEventsStreamPath(p string) bool {
	if p == "/events" || p == "/public/events" {
		return true
	}
	return strings.HasPrefix(p, "/n/") && strings.HasSuffix(p, "/events")
}

// gzipMiddleware compresses responses with gzip when the client advertises
// support (Accept-Encoding: gzip) and the body turns out to exceed
// gzipMinBytes, so the larger history/series JSON payloads (the whole point
// of this task -- a congested uplink pays less for them) travel compressed
// while small responses stay as-is.
//
// /events and /public/events are excluded unconditionally: both stream
// text/event-stream (see sse.go's eventsHandler/publicEventsHandler), and
// gzipResponseWriter's buffer-then-decide strategy below would hold every
// frame until either the 1KB threshold or the connection closes -- exactly
// backwards for a live push stream, where the browser needs each frame the
// moment it's written, not once several KB have accumulated. As of Task 4
// (fleet-web-a), a master's node-scoped SSE stream (/n/{id}/events,
// node_scope.go's withNodeRouter) is excluded too, via isEventsStreamPath --
// this middleware runs OUTSIDE withNodeRouter (routes.go), so it always
// sees the request's ORIGINAL, still-/n/{id}/-prefixed path, never the
// prefix-stripped /events withNodeRouter re-dispatches internally.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isEventsStreamPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.Close()
		next.ServeHTTP(gw, r)
	})
}

// gzipResponseWriter buffers a handler's output up to gzipMinBytes before
// deciding whether to compress: only once the buffer crosses the threshold
// does it commit to gzip (startGzip), so a response that never reaches 1KB
// is flushed uncompressed instead (flushPlain, called from Close for the
// case the handler wrote fewer than gzipMinBytes total).
type gzipResponseWriter struct {
	http.ResponseWriter
	buf        []byte
	gz         *gzip.Writer
	wroteHdr   bool
	statusCode int
	passthru   bool
	// finalized guards flushPlain against running twice for the SAME
	// request. It's only needed for the passthrough terminal state: the gz
	// terminal state is already idempotent via Close's own `g.gz != nil`
	// check. Without this, a handler that pre-sets its own Content-Encoding
	// and then writes >= gzipMinBytes hits flushPlain once from startGzip
	// (backing off from double-encoding) and a SECOND time from Close (which
	// only ever checked g.gz, still nil on this path) -- two WriteHeader
	// calls on the underlying ResponseWriter for one request. See
	// flushPlain/Close.
	finalized bool
}

// WriteHeader defers the actual header write (writeStatus) until Write/Close
// knows whether the response will be compressed -- Content-Encoding and
// Content-Length must be settled before headers go out, and that isn't known
// until either the buffer crosses gzipMinBytes or the handler finishes below
// it.
func (g *gzipResponseWriter) WriteHeader(code int) {
	g.statusCode = code
	g.wroteHdr = true
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if g.gz != nil {
		return g.gz.Write(p)
	}
	if g.passthru {
		return g.ResponseWriter.Write(p)
	}
	g.buf = append(g.buf, p...)
	if len(g.buf) >= gzipMinBytes {
		g.startGzip()
		return len(p), nil
	}
	return len(p), nil
}

// startGzip commits the response to gzip: it sets Content-Encoding, drops
// any pre-set Content-Length (the compressed length is different and not
// known up front), writes the deferred status, and starts streaming the
// buffered-so-far bytes plus everything after through a gzip.Writer.
//
// If the handler already set its own Content-Encoding (e.g. a handler that
// serves pre-compressed content), this backs off entirely via flushPlain
// instead of double-encoding.
func (g *gzipResponseWriter) startGzip() {
	h := g.ResponseWriter.Header()
	if h.Get("Content-Encoding") != "" {
		g.flushPlain()
		return
	}
	h.Set("Content-Encoding", "gzip")
	h.Del("Content-Length")
	g.writeStatus()
	g.gz = gzip.NewWriter(g.ResponseWriter)
	if len(g.buf) > 0 {
		_, _ = g.gz.Write(g.buf)
		g.buf = nil
	}
}

// flushPlain commits the response to going out uncompressed: the buffered
// bytes so far (if any) plus the deferred status. Reached either because the
// handler's total output never crossed gzipMinBytes (via Close) or because
// startGzip found an existing Content-Encoding it must not override (also,
// in that second case, potentially again from Close -- guarded below).
//
// Idempotent: the pre-existing-Content-Encoding path finalizes here from
// startGzip while g.gz stays nil, so Close's own g.gz != nil check can't
// tell this state apart from "never finalized" and would call flushPlain a
// second time -- which would call ResponseWriter.WriteHeader a second time
// too ("http: superfluous response.WriteHeader call"). The finalized guard
// makes any call after the first a no-op.
func (g *gzipResponseWriter) flushPlain() {
	if g.finalized {
		return
	}
	g.finalized = true
	g.passthru = true
	g.writeStatus()
	if len(g.buf) > 0 {
		_, _ = g.ResponseWriter.Write(g.buf)
		g.buf = nil
	}
}

// writeStatus emits the deferred WriteHeader call, if the handler ever made
// one -- a handler that never calls WriteHeader relies on the underlying
// ResponseWriter's own implicit-200-on-first-Write behavior, which still
// works here since flushPlain/startGzip only write bytes after this returns.
func (g *gzipResponseWriter) writeStatus() {
	if g.wroteHdr {
		g.ResponseWriter.WriteHeader(g.statusCode)
	}
}

// Close finalizes the response: a gzip stream already in progress just gets
// its trailer flushed, otherwise (the handler's total output never crossed
// gzipMinBytes) the buffered bytes are emitted uncompressed via flushPlain.
func (g *gzipResponseWriter) Close() {
	if g.gz != nil {
		_ = g.gz.Close()
		return
	}
	g.flushPlain()
}

// Hijack preserves websocket/SSE upgrade paths for any handler wrapped by
// gzipMiddleware that needs to take over the raw connection -- without this,
// wrapping gzipResponseWriter around such a handler's ResponseWriter would
// silently break http.Hijacker type assertions the handler might make.
// (routes.go's SSE handlers don't hit this path since gzipMiddleware
// excludes every events-stream path (isEventsStreamPath) outright, but this
// keeps the writer correct for any other upgrade-style handler that might
// be added later.)
func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := g.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}
