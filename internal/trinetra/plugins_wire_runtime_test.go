package trinetra

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type revokeRegistry struct {
	plugin.Registry
	cb func([]plugin.Revocation)
}

func (r *revokeRegistry) OnRevoke(f func([]plugin.Revocation)) { r.cb = f }

func TestPluginRuntimeWireSetsLifecycleAndInventory(t *testing.T) {
	d := &pluginDeps{stateDir: t.TempDir(), libDir: t.TempDir(), logf: func(string, ...any) {}, now: time.Now}
	wireRuntime(d)
	if d.lifecycle == nil || d.inventory == nil {
		t.Fatal("lifecycle/inventory not set")
	}
	if len(d.starts) != 1 || len(d.stops) != 1 {
		t.Fatalf("starts=%d stops=%d", len(d.starts), len(d.stops))
	}
	l, err := d.lifecycle.List()
	if err != nil || len(l) != 0 {
		t.Fatalf("List = %v %v", l, err)
	}
	if _, err := d.lifecycle.Get("nope"); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("Get = %v", err)
	}
	d.starts[0]()
	d.stops[0]()
}

func TestPluginRuntimeWireRegistersRevocationHook(t *testing.T) {
	reg := &revokeRegistry{}
	d := &pluginDeps{stateDir: t.TempDir(), libDir: t.TempDir(), registry: reg, logf: func(string, ...any) {}}
	wireRuntime(d)
	if len(d.starts) != 2 {
		t.Fatalf("starts = %d", len(d.starts))
	}
	d.starts[0]()
	if reg.cb == nil {
		t.Fatal("OnRevoke not registered by the first start")
	}
	reg.cb([]plugin.Revocation{{ID: "none"}})
}

func TestPluginRuntimeWireLeavesLifecycleNilOnStoreError(t *testing.T) {
	file := t.TempDir() + "/f"
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &pluginDeps{stateDir: file, libDir: t.TempDir(), logf: func(string, ...any) {}}
	wireRuntime(d)
	if d.lifecycle != nil || d.inventory != nil {
		t.Fatal("lifecycle set despite store failure")
	}
}

func TestPluginRuntimeFetchRefusesNon200AndPlainRedirect(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			_, _ = w.Write([]byte("body"))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	prev := http.DefaultTransport
	http.DefaultTransport = ts.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = prev })
	rc, err := pluginFetch(context.Background(), ts.URL+"/ok")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "body" {
		t.Fatalf("body = %q", b)
	}
	if _, err := pluginFetch(context.Background(), ts.URL+"/missing"); err == nil {
		t.Fatal("404 accepted")
	}
}
