package trinetra

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type fakePluginMgr struct {
	plugin.Manager
	status   plugin.Status
	enabled  []plugin.EnableRequest
	installs []plugin.InstallRequest
	configs  []plugin.SetConfigRequest
	updates  []plugin.UpdateResult
	list     []plugin.Status
	devSess  plugin.DevSession
	detached []string
}

func (f *fakePluginMgr) Get(string) (plugin.Status, error) { return f.status, nil }
func (f *fakePluginMgr) Enable(r plugin.EnableRequest) error {
	f.enabled = append(f.enabled, r)
	return nil
}

func (f *fakePluginMgr) Install(r plugin.InstallRequest) (plugin.InstallResult, error) {
	f.installs = append(f.installs, r)
	return plugin.InstallResult{ID: "demo", Version: "1.0.0", Trust: plugin.TrustVerified,
		Permissions: []plugin.Permission{plugin.PermMetricsRead}}, nil
}

func (f *fakePluginMgr) SetConfig(r plugin.SetConfigRequest) error {
	f.configs = append(f.configs, r)
	return nil
}
func (f *fakePluginMgr) Update(plugin.UpdateRequest) ([]plugin.UpdateResult, error) {
	return f.updates, nil
}
func (f *fakePluginMgr) List() ([]plugin.Status, error) { return f.list, nil }
func (f *fakePluginMgr) DevAttach(plugin.DevRequest) (plugin.DevSession, error) {
	return f.devSess, nil
}

func (f *fakePluginMgr) DevDetach(id string) error {
	f.detached = append(f.detached, id)
	return nil
}

func runPluginCmd(t *testing.T, mgr plugin.Manager, stdin string, sub string, args ...string) (code int, out, errOut string) {
	t.Helper()
	prevW, prevO, prevE, prevIn := pluginWithClient, stdout, stderr, pluginStdin
	var ob, eb bytes.Buffer
	stdout, stderr = &ob, &eb
	pluginStdin = strings.NewReader(stdin)
	pluginWithClient = func(f func(plugin.Manager) error) error { return f(mgr) }
	t.Cleanup(func() { pluginWithClient, stdout, stderr, pluginStdin = prevW, prevO, prevE, prevIn })
	code = cmdPluginRuntime(sub, args)
	return code, ob.String(), eb.String()
}

func statusWith(trust plugin.Trust, perms ...plugin.Permission) plugin.Status {
	return plugin.Status{Installed: plugin.Installed{ID: "demo", Version: "1.0.0", Trust: trust,
		Manifest: plugin.Manifest{ID: "demo", Name: "Demo", Version: "1.0.0", Publisher: "Acme", Permissions: perms}}}
}

func TestPluginCmdEnableVerifiedSinglePrompt(t *testing.T) {
	f := &fakePluginMgr{status: statusWith(plugin.TrustVerified, plugin.PermMetricsRead, plugin.PermNetOutbound)}
	code, out, errOut := runPluginCmd(t, f, "y\n", "enable", "demo")
	if code != 0 {
		t.Fatalf("code %d %s", code, errOut)
	}
	if strings.Count(out, "[y/N]") != 1 || !strings.Contains(out, "  - metrics.read: "+plugin.Describe(plugin.PermMetricsRead)) {
		t.Fatalf("out = %q", out)
	}
	if len(f.enabled) != 1 || len(f.enabled[0].Accepted) != 2 || f.enabled[0].AcknowledgedRisk || f.enabled[0].Via != "cli" {
		t.Fatalf("enabled = %+v", f.enabled)
	}
	f.enabled = nil
	code, _, _ = runPluginCmd(t, f, "n\n", "enable", "demo")
	if code == 0 || len(f.enabled) != 0 {
		t.Fatalf("declined prompt enabled: %d %v", code, f.enabled)
	}
}

func TestPluginCmdEnableUnverifiedPerPermission(t *testing.T) {
	f := &fakePluginMgr{status: statusWith(plugin.TrustUnverified, plugin.PermMetricsRead, plugin.PermNetOutbound)}
	code, out, errOut := runPluginCmd(t, f, "yes\ny\ny\n", "enable", "demo")
	if code != 0 {
		t.Fatalf("code %d %s", code, errOut)
	}
	if !strings.Contains(out, "UNVERIFIED") || !strings.Contains(out, `Type "yes" to continue:`) ||
		!strings.Contains(out, "Allow metrics.read (") || !strings.Contains(out, "Allow net.outbound (") {
		t.Fatalf("out = %q", out)
	}
	if len(f.enabled) != 1 || !f.enabled[0].AcknowledgedRisk || len(f.enabled[0].Accepted) != 2 {
		t.Fatalf("enabled = %+v", f.enabled)
	}
}

