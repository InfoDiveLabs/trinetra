package trinetra

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type pluginPushedFrame struct {
	node string
	f    fleet.Frame
}

type pusherFixture struct {
	p      *pluginPusher
	lc     *fleetFakeLifecycle
	mu     sync.Mutex
	frames []pluginPushedFrame
	online map[string]bool
	audit  string
	pkg    string
	data   []byte
}

func newPusherFixture(t *testing.T, trust plugin.Trust) *pusherFixture {
	t.Helper()
	fx := &pusherFixture{lc: newFleetFakeLifecycle(), online: map[string]bool{"n1": true, "n2": true, "n3": true}}
	fx.data = []byte("package-bytes")
	fx.pkg = filepath.Join(t.TempDir(), "x.tar.gz")
	if err := os.WriteFile(fx.pkg, fx.data, 0o600); err != nil {
		t.Fatal(err)
	}
	fx.lc.installed["xp"] = plugin.Status{Installed: plugin.Installed{
		ID: "xp", Version: "1.0.0", Trust: trust, State: plugin.StateEnabled, PackageSHA256: shaOf(fx.data),
		Manifest: plugin.Manifest{ID: "xp", Permissions: []plugin.Permission{"metrics.read", "metrics.write"}},
	}}
	fx.audit = filepath.Join(t.TempDir(), "audit.jsonl")
	nodes := func() []fleet.Node {
		return []fleet.Node{
			{ID: "n1", Name: "web-1", Tags: []string{"prod"}},
			{ID: "n2", Name: "web-2", Tags: []string{"prod", "eu"}},
			{ID: "n3", Name: "db-1", Tags: []string{"db"}},
			{ID: "n4", Name: "old", Tags: []string{"prod"}, Revoked: true},
		}
	}
	fx.p = newPluginPusher(func() plugin.Lifecycle { return fx.lc }, nodes,
		func(id string, f fleet.Frame) bool {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			if !fx.online[id] {
				return false
			}
			fx.frames = append(fx.frames, pluginPushedFrame{id, f})
			return true
		},
		func(id string) bool { return fx.online[id] },
		newAuditLog(fx.audit),
		func(id string) map[string]string { return map[string]string{"interval": "30"} },
		func() time.Time { return time.Unix(1_700_000_000, 0) })
	fx.lc.pkgs["xp"] = plugin.PackageInfo{ID: "xp", Version: "1.0.0", Path: fx.pkg, SHA256: shaOf(fx.data), Size: int64(len(fx.data)), Trust: trust}
	return fx
}

func (fx *pusherFixture) framesFor(node, typ string) []fleet.Frame {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	var out []fleet.Frame
	for _, pf := range fx.frames {
		if pf.node == node && pf.f.Type == typ {
			out = append(out, pf.f)
		}
	}
	return out
}

