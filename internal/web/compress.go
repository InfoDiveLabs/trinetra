package web

import (
	"bufio"
	"compress/gzip"
	"net"
	"net/http"
	"strings"
)

// gzipMinBytes is the minimum response size gzipMiddleware will compress.
const gzipMinBytes = 1024

// isEventsStreamPath reports whether p is a live SSE stream path that must
// never be buffered by gzipMiddleware: the two unprefixed streams
// (/events, /public/events) or a master's node-scoped counterpart
// (/n/{id}/events) -- the exact same request withNodeRouter (node_scope.go)
// re-dispatches internally as a plain /events once it resolves the node scope.
// The node-scoped match is intentionally loose (any /n/.../events path, not a
// validated node id): worst case a malformed /n/.../events path that
// withNodeRouter itself would 404 just skips gzip too, which is harmless --
// only a missed compression opportunity on a path that was never going to
// succeed anyway.
func isEventsStreamPath(p string) bool {
	if p == "/events" || p == "/public/events" {
		return true
	}
	return strings.HasPrefix(p, "/n/") && strings.HasSuffix(p, "/events")
}

// gzipMiddleware compresses responses with gzip when the client advertises support
// (Accept-Encoding: gzip) and the body turns out to exceed gzipMinBytes.
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

// gzipResponseWriter buffers a handler's output up to gzipMinBytes before deciding whether
// to compress: only once the buffer crosses the threshold does it commit to gzip.
type gzipResponseWriter struct {
	http.ResponseWriter
	buf        []byte
	gz         *gzip.Writer
	wroteHdr   bool
	statusCode int
	passthru   bool
	// finalized guards flushPlain against running twice for the SAME request.
	finalized bool
}

// WriteHeader defers the actual header write (writeStatus) until Write/Close knows whether
// the response will be compressed.
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

// startGzip commits the response to gzip: it sets Content-Encoding, drops any pre-set
// Content-Length (the compressed length is different and not known up front).
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

// flushPlain commits the response to going out uncompressed: the buffered bytes so far (if
// any) plus the deferred status.
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

// writeStatus emits the deferred WriteHeader call, if the handler ever made one.
func (g *gzipResponseWriter) writeStatus() {
	if g.wroteHdr {
		g.ResponseWriter.WriteHeader(g.statusCode)
	}
}

// Close finalizes the response: a gzip stream already in progress just gets its trailer
// flushed, otherwise.
func (g *gzipResponseWriter) Close() {
	if g.gz != nil {
		_ = g.gz.Close()
		return
	}
	g.flushPlain()
}

// Hijack preserves websocket/SSE upgrade paths for any handler wrapped by gzipMiddleware
// that needs to take over the raw connection -- without this.
func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := g.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}
