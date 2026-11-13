package trinetra

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/plugin/plugintest"
)

func scrape(addr, bearer string) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/metrics", nil)
	if err != nil {
		return 0, "", err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func TestPrometheusPluginEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a plugin binary")
	}
	pkg := plugintest.BuildFrom(t, t.TempDir(), filepath.Join("..", "..", "plugins", "prometheus"))
	e := startE2EDaemon(t, pkg.Keys, config.Default())

	snap, _ := json.Marshal(Snapshot{TS: 1700000000, CPU: 33.5, MemPct: 50, Load1: 1.25, Disks: map[string]float64{"/": 61.5}})
	if err := os.MkdirAll(e.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.stateDir, "status.json"), snap, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := e.mgr.Install(plugin.InstallRequest{Ref: pkg.Path, Actor: "root", Via: "cli"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if err := e.mgr.Enable(plugin.EnableRequest{ID: "prometheus", Actor: "root", Via: "cli", Accepted: pkg.Manifest.Permissions}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := e.mgr.SetConfig(plugin.SetConfigRequest{ID: "prometheus", Values: map[string]string{"listen": addr, "bearer_token": "e2e-token"}, Actor: "root", Via: "cli"}); err != nil {
		t.Fatalf("set config: %v", err)
	}

	var body string
	e2eWait(t, "authenticated scrape", 30*time.Second, func() bool {
		code, b, err := scrape(addr, "e2e-token")
		body = b
		return err == nil && code == http.StatusOK
	})
	for _, want := range []string{
		"trinetra_cpu_usage_percent 33.5\n",
		"trinetra_memory_usage_percent 50\n",
		"trinetra_load1 1.25\n",
		"trinetra_snapshot_timestamp_seconds 1700000000\n",
		`trinetra_disk_usage_percent{mount="/"} 61.5` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q in\n%s", want, body)
		}
	}
	if code, _, err := scrape(addr, ""); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("scrape without token: %d %v", code, err)
	}
	st, err := e.mgr.Get("prometheus")
	if err != nil || !st.Running || st.Health.Status.State != "ok" || !strings.Contains(st.Health.Status.Message, addr) {
		t.Fatalf("status %+v %v", st, err)
	}

	if err := e.mgr.Disable("prometheus", "root", "cli"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	e2eWait(t, "listener closed", 10*time.Second, func() bool {
		_, _, err := scrape(addr, "e2e-token")
		return err != nil
	})
}
