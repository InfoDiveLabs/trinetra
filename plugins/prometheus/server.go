package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

type source interface {
	Snapshot() (core.DashboardView, error)
	FleetNodes() ([]core.NodeSummary, error)
}

type settings struct {
	listen string
	token  string
	fleet  bool
}

type exporter struct {
	src    source
	report func(state, msg string)

	applyMu sync.Mutex
	mu      sync.Mutex
	cur     settings
	srv     *http.Server
	bound   string
	stopped chan struct{}
}

func newExporter(src source, report func(state, msg string)) *exporter {
	return &exporter{src: src, report: report}
}

func (e *exporter) addr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bound
}

func (e *exporter) apply(s settings) {
	e.applyMu.Lock()
	defer e.applyMu.Unlock()
	if state, msg := e.swap(s); state != "" {
		e.report(state, msg)
	}
}

func (e *exporter) swap(s settings) (state, msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.srv != nil && e.cur.listen == s.listen {
		e.cur = s
		return "", ""
	}
	e.stopLocked()
	e.cur = s
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return "error", "listen " + s.listen + ": " + err.Error()
	}
	srv := &http.Server{Handler: e.handler(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			e.report("error", err.Error())
		}
	}()
	e.srv, e.stopped, e.bound = srv, stopped, ln.Addr().String()
	return "ok", "serving on " + e.bound
}

func (e *exporter) stopLocked() {
	if e.srv == nil {
		return
	}
	_ = e.srv.Close()
	<-e.stopped
	e.srv, e.stopped, e.bound = nil, nil, ""
}

func (e *exporter) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopLocked()
}

func (e *exporter) settings() settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cur
}

func (e *exporter) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", e.serveMetrics)
	return mux
}

func authorized(token, header string) bool {
	if token == "" {
		return true
	}
	got, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (e *exporter) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := e.settings()
	if !authorized(s.token, r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="trinetra"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	view, err := e.src.Snapshot()
	if err != nil {
		http.Error(w, "snapshot unavailable", http.StatusServiceUnavailable)
		return
	}
	var nodes []core.NodeSummary
	if s.fleet {
		nodes, _ = e.src.FleetNodes()
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = writeExposition(w, buildFamilies(view, nodes, s.fleet))
}