func TestPluginCmdEnableUnverifiedAbortOnAnyNo(t *testing.T) {
	for _, in := range []string{"no\n", "yes\ny\nn\n", "yes\nn\n", ""} {
		f := &fakePluginMgr{status: statusWith(plugin.TrustUnverified, plugin.PermMetricsRead, plugin.PermNetOutbound)}
		code, _, errOut := runPluginCmd(t, f, in, "enable", "demo")
		if code == 0 || len(f.enabled) != 0 || !strings.Contains(errOut, "nothing granted") {
			t.Errorf("input %q: code %d enabled %v err %q", in, code, f.enabled, errOut)
		}
	}
}

func TestPluginCmdEnableAcceptFlagMustMatch(t *testing.T) {
	f := &fakePluginMgr{status: statusWith(plugin.TrustVerified, plugin.PermMetricsRead, plugin.PermNetOutbound)}
	if code, _, errOut := runPluginCmd(t, f, "", "enable", "demo", "--accept=metrics.read"); code != 1 || !strings.Contains(errOut, "exactly") {
		t.Fatalf("partial accept: %d %q", code, errOut)
	}
	if code, _, _ := runPluginCmd(t, f, "", "enable", "demo", "--accept=metrics.read,net.outbound,exec"); code != 1 {
		t.Fatalf("extra accept: %d", code)
	}
	if code, _, errOut := runPluginCmd(t, f, "", "enable", "demo", "--accept=net.outbound,metrics.read"); code != 0 {
		t.Fatalf("exact accept: %d %q", code, errOut)
	}
	if len(f.enabled) != 1 {
		t.Fatalf("enabled = %+v", f.enabled)
	}
	u := &fakePluginMgr{status: statusWith(plugin.TrustUnverified, plugin.PermMetricsRead)}
	if code, _, errOut := runPluginCmd(t, u, "", "enable", "demo", "--accept=metrics.read"); code != 1 || !strings.Contains(errOut, "--i-understand-the-risk") {
		t.Fatalf("unverified without risk flag: %d %q", code, errOut)
	}
	if code, _, _ := runPluginCmd(t, u, "", "enable", "demo", "--accept=metrics.read", "--i-understand-the-risk"); code != 0 || !u.enabled[0].AcknowledgedRisk {
		t.Fatalf("unverified with flag: %d %+v", code, u.enabled)
	}
}

func TestPluginCmdEnablePendingUsesStagedPermissions(t *testing.T) {
	st := statusWith(plugin.TrustVerified, plugin.PermMetricsRead)
	st.Pending = &plugin.PendingUpdate{Version: "2.0.0", Trust: plugin.TrustVerified,
		Manifest: plugin.Manifest{Name: "Demo", Publisher: "Acme", Permissions: []plugin.Permission{plugin.PermMetricsRead, plugin.PermAlertsRaise}}}
	f := &fakePluginMgr{status: st}
	code, out, _ := runPluginCmd(t, f, "y\n", "enable", "demo")
	if code != 0 || !strings.Contains(out, "alerts.raise") || !f.enabled[0].Pending || len(f.enabled[0].Accepted) != 2 {
		t.Fatalf("%d %q %+v", code, out, f.enabled)
	}
}

