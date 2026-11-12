package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type fakeServer struct {
	mu       sync.Mutex
	index    []byte
	sig      []byte
	status   int
	requests int
	srv      *httptest.Server
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{status: 200}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		if f.status != 200 {
			http.Error(w, "boom", f.status)
			return
		}
		switch r.URL.Path {
		case "/index/index.json":
			w.Write(f.index)
		case "/index/index.json.sig":
			w.Write(f.sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) set(t *testing.T, ix Index, priv byte) {
	b, sig := marshalSigned(t, ix, testPriv(priv))
	f.mu.Lock()
	f.index, f.sig = b, sig
	f.mu.Unlock()
}

func (f *fakeServer) registry(t *testing.T, now *time.Time, installed map[string]string) *Registry {
	return New(Options{
		Dir:       t.TempDir(),
		BaseURL:   func() string { return f.srv.URL + "/index/" },
		Keys:      keys(),
		HTTP:      f.srv.Client(),
		Now:       func() time.Time { return *now },
		Installed: func() map[string]string { return installed },
	})
}

func TestRefreshVerifiesAndCaches(t *testing.T) {
	f := newFakeServer(t)
	f.set(t, sampleIndex(3), 7)
	now := testNow
	r := f.registry(t, &now, nil)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, err := r.Catalog(false)
	if err != nil || c.Stale || c.Serial != 3 || len(c.Entries) != 1 || c.Entries[0].Latest != "1.1.0" {
		t.Fatalf("catalog = %+v err=%v", c, err)
	}
	r2 := New(Options{Dir: r.o.Dir, Keys: keys(), Now: func() time.Time { return now }})
	c2, _ := r2.Catalog(false)
	if c2.Serial != 3 || c2.Stale || len(c2.Entries) != 1 {
		t.Fatalf("cache not reloaded: %+v", c2)
	}
}

func TestRefreshRejectsRollbackKeepsCache(t *testing.T) {
	f := newFakeServer(t)
	f.set(t, sampleIndex(5), 7)
	now := testNow
	r := f.registry(t, &now, nil)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.set(t, sampleIndex(4), 7)
	if err := r.Refresh(context.Background()); !errors.Is(err, ErrRollback) {
		t.Fatalf("err = %v", err)
	}
	c, _ := r.Catalog(false)
	if c.Serial != 5 || !strings.Contains(c.Error, "older") || len(c.Entries) != 1 {
		t.Fatalf("catalog = %+v", c)
	}
	f.set(t, sampleIndex(6), 7)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, _ := r.Catalog(false); c.Error != "" || c.Serial != 6 {
		t.Fatalf("error not cleared: %+v", c)
	}
}

func TestRefreshFailureKeepsCache(t *testing.T) {
	f := newFakeServer(t)
	f.set(t, sampleIndex(5), 7)
	now := testNow
	r := f.registry(t, &now, nil)
	_ = r.Refresh(context.Background())
	f.mu.Lock()
	f.status = 500
	f.mu.Unlock()
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if c, _ := r.Catalog(false); len(c.Entries) != 1 || c.Error == "" {
		t.Fatalf("catalog = %+v", c)
	}
}

func TestRefreshRefusesPlainHTTP(t *testing.T) {
	r := New(Options{Dir: t.TempDir(), Keys: keys(), BaseURL: func() string { return "http://example.com/index/" }})
	if err := r.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v", err)
	}
}

func TestCatalogStaleAfterExpiryButInstalledUnaffected(t *testing.T) {
	f := newFakeServer(t)
	f.set(t, sampleIndex(3), 7)
	now := testNow
	r := f.registry(t, &now, map[string]string{"uptime": "1.0.0"})
	_ = r.Refresh(context.Background())
	now = testNow.Add(25 * 24 * time.Hour)
	c, _ := r.Catalog(false)
	if !c.Stale || len(c.Entries) != 1 || c.Entries[0].Installed != "1.0.0" {
		t.Fatalf("catalog = %+v", c)
	}
	r2 := New(Options{Dir: r.o.Dir, Keys: keys(), Now: func() time.Time { return now }})
	if c2, _ := r2.Catalog(false); !c2.Stale || len(c2.Entries) != 1 {
		t.Fatalf("expired cache dropped: %+v", c2)
	}
}

func TestResolveRefusedWhenStale(t *testing.T) {
	f := newFakeServer(t)
	f.set(t, sampleIndex(3), 7)
	now := testNow
	r := f.registry(t, &now, nil)
	if _, err := r.Resolve("uptime", "", "linux", "amd64"); !errors.Is(err, plugin.ErrCatalogStale) {
		t.Fatalf("no index: %v", err)
	}
	_ = r.Refresh(context.Background())
	now = testNow.Add(25 * 24 * time.Hour)
	if _, err := r.Resolve("uptime", "", "linux", "amd64"); !errors.Is(err, plugin.ErrCatalogStale) {
		t.Fatalf("expired: %v", err)
	}
}

func TestResolveNoKeys(t *testing.T) {
	r := New(Options{Dir: t.TempDir()})
	if _, err := r.Resolve("uptime", "", "linux", "amd64"); !errors.Is(err, plugin.ErrNoRegistryKeys) {
		t.Fatalf("err = %v", err)
	}
	if err := r.Refresh(context.Background()); !errors.Is(err, plugin.ErrNoRegistryKeys) {
		t.Fatalf("refresh err = %v", err)
	}
}

func TestResolvePicksPlatformPackage(t *testing.T) {
	f := newFakeServer(t)
	f.set(t, sampleIndex(3), 7)
	now := testNow
	r := f.registry(t, &now, nil)
	_ = r.Refresh(context.Background())
	ref, err := r.Resolve("uptime", "", "linux", "arm64")
	if err != nil || ref.Version != "1.1.0" || !strings.HasSuffix(ref.URL, "linux-arm64.tgz") || ref.Size != 12 {
		t.Fatalf("ref = %+v err=%v", ref, err)
	}
	ref, err = r.Resolve("uptime", "1.0.0", "linux", "amd64")
	if err != nil || ref.Version != "1.0.0" {
		t.Fatalf("pinned: %+v %v", ref, err)
	}
	if _, err := r.Resolve("uptime", "1.0.0", "linux", "arm64"); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("missing platform: %v", err)
	}
	if _, err := r.Resolve("nope", "", "linux", "amd64"); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if v, ok := r.Latest("uptime"); !ok || v != "1.1.0" {
		t.Fatalf("Latest = %q %v", v, ok)
	}
}

