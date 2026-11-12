package trinetra

import (
	"bytes"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type fakeFleetOps struct {
	pushes []plugin.PushRequest
	err    error
}

func (f *fakeFleetOps) Push(r plugin.PushRequest) (plugin.PushResult, error) {
	f.pushes = append(f.pushes, r)
	if f.err != nil {
		return plugin.PushResult{}, f.err
	}
	return plugin.PushResult{Targets: []plugin.NodeState{{Node: "n1", NodeName: "web-1", Phase: "offered", Version: "1.0.0"}}}, nil
}
func (f *fakeFleetOps) NodeStates(string) ([]plugin.NodeState, error) { return nil, nil }
func (f *fakeFleetOps) RelaySlots(string, plugin.Slot) ([]plugin.SlotOffer, error) {
	return nil, nil
}
func (f *fakeFleetOps) RelayRender(plugin.UIRequest) (plugin.RenderResult, error) {
	return plugin.RenderResult{}, nil
}
func (f *fakeFleetOps) RelayAction(plugin.UIActionRequest) (plugin.ActionResult, error) {
	return plugin.ActionResult{}, nil
}

type pushCLI struct {
	ops *fakeFleetOps
	out *bytes.Buffer
	err *bytes.Buffer
}

func setupPushCLI(t *testing.T, trust plugin.Trust, stdin string) *pushCLI {
	t.Helper()
	lc := newFleetFakeLifecycle()
	lc.installed["xp"] = plugin.Status{Installed: plugin.Installed{ID: "xp", Version: "1.0.0", Trust: trust, Manifest: plugin.Manifest{
		ID: "xp", Name: "Example", Publisher: "someone", Permissions: []plugin.Permission{"metrics.read", "metrics.write"}}}}
	ops := &fakeFleetOps{}
	mgr := plugin.Compose(lc, nil, nil, ops)
	prevWith, prevIn, prevOut, prevErr := pluginWithClient, pushStdin, stdout, stderr
	t.Cleanup(func() { pluginWithClient, pushStdin, stdout, stderr = prevWith, prevIn, prevOut, prevErr })
	pluginWithClient = func(f func(plugin.Manager) error) error { return f(mgr) }
	pushStdin = strings.NewReader(stdin)
	c := &pushCLI{ops: ops, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	stdout, stderr = c.out, c.err
	return c
}

func TestPluginPushVerifiedSingleConfirm(t *testing.T) {
	c := setupPushCLI(t, plugin.TrustVerified, "y\n")
	if code := cmdPluginPush([]string{"xp", "--nodes", "web-1,web-2"}); code != 0 {
		t.Fatalf("exit %d: %s", code, c.err)
	}
	if len(c.ops.pushes) != 1 {
		t.Fatalf("pushes = %+v", c.ops.pushes)
	}
	r := c.ops.pushes[0]
	if r.ID != "xp" || len(r.Nodes) != 2 || r.Nodes[1] != "web-2" || r.AcknowledgedRisk || len(r.Accepted) != 0 || r.Via != "cli" || r.Actor == "" {
		t.Fatalf("request = %+v", r)
	}
	out := c.out.String()
	for _, want := range []string{"metrics.read", "metrics.write", "web-1, web-2", "web-1  offered"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "[y/N]"); n != 1 {
		t.Fatalf("prompted %d times, want 1", n)
	}

	c = setupPushCLI(t, plugin.TrustVerified, "n\n")
	if code := cmdPluginPush([]string{"xp", "--tag", "prod"}); code != 1 || len(c.ops.pushes) != 0 {
		t.Fatalf("declined push: exit %d pushes %+v", code, c.ops.pushes)
	}
}

func TestPluginPushUnverifiedShowsNodesAndPerPermission(t *testing.T) {
	c := setupPushCLI(t, plugin.TrustUnverified, "y\ny\ny\n")
	if code := cmdPluginPush([]string{"xp", "--nodes", "web-1", "--tag", "prod"}); code != 0 {
		t.Fatalf("exit %d: %s", code, c.err)
	}
	r := c.ops.pushes[0]
	if !r.AcknowledgedRisk || len(r.Accepted) != 2 || r.Tag != "prod" {
		t.Fatalf("request = %+v", r)
	}
	out := c.out.String()
	for _, want := range []string{"UNVERIFIED", "web-1 and every node tagged prod", "Allow metrics.read", "Allow metrics.write"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "[y/N]"); n != 3 {
		t.Fatalf("prompted %d times, want risk + 2 permissions", n)
	}

	c = setupPushCLI(t, plugin.TrustUnverified, "y\ny\nn\n")
	if code := cmdPluginPush([]string{"xp", "--nodes", "web-1"}); code != 1 || len(c.ops.pushes) != 0 {
		t.Fatalf("declining one permission still pushed: exit %d %+v", code, c.ops.pushes)
	}

	c = setupPushCLI(t, plugin.TrustUnverified, "")
	if code := cmdPluginPush([]string{"xp", "--nodes", "web-1", "--i-understand-the-risk", "--accept=metrics.read,metrics.write"}); code != 0 {
		t.Fatalf("non-interactive exit %d: %s", code, c.err)
	}
	if r := c.ops.pushes[0]; !r.AcknowledgedRisk || len(r.Accepted) != 2 {
		t.Fatalf("request = %+v", r)
	}
}

func TestPluginPushRemoveAndUsage(t *testing.T) {
	c := setupPushCLI(t, plugin.TrustVerified, "y\n")
	if code := cmdPluginPush([]string{"xp", "--nodes", "web-1", "--remove"}); code != 0 || !c.ops.pushes[0].Remove {
		t.Fatalf("remove: exit %d %+v", code, c.ops.pushes)
	}
	for _, args := range [][]string{nil, {"--nodes", "a"}, {"xp"}} {
		c = setupPushCLI(t, plugin.TrustVerified, "")
		if code := cmdPluginPush(args); code != 2 || !strings.Contains(c.err.String(), "usage:") {
			t.Fatalf("%v: exit %d %q", args, code, c.err)
		}
	}
}