func TestPluginCmdInstallPrintsTrustAndNextStep(t *testing.T) {
	f := &fakePluginMgr{}
	code, out, errOut := runPluginCmd(t, f, "", "install", "demo@1.0.0")
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	for _, want := range []string{"installed demo 1.0.0 (verified)", "metrics.read", "trinetra plugin enable demo"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if f.installs[0].Ref != "demo@1.0.0" || f.installs[0].Via != "cli" {
		t.Fatalf("install req = %+v", f.installs[0])
	}
	runPluginCmd(t, f, "", "install", "./pkg/demo.tar.gz")
	if !filepath.IsAbs(f.installs[1].Ref) {
		t.Fatalf("file ref not absolute: %s", f.installs[1].Ref)
	}
	runPluginCmd(t, f, "", "install", "https://x.example/p.tgz", "--sha256", "abc")
	if f.installs[2].Ref != "https://x.example/p.tgz" || f.installs[2].ExpectSHA256 != "abc" {
		t.Fatalf("url req = %+v", f.installs[2])
	}
}

func TestPluginCmdSetParsesPairs(t *testing.T) {
	f := &fakePluginMgr{}
	code, _, errOut := runPluginCmd(t, f, "s3cret value\n", "set", "demo", "url=http://a/?x=1&y=2", "token=-", "name=")
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	got := f.configs[0]
	if got.ID != "demo" || got.Values["url"] != "http://a/?x=1&y=2" || got.Values["token"] != "s3cret value" || got.Values["name"] != "" {
		t.Fatalf("config = %+v", got)
	}
	if code, _, _ := runPluginCmd(t, f, "", "set", "demo", "bogus"); code != 2 {
		t.Fatalf("bad pair exit %d", code)
	}
	if code, _, _ := runPluginCmd(t, f, "", "set", "demo"); code != 2 {
		t.Fatalf("no pairs exit %d", code)
	}
}

func TestPluginCmdListTable(t *testing.T) {
	f := &fakePluginMgr{list: []plugin.Status{
		{Installed: plugin.Installed{ID: "alpha", Version: "1.0.0", Trust: plugin.TrustVerified, State: plugin.StateEnabled}, UpdateAvailable: "1.1.0"},
		{Installed: plugin.Installed{ID: "beta", Version: "0.2.0", Trust: plugin.TrustUnverified, State: plugin.StateInstalled}},
	}}
	_, out, _ := runPluginCmd(t, f, "", "list")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || strings.Fields(lines[0])[0] != "ID" || strings.Join(strings.Fields(lines[0]), " ") != "ID VERSION TRUST STATE UPDATE" {
		t.Fatalf("out = %q", out)
	}
	if strings.Join(strings.Fields(lines[1]), " ") != "alpha 1.0.0 verified enabled 1.1.0" || strings.Join(strings.Fields(lines[2]), " ") != "beta 0.2.0 unverified installed -" {
		t.Fatalf("rows = %q", lines)
	}
}

func TestPluginCmdUpdateShowsReconsent(t *testing.T) {
	f := &fakePluginMgr{updates: []plugin.UpdateResult{
		{ID: "a", From: "1.0.0", To: "1.1.0"},
		{ID: "b", From: "1.0.0", To: "2.0.0", NeedsConsent: []plugin.Permission{plugin.PermNetOutbound}},
	}}
	code, out, _ := runPluginCmd(t, f, "", "update", "--all")
	if code != 0 || !strings.Contains(out, "a 1.0.0 -> 1.1.0") ||
		!strings.Contains(out, "run: trinetra plugin enable b (new permissions: net.outbound)") {
		t.Fatalf("%d %q", code, out)
	}
	f.updates = []plugin.UpdateResult{{ID: "c", Error: "boom"}}
	if code, _, _ := runPluginCmd(t, f, "", "update", "c"); code != 1 {
		t.Fatalf("failed update exit %d", code)
	}
}

func TestPluginCmdDevPrintsBanner(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "run.sh")
	script := "#!/bin/sh\ntest \"$TRINETRA_PLUGIN_ID\" = devplug && test -r \"$TRINETRA_PLUGIN_TOKEN_FILE\" && test \"$TRINETRA_PLUGIN_DEV\" = 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakePluginMgr{devSess: plugin.DevSession{ID: "devplug", Token: "tok", HostSocket: "/h.sock", PluginSocket: "/p.sock"}}
	code, out, errOut := runPluginCmd(t, f, "", "dev", dir, "--bin", bin)
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	if !strings.Contains(out, "DEV MODE: devplug is unsigned and NOT sandboxed. Do not use on production hosts.") {
		t.Fatalf("out = %q", out)
	}
	if strings.Contains(out, "plugin exited") {
		t.Fatalf("plugin env check failed: %q", out)
	}
	if len(f.detached) != 1 || f.detached[0] != "devplug" {
		t.Fatalf("detached = %v", f.detached)
	}
}

func TestPluginCmdStatusAndDaemonDown(t *testing.T) {
	st := statusWith(plugin.TrustVerified, plugin.PermMetricsRead)
	st.Running, st.Restarts, st.State = true, 2, plugin.StateEnabled
	st.Consents = []plugin.ConsentRecord{{Actor: "root", Via: "cli", Version: "1.0.0", Permissions: st.Manifest.Permissions}}
	st.Health = plugin.Health{Connected: true, Status: plugin.StatusReport{State: "ok"}, RecentLogs: []plugin.LogEntry{{Level: "info", Message: "hello"}}}
	f := &fakePluginMgr{status: st}
	code, out, _ := runPluginCmd(t, f, "", "status", "demo")
	for _, want := range []string{"running:  true (restarts 2)", "health:   ok", "root via cli", "[info] hello"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	prev := pluginWithClient
	t.Cleanup(func() { pluginWithClient = prev })
	pluginWithClient = func(func(plugin.Manager) error) error { return errDaemonDown }
	var eb bytes.Buffer
	prevE := stderr
	stderr = &eb
	t.Cleanup(func() { stderr = prevE })
	if code := cmdPluginRuntime("list", nil); code != 1 || !errors.Is(errDaemonDown, errDaemonDown) || !strings.Contains(eb.String(), "not reachable") {
		t.Fatalf("daemon down: %d %q", code, eb.String())
	}
}
