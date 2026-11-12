package web

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/plugin/plugintest"
	"github.com/InfoDiveLabs/trinetra/internal/pluginhost"
	"github.com/InfoDiveLabs/trinetra/internal/pluginrt"
)

type execSupervisor struct {
	stateDir, hostSock string
	mu                 sync.Mutex
	procs              map[string]*execRun
}

type execRun struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (s *execSupervisor) Start(rec plugin.Installed, bin, token string) error {
	_ = s.Stop(rec.ID)
	sock := pluginrt.PluginSocketPath(rec.ID)
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	dir := filepath.Join(s.stateDir, "plugins", rec.ID)
	cmd := exec.Command(bin)
	cmd.Env = []string{
		plugin.EnvPluginID + "=" + rec.ID, plugin.EnvHostSocket + "=" + s.hostSock, plugin.EnvPluginSocket + "=" + sock,
		plugin.EnvTokenFile + "=" + filepath.Join(dir, plugin.TokenFileName), plugin.EnvDataDir + "=" + filepath.Join(dir, "data"),
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	r := &execRun{cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(r.done) }()
	s.mu.Lock()
	s.procs[rec.ID] = r
	s.mu.Unlock()
	return nil
}

func (s *execSupervisor) Stop(id string) error {
	s.mu.Lock()
	r := s.procs[id]
	delete(s.procs, id)
	s.mu.Unlock()
	if r != nil {
		_ = r.cmd.Process.Kill()
		<-r.done
	}
	return nil
}

func (s *execSupervisor) Running(id string) (bool, int) {
	s.mu.Lock()
	r := s.procs[id]
	s.mu.Unlock()
	if r == nil {
		return false, 0
	}
	select {
	case <-r.done:
		return false, 0
	default:
		return true, 0
	}
}

func realPluginManager(t *testing.T) plugin.Manager {
	t.Helper()
	root := plugintest.ShortDir(t)
	prevRoot := pluginrt.SocketRoot
	pluginrt.SocketRoot = filepath.Join(root, "p")
	t.Cleanup(func() { pluginrt.SocketRoot = prevRoot })
	stateDir := filepath.Join(root, "state")
	store, err := pluginrt.OpenStore(stateDir, filepath.Join(root, "lib"))
	if err != nil {
		t.Fatal(err)
	}
	var mgr *pluginrt.Manager
	svc, err := pluginhost.NewService(pluginhost.ServiceDeps{
		StateDir:  stateDir,
		Inventory: func() plugin.Inventory { return mgr.Inventory() },
		Submit:    func(int64, map[string]float64) bool { return true },
		Deliver:   func(pluginhost.AlertOut) {},
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Listen(filepath.Join(root, "h")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	sup := &execSupervisor{stateDir: stateDir, hostSock: svc.HostSocket(), procs: map[string]*execRun{}}
	mgr = pluginrt.NewManager(pluginrt.Deps{
		Store: store, Host: func() plugin.HostRegistry { return svc }, Supervisor: sup,
		AllowUnverified: func() bool { return true }, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Now: time.Now,
		Validate: plugin.ValidateValues, Coerce: plugin.CoerceStrings, WithDefaults: plugin.WithDefaults, Redact: plugin.Redact,
		Logf: t.Logf,
	})
	t.Cleanup(mgr.StopAll)
	return plugin.Compose(mgr, svc.UI, nil, nil)
}

func TestIntegrationsEndToEndWithRealPlugin(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a plugin binary")
	}
	pkg := plugintest.Build(t, t.TempDir())
	m := realPluginManager(t)
	d := enrollTestDeps(t)
	d.Plugins = func() plugin.Manager { return m }
	e := &pluginEnv{t: t, d: d, h: newHandler(d), users: newUserStore(d.StateDir), sessions: newSessionStore(d.StateDir)}

	res, err := m.Install(plugin.InstallRequest{Ref: pkg.Path, Actor: "root", Via: "cli"})
	if err != nil || res.Trust != plugin.TrustUnverified {
		t.Fatalf("install %+v %v", res, err)
	}
	body := e.get(RoleAdmin, "/integrations").Body.String()
	if !strings.Contains(body, "E2E Probe") || !strings.Contains(body, "Unverified") {
		t.Fatalf("integrations list missing the installed plugin:\n%s", body)
	}
	body = e.get(RoleAdmin, "/integrations/e2e/enable").Body.String()
	if !strings.Contains(body, "UNVERIFIED") || strings.Count(body, `name="accept"`) != 2 {
		t.Fatalf("consent page:\n%s", body)
	}
	rr := e.post(RoleAdmin, "/integrations/e2e/enable", map[string][]string{
		"accept": {"metrics.publish", "alerts.raise"}, "ack_risk": {"1"},
	})
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "enabled") {
		t.Fatalf("enable: %d %s %s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := m.Get(plugintest.ID)
		if err == nil && st.Running && st.Health.Connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("plugin never connected: %+v %v", st, err)
		}
		time.Sleep(25 * time.Millisecond)
	}

	if code, page := fragment(t, e, RoleViewer, "/p/e2e/main"); code != http.StatusOK || !strings.Contains(page, `hx-trigger="load"`) {
		t.Fatalf("plugin page shell: %d", code)
	}
	code, frag := fragment(t, e, RoleViewer, "/plugins/slot?slot=nav.page&plugin=e2e&slot_id=main")
	if code != http.StatusOK || !strings.Contains(frag, plugintest.WidgetText) || strings.Contains(frag, "plugin-error") {
		t.Fatalf("slot fragment: %d\n%s", code, frag)
	}
	if !strings.Contains(frag, "42") {
		t.Errorf("stat widget missing:\n%s", frag)
	}
	if body := e.get(RoleAdmin, "/integrations/e2e").Body.String(); !strings.Contains(body, "E2E Probe") {
		t.Errorf("detail page missing plugin name")
	}

	if rr := e.post(RoleAdmin, "/integrations/e2e/disable", nil); rr.Code != http.StatusSeeOther {
		t.Fatalf("disable: %d %s", rr.Code, rr.Body.String())
	}
	st, _ := m.Get(plugintest.ID)
	if st.State != plugin.StateInstalled || st.Running {
		t.Fatalf("after disable %+v", st)
	}
	code, frag = fragment(t, e, RoleViewer, "/plugins/slot?slot=nav.page&plugin=e2e&slot_id=main")
	if code == http.StatusOK && strings.Contains(frag, plugintest.WidgetText) {
		t.Fatal("disabled plugin still renders")
	}
	audit := readAuditActions(t, d.StateDir)
	for _, want := range []string{"plugin.consent", "plugin.enable", "plugin.disable"} {
		if !strings.Contains(audit, want) {
			t.Errorf("web audit missing %s", want)
		}
	}
}