func TestPushResolvesNodesAndTag(t *testing.T) {
	fx := newPusherFixture(t, plugin.TrustVerified)
	res, err := fx.p.Push(plugin.PushRequest{ID: "xp", Nodes: []string{"web-1", "self"}, Tag: "db", Actor: "root", Via: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Targets) != 2 || res.Targets[0].NodeName != "db-1" || res.Targets[1].NodeName != "web-1" {
		t.Fatalf("targets = %+v", res.Targets)
	}
	if len(fx.framesFor("n1", fleet.FramePluginOffer)) != 1 || len(fx.framesFor("n3", fleet.FramePluginOffer)) != 1 || len(fx.framesFor("n2", fleet.FramePluginOffer)) != 0 {
		t.Fatalf("offers misrouted: %+v", fx.frames)
	}
	var o fleet.PluginOffer
	json.Unmarshal(fx.framesFor("n1", fleet.FramePluginOffer)[0].Data, &o)
	if o.Config["interval"] != "30" || o.SHA256 != shaOf(fx.data) {
		t.Fatalf("offer = %+v", o)
	}

	if _, err := fx.p.Push(plugin.PushRequest{ID: "xp", Nodes: []string{"nope"}}); err == nil {
		t.Fatal("unknown node accepted")
	}
	if _, err := fx.p.Push(plugin.PushRequest{ID: "xp", Nodes: []string{"self"}}); err == nil {
		t.Fatal("self-only push accepted")
	}
	if _, err := fx.p.Push(plugin.PushRequest{ID: "xp"}); err == nil {
		t.Fatal("empty target set accepted")
	}
	if _, err := fx.p.Push(plugin.PushRequest{ID: "xp", Nodes: []string{"old"}}); err == nil {
		t.Fatal("revoked node accepted")
	}
	if _, err := fx.p.Push(plugin.PushRequest{ID: "missing", Nodes: []string{"web-1"}}); err == nil {
		t.Fatal("plugin not installed on master accepted")
	}
}

func TestPushUnverifiedNeedsRiskAckAndAllPermissions(t *testing.T) {
	fx := newPusherFixture(t, plugin.TrustUnverified)
	all := []plugin.Permission{"metrics.read", "metrics.write"}
	cases := []plugin.PushRequest{
		{ID: "xp", Nodes: []string{"web-1"}, Accepted: all},
		{ID: "xp", Nodes: []string{"web-1"}, AcknowledgedRisk: true, Accepted: all[:1]},
		{ID: "xp", Nodes: []string{"web-1"}, AcknowledgedRisk: true},
	}
	for i, c := range cases {
		if _, err := fx.p.Push(c); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	if len(fx.frames) != 0 {
		t.Fatal("frames sent for refused pushes")
	}
	if _, err := fx.p.Push(plugin.PushRequest{ID: "xp", Nodes: []string{"web-1"}, AcknowledgedRisk: true, Accepted: []plugin.Permission{"metrics.write", "metrics.read"}}); err != nil {
		t.Fatal(err)
	}
}

func TestPushRecordsConsentWithNodes(t *testing.T) {
	fx := newPusherFixture(t, plugin.TrustVerified)
	if _, err := fx.p.Push(plugin.PushRequest{ID: "xp", Nodes: []string{"web-1", "web-2"}, Actor: "alice", Via: "web"}); err != nil {
		t.Fatal(err)
	}
	var o fleet.PluginOffer
	json.Unmarshal(fx.framesFor("n2", fleet.FramePluginOffer)[0].Data, &o)
	c := o.Consent
	if c.Actor != "alice" || c.Via != "web" || c.Version != "1.0.0" || len(c.Nodes) != 2 || len(c.Permissions) != 2 || c.Unverified {
		t.Fatalf("consent = %+v", c)
	}
}

func TestPushAuditEntry(t *testing.T) {
	fx := newPusherFixture(t, plugin.TrustVerified)
	if _, err := fx.p.Push(plugin.PushRequest{ID: "xp", Nodes: []string{"web-1", "db-1"}, Actor: "alice", Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(fx.audit)
	if err != nil {
		t.Fatal(err)
	}
	var e core.AuditEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &e); err != nil {
		t.Fatal(err)
	}
	if e.Action != "fleet.plugin.push" || e.Actor != "alice" || e.Target != "xp" || !strings.Contains(e.Detail, "web-1") || !strings.Contains(e.Detail, "db-1") {
		t.Fatalf("audit = %+v", e)
	}
}

func TestOpenOnlyForOfferedNode(t *testing.T) {
	fx := newPusherFixture(t, plugin.TrustVerified)
	if _, _, err := fx.p.Open("n1", shaOf(fx.data)); err == nil {
		t.Fatal("Open succeeded without an offer")
	}
	if _, err := fx.p.Push(plugin.PushRequest{ID: "xp", Nodes: []string{"web-1"}}); err != nil {
		t.Fatal(err)
	}
	rc, size, err := fx.p.Open("n1", shaOf(fx.data))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if size != int64(len(fx.data)) || string(b) != string(fx.data) {
		t.Fatalf("size %d body %q", size, b)
	}
	if _, _, err := fx.p.Open("n2", shaOf(fx.data)); err == nil {
		t.Fatal("Open succeeded for a node without an offer")
	}
	if _, _, err := fx.p.Open("n1", strings.Repeat("0", 64)); err == nil {
		t.Fatal("Open succeeded for an unoffered digest")
	}
	fx.p.now = func() time.Time { return time.Unix(1_700_000_000, 0).Add(pluginOfferTTL + time.Second) }
	if _, _, err := fx.p.Open("n1", shaOf(fx.data)); err == nil {
		t.Fatal("expired offer still served")
	}
}

func TestOnConnectReoffersPending(t *testing.T) {
	fx := newPusherFixture(t, plugin.TrustVerified)
	fx.online["n1"] = false
	if _, err := fx.p.Push(plugin.PushRequest{ID: "xp", Nodes: []string{"web-1"}}); err != nil {
		t.Fatal(err)
	}
	if len(fx.frames) != 0 {
		t.Fatal("frame delivered to an offline node")
	}
	fx.online["n1"] = true
	fx.p.OnConnect("n1")
	if len(fx.framesFor("n1", fleet.FramePluginOffer)) != 1 {
		t.Fatalf("frames = %+v", fx.frames)
	}
	fx.p.OnReport("n1", []fleet.PluginReport{{ID: "xp", Phase: phaseEnabled, State: plugin.StateEnabled}})
	fx.p.OnConnect("n1")
	if len(fx.framesFor("n1", fleet.FramePluginOffer)) != 1 {
		t.Fatal("completed offer re-sent")
	}

	fx.online["n2"] = false
	if _, err := fx.p.Push(plugin.PushRequest{ID: "xp", Nodes: []string{"web-2"}, Remove: true}); err != nil {
		t.Fatal(err)
	}
	fx.online["n2"] = true
	fx.p.OnConnect("n2")
	if len(fx.framesFor("n2", fleet.FramePluginRemove)) != 1 {
		t.Fatalf("pending removal not re-sent: %+v", fx.frames)
	}
}

func TestNodeStatesFromReports(t *testing.T) {
	fx := newPusherFixture(t, plugin.TrustVerified)
	if _, err := fx.p.Push(plugin.PushRequest{ID: "xp", Nodes: []string{"web-1", "web-2"}}); err != nil {
		t.Fatal(err)
	}
	fx.p.OnReport("n1", []fleet.PluginReport{{ID: "xp", Version: "1.0.0", Phase: phaseDownloading, Received: 5, Size: 13}})
	fx.p.OnReport("n2", []fleet.PluginReport{{ID: "xp", Phase: phaseFailed, Error: "bad"}, {ID: "unrelated", Phase: phaseEnabled}})
	st, err := fx.p.NodeStates("xp")
	if err != nil || len(st) != 2 {
		t.Fatalf("states = %+v err %v", st, err)
	}
	if st[0].NodeName != "web-1" || st[0].Phase != phaseDownloading || st[0].Received != 5 || st[1].Phase != phaseFailed || st[1].Error != "bad" {
		t.Fatalf("states = %+v", st)
	}
}

func TestRevokeSentToConnectedAndReplayedOnConnect(t *testing.T) {
	fx := newPusherFixture(t, plugin.TrustVerified)
	fx.online["n2"] = false
	fx.p.Revoke([]plugin.Revocation{{ID: "xp", Reason: "bad"}})
	if len(fx.framesFor("n1", fleet.FramePluginRevoke)) != 1 || len(fx.framesFor("n2", fleet.FramePluginRevoke)) != 0 {
		t.Fatalf("frames = %+v", fx.frames)
	}
	fx.online["n2"] = true
	fx.p.OnConnect("n2")
	if len(fx.framesFor("n2", fleet.FramePluginRevoke)) != 1 {
		t.Fatal("revocation not replayed on connect")
	}
}
