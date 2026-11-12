package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

type fakeSource struct {
	view     core.DashboardView
	nodes    []core.NodeSummary
	err      error
	mu       sync.Mutex
	fleetHit int
}

func (f *fakeSource) Snapshot() (core.DashboardView, error) { return f.view, f.err }

func (f *fakeSource) FleetNodes() ([]core.NodeSummary, error) {
	f.mu.Lock()
	f.fleetHit++
	f.mu.Unlock()
	return f.nodes, nil
}

type statusLog struct {
	mu   sync.Mutex
	last [2]string
}

func (s *statusLog) report(state, msg string) {
	s.mu.Lock()
	s.last = [2]string{state, msg}
	s.mu.Unlock()
}

func (s *statusLog) get() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[0], s.last[1]
}

func newTestExporter(t *testing.T, src source) (*exporter, *statusLog) {
	t.Helper()
	st := &statusLog{}
	e := newExporter(src, st.report)
	t.Cleanup(e.close)
	return e, st
}

func get(t *testing.T, url, auth string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestMetricsServesExposition(t *testing.T) {
	e, st := newTestExporter(t, &fakeSource{view: testView()})
	e.apply(settings{listen: "127.0.0.1:0"})
	code, body := get(t, "http://"+e.addr()+"/metrics", "")
	if code != 200 || body != goldenHost {
		t.Fatalf("status %d, body mismatch:\n%s", code, body)
	}
	if state, msg := st.get(); state != "ok" || msg != "serving on "+e.addr() {
		t.Fatalf("status = %q %q", state, msg)
	}
}

func TestMetricsContentType(t *testing.T) {
	e, _ := newTestExporter(t, &fakeSource{})
	e.apply(settings{listen: "127.0.0.1:0"})
	resp, err := http.Get("http://" + e.addr() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
}

func TestMetricsRequiresBearerWhenSet(t *testing.T) {
	e, _ := newTestExporter(t, &fakeSource{view: testView()})
	e.apply(settings{listen: "127.0.0.1:0", token: "s3cret"})
	url := "http://" + e.addr() + "/metrics"
	for _, auth := range []string{"", "Bearer wrong", "Bearer s3cre", "Basic s3cret", "s3cret"} {
		if code, _ := get(t, url, auth); code != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d, want 401", auth, code)
		}
	}
	if code, _ := get(t, url, "Bearer s3cret"); code != 200 {
		t.Errorf("valid token: status %d, want 200", code)
	}
}

func TestMetricsOpenWithoutToken(t *testing.T) {
	e, _ := newTestExporter(t, &fakeSource{view: testView()})
	e.apply(settings{listen: "127.0.0.1:0"})
	if code, _ := get(t, "http://"+e.addr()+"/metrics", ""); code != 200 {
		t.Fatalf("status %d, want 200", code)
	}
}

func TestOtherPathsAndMethods(t *testing.T) {
	e, _ := newTestExporter(t, &fakeSource{})
	e.apply(settings{listen: "127.0.0.1:0"})
	if code, _ := get(t, "http://"+e.addr()+"/", ""); code != http.StatusNotFound {
		t.Errorf("/: status %d, want 404", code)
	}
	resp, err := http.Post("http://"+e.addr()+"/metrics", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d, want 405", resp.StatusCode)
	}
}

func TestSnapshotErrorIs503(t *testing.T) {
	e, _ := newTestExporter(t, &fakeSource{err: errors.New("boom")})
	e.apply(settings{listen: "127.0.0.1:0"})
	if code, _ := get(t, "http://"+e.addr()+"/metrics", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
}

func TestFleetOnlyWhenEnabled(t *testing.T) {
	src := &fakeSource{nodes: []core.NodeSummary{{ID: "n1", Name: "web", State: "online"}}}
	e, _ := newTestExporter(t, src)
	e.apply(settings{listen: "127.0.0.1:0"})
	_, body := get(t, "http://"+e.addr()+"/metrics", "")
	if strings.Contains(body, "trinetra_node_up") || src.fleetHit != 0 {
		t.Fatalf("fleet data served or fetched while disabled (hits %d)", src.fleetHit)
	}
	e.apply(settings{listen: "127.0.0.1:0", fleet: true})
	_, body = get(t, "http://"+e.addr()+"/metrics", "")
	if !strings.Contains(body, `trinetra_node_up{node="web",id="n1"} 1`) {
		t.Fatalf("fleet metric missing:\n%s", body)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestListenerRestartsOnConfigChange(t *testing.T) {
	e, st := newTestExporter(t, &fakeSource{view: testView()})
	first, second := freeAddr(t), freeAddr(t)
	e.apply(settings{listen: first})
	if code, _ := get(t, "http://"+first+"/metrics", ""); code != 200 {
		t.Fatalf("first listener status %d", code)
	}
	e.apply(settings{listen: first, token: "t"})
	if e.addr() != first {
		t.Fatalf("listener moved on a token-only change: %s -> %s", first, e.addr())
	}
	e.apply(settings{listen: second, token: "t"})
	if e.addr() != second {
		t.Fatalf("addr = %s, want %s", e.addr(), second)
	}
	if code, _ := get(t, "http://"+second+"/metrics", "Bearer t"); code != 200 {
		t.Fatalf("second listener status %d", code)
	}
	if conn, err := net.DialTimeout("tcp", first, time.Second); err == nil {
		conn.Close()
		t.Fatal("old listener still accepting after restart")
	}
	if state, msg := st.get(); state != "ok" || msg != "serving on "+second {
		t.Fatalf("status = %q %q", state, msg)
	}
}

func TestListenFailureReportsError(t *testing.T) {
	a, _ := newTestExporter(t, &fakeSource{})
	a.apply(settings{listen: "127.0.0.1:0"})
	b, st := newTestExporter(t, &fakeSource{})
	b.apply(settings{listen: a.addr()})
	if state, msg := st.get(); state != "error" || msg == "" {
		t.Fatalf("status = %q %q, want error", state, msg)
	}
	if b.addr() != "" {
		t.Fatalf("addr = %q after failed listen", b.addr())
	}
}

func TestScrapeNotBlockedBySlowStatusReport(t *testing.T) {
	release := make(chan struct{})
	addr := make(chan string, 1)
	e := newExporter(&fakeSource{view: testView()}, func(state, msg string) {
		if a, ok := strings.CutPrefix(msg, "serving on "); ok {
			addr <- a
			<-release
		}
	})
	t.Cleanup(e.close)
	t.Cleanup(func() { close(release) })
	go e.apply(settings{listen: "127.0.0.1:0"})
	url := "http://" + <-addr + "/metrics"
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("scrape during a status report: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("code = %d", resp.StatusCode)
	}
}
