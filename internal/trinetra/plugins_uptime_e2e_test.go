package trinetra

import (
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/plugin/plugintest"
)

func TestUptimePluginEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the uptime plugin binary")
	}
	_, self, _, _ := runtime.Caller(0)
	src := filepath.Join(filepath.Dir(self), "..", "..", "plugins", "uptime")
	pkg := plugintest.BuildFrom(t, t.TempDir(), src)

	live, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	go func() {
		for {
			c, err := live.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()

	cfg := config.Default()
	if err := cfg.Set("plugins.allow_unverified", "false"); err != nil {
		t.Fatal(err)
	}
	e := startE2EDaemon(t, pkg.Key, cfg)
	if _, err := e.mgr.Install(plugin.InstallRequest{Ref: pkg.Path, Actor: "root", Via: "cli"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := e.mgr.SetConfig(plugin.SetConfigRequest{ID: "uptime", Actor: "root", Via: "cli", Values: map[string]string{
		"checks":          "tcp " + live.Addr().String() + " name=live\ntcp " + deadAddr + " name=dead",
		"down_after_runs": "1",
	}}); err != nil {
		t.Fatalf("set config: %v", err)
	}
	if err := e.mgr.Enable(plugin.EnableRequest{ID: "uptime", Actor: "root", Via: "cli", Accepted: pkg.Manifest.Permissions}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	e2eWait(t, "live check published up", 30*time.Second, func() bool {
		v, ok := e.sink.metric("plugin.uptime.check.live.up")
		return ok && v == 1
	})
	e2eWait(t, "dead check published down", 30*time.Second, func() bool {
		v, ok := e.sink.metric("plugin.uptime.check.dead.up")
		return ok && v == 0
	})
	e2eWait(t, "down alert delivered", 30*time.Second, func() bool { return e.sink.count("plugin.uptime.check.dead.down", "fire") == 1 })
	if n := e.sink.count("plugin.uptime.check.live.down", "fire"); n != 0 {
		t.Fatalf("live check alerted %d times", n)
	}

	c := e.controlClient(t)
	rr, err := c.Plugins().RenderSlot(plugin.UIRequest{PluginID: "uptime", Request: plugin.SlotRequest{
		Slot: plugin.SlotNavPage, SlotID: "checks", User: plugin.UserContext{Name: "alice", Role: plugin.RoleViewer},
	}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(rr.Widgets) != 2 || rr.Widgets[0].Status == nil || len(rr.Widgets[0].Status.Items) != 2 {
		t.Fatalf("widgets %+v", rr.Widgets)
	}
	states := map[string]string{}
	for _, it := range rr.Widgets[0].Status.Items {
		states[it.Name] = it.State
	}
	if states["live"] != "up" || states["dead"] != "down" {
		t.Fatalf("states %v", states)
	}
}