func TestResolveRevokedVersion(t *testing.T) {
	f := newFakeServer(t)
	ix := sampleIndex(3)
	ix.Revoked = []plugin.Revocation{{ID: "uptime", Versions: []string{"1.1.0"}, Reason: "leaks tokens"}}
	f.set(t, ix, 7)
	now := testNow
	r := f.registry(t, &now, map[string]string{"uptime": "1.1.0"})
	_ = r.Refresh(context.Background())
	if _, err := r.Resolve("uptime", "1.1.0", "linux", "amd64"); !errors.Is(err, plugin.ErrRevoked) {
		t.Fatalf("err = %v", err)
	}
	ref, err := r.Resolve("uptime", "", "linux", "amd64")
	if err != nil || ref.Version != "1.0.0" {
		t.Fatalf("latest should skip revoked: %+v %v", ref, err)
	}
	c, _ := r.Catalog(false)
	if c.Entries[0].Latest != "1.0.0" || c.Entries[0].Revoked != "leaks tokens" || c.Entries[0].UpdateAvailable {
		t.Fatalf("entry = %+v", c.Entries[0])
	}
}

func TestOnRevokeFiresWithList(t *testing.T) {
	f := newFakeServer(t)
	ix := sampleIndex(3)
	ix.Revoked = []plugin.Revocation{{ID: "uptime", Reason: "bad"}}
	f.set(t, ix, 7)
	now := testNow
	r := f.registry(t, &now, nil)
	var got []plugin.Revocation
	r.OnRevoke(func(l []plugin.Revocation) { got = l })
	_ = r.Refresh(context.Background())
	if len(got) != 1 || got[0].ID != "uptime" || got[0].Reason != "bad" {
		t.Fatalf("got = %+v", got)
	}
	if reason, ok := r.Revocation("uptime", "9.9.9"); !ok || reason != "bad" {
		t.Fatalf("whole-plugin revocation: %q %v", reason, ok)
	}
}

func TestCatalogUpdateAvailableFlag(t *testing.T) {
	f := newFakeServer(t)
	f.set(t, sampleIndex(3), 7)
	now := testNow
	inst := map[string]string{"uptime": "1.0.0"}
	r := f.registry(t, &now, inst)
	_ = r.Refresh(context.Background())
	if c, _ := r.Catalog(false); !c.Entries[0].UpdateAvailable || c.Entries[0].Installed != "1.0.0" {
		t.Fatalf("entry = %+v", c.Entries[0])
	}
	inst["uptime"] = "1.1.0"
	if c, _ := r.Catalog(false); c.Entries[0].UpdateAvailable {
		t.Fatalf("up to date flagged: %+v", c.Entries[0])
	}
}

func TestCatalogRefreshFlagFetches(t *testing.T) {
	f := newFakeServer(t)
	f.set(t, sampleIndex(3), 7)
	now := testNow
	r := f.registry(t, &now, nil)
	c, err := r.Catalog(true)
	if err != nil || c.Serial != 3 {
		t.Fatalf("catalog = %+v err=%v", c, err)
	}
}

func TestNoKeysCatalogExplains(t *testing.T) {
	r := New(Options{Dir: t.TempDir()})
	c, err := r.Catalog(false)
	if err != nil || !c.Stale || c.Error != plugin.ErrNoRegistryKeys.Error() || len(c.Entries) != 0 {
		t.Fatalf("catalog = %+v err=%v", c, err)
	}
}

func TestRunSkipsFetchWhenCacheFresh(t *testing.T) {
	f := newFakeServer(t)
	f.set(t, sampleIndex(3), 7)
	now := testNow
	r := f.registry(t, &now, nil)
	_ = r.Refresh(context.Background())
	f.mu.Lock()
	before := f.requests
	f.mu.Unlock()
	r2 := New(Options{Dir: r.o.Dir, Keys: keys(), BaseURL: r.o.BaseURL, HTTP: f.srv.Client(), Now: func() time.Time { return now }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r2.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requests != before {
		t.Fatalf("fresh cache refetched: %d -> %d", before, f.requests)
	}
}

func TestRunRefreshesWhenNoCache(t *testing.T) {
	f := newFakeServer(t)
	f.set(t, sampleIndex(3), 7)
	now := testNow
	r := f.registry(t, &now, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, _ := r.Catalog(false); c.Serial == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if c, _ := r.Catalog(false); c.Serial != 3 {
		t.Fatalf("not refreshed: %+v", c)
	}
}
