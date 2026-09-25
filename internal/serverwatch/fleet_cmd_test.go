package serverwatch

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/control"
	"serverwatch/internal/core"
	"serverwatch/internal/fleet"
)

func fleetCLIEnv(t *testing.T) (dir string, out, errb *bytes.Buffer) {
	t.Helper()
	dir = t.TempDir()
	prevState, prevCfg, prevOut, prevErr := stateDir, cfgPath, stdout, stderr
	stateDir, cfgPath = dir, filepath.Join(dir, "config.json")
	out, errb = &bytes.Buffer{}, &bytes.Buffer{}
	stdout, stderr = out, errb
	t.Setenv("RUNTIME_DIRECTORY", t.TempDir())
	t.Cleanup(func() { stateDir, cfgPath, stdout, stderr = prevState, prevCfg, prevOut, prevErr })
	return dir, out, errb
}

func loadTestCfg(t *testing.T) *config.Config {
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// chmodUnwritable makes it impossible to remove entries inside dir (an
// existing directory containing at least one file) without touching dir's
// parent, so os.RemoveAll(dir) fails partway through with a real OS
// permission error -- the CLI's purge-failure path under test. It restores
// permissions in cleanup so the enclosing t.TempDir() can still remove it.
func chmodUnwritable(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
}

// fleetCLIFakeAPI is a minimal core.API stub: the fleet CLI's daemon-backed
// subcommands never call these methods directly (they go through
// core.FleetAPI via Client.Fleet()), but control.Serve requires its api
// argument to satisfy the full core.API interface.
type fleetCLIFakeAPI struct{}

func (fleetCLIFakeAPI) Snapshot() (core.DashboardView, error)      { return core.DashboardView{}, nil }
func (fleetCLIFakeAPI) Monitoring() (core.MonitoringView, error)   { return core.MonitoringView{}, nil }
func (fleetCLIFakeAPI) Config() (*config.Config, error)            { return &config.Config{}, nil }
func (fleetCLIFakeAPI) Doctor() (core.DoctorReport, error)         { return core.DoctorReport{}, nil }
func (fleetCLIFakeAPI) HostInfo() (core.HostInfoView, error)       { return core.HostInfoView{}, nil }
func (fleetCLIFakeAPI) Version() (string, error)                   { return "v-test", nil }
func (fleetCLIFakeAPI) ApplyConfig(*config.Config) error           { return nil }
func (fleetCLIFakeAPI) AckAlert(string) error                      { return nil }
func (fleetCLIFakeAPI) UnackAlert(string) error                    { return nil }
func (fleetCLIFakeAPI) TestChannel(string) error                   { return nil }
func (fleetCLIFakeAPI) ValidateChannel(config.ChannelConfig) error { return nil }
func (fleetCLIFakeAPI) ContainerLogs(string, int) (string, error)  { return "", nil }
func (fleetCLIFakeAPI) Series(metric string, from, to int64, res core.Resolution) ([]core.SeriesPoint, error) {
	return nil, nil
}
func (fleetCLIFakeAPI) Events(from, to int64) ([]core.DownEventView, error) { return nil, nil }
func (fleetCLIFakeAPI) ActiveAlerts() ([]core.AlertRecord, error)           { return nil, nil }
func (fleetCLIFakeAPI) AlertHistory(int64, int) ([]core.AlertRecord, error) { return nil, nil }
func (fleetCLIFakeAPI) EnrollmentPIN(context.Context) (string, bool, error) { return "", false, nil }
func (fleetCLIFakeAPI) MonitorTargets(context.Context) ([]core.TargetView, error) {
	return nil, nil
}
func (fleetCLIFakeAPI) Subscribe(context.Context) (<-chan core.Event, error) {
	return nil, errors.New("not implemented")
}

// fleetCLIFake implements core.FleetProvider on top of fleetCLIFakeAPI, with
// every Fleet.* call recorded so a test can assert what the CLI sent, and
// every response settable so a test can assert what the CLI printed.
type fleetCLIFake struct {
	fleetCLIFakeAPI

	status core.FleetStatus
	nodes  []core.NodeSummary
	tokens []core.TokenView

	renamedID, renamedTo string
	taggedID             string
	taggedTags           []string
	revokedID            string
	revokeErr            error
	removedID            string
	deletedTokenID       string
	createSpec           core.TokenSpec
	createResult         core.CreatedToken
}

func (f *fleetCLIFake) Fleet() core.FleetAPI          { return fleetCLIFakeFleetAPI{f} }
func (f *fleetCLIFake) Node(string) (core.API, error) { return nil, core.ErrNoSuchNode }

type fleetCLIFakeFleetAPI struct{ f *fleetCLIFake }

func (a fleetCLIFakeFleetAPI) Status() (core.FleetStatus, error) { return a.f.status, nil }

func (a fleetCLIFakeFleetAPI) Nodes(filter core.NodeFilter) ([]core.NodeSummary, error) {
	var out []core.NodeSummary
	for _, n := range a.f.nodes {
		if filter.Match(n) {
			out = append(out, n)
		}
	}
	return out, nil
}

func (a fleetCLIFakeFleetAPI) RenameNode(id, name string) error {
	a.f.renamedID, a.f.renamedTo = id, name
	return nil
}

func (a fleetCLIFakeFleetAPI) SetNodeTags(id string, tags []string) error {
	a.f.taggedID, a.f.taggedTags = id, tags
	return nil
}

func (a fleetCLIFakeFleetAPI) RevokeNode(id string) error {
	a.f.revokedID = id
	return a.f.revokeErr
}

func (a fleetCLIFakeFleetAPI) RemoveNode(id string) error {
	a.f.removedID = id
	return nil
}

func (a fleetCLIFakeFleetAPI) Tokens() ([]core.TokenView, error) { return a.f.tokens, nil }

func (a fleetCLIFakeFleetAPI) CreateToken(spec core.TokenSpec) (core.CreatedToken, error) {
	a.f.createSpec = spec
	return a.f.createResult, nil
}

func (a fleetCLIFakeFleetAPI) DeleteToken(id string) error {
	a.f.deletedTokenID = id
	return nil
}

// startFleetDaemon stands up a real control.Serve loop at
// controlSocketPath/controlTokenPath, so withDaemon's control.Dial in
// fleet_cmd.go reaches it precisely as it would a real daemon. It points
// RUNTIME_DIRECTORY at a freshly made short-prefix temp dir rather than
// reusing fleetCLIEnv's: a plain t.TempDir() nests under the test's name,
// which combined with a long test name and a long $TMPDIR (common on
// macOS) can exceed unix domain sockets' ~104-byte sun_path limit --
// mirroring internal/control/server_test.go's shortSocketPath fix for the
// same problem.
func startFleetDaemon(t *testing.T, api core.API) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sw-fleet")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("RUNTIME_DIRECTORY", dir)

	const token = "test-token"
	if err := os.WriteFile(controlTokenPath(), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", controlSocketPath())
	if err != nil {
		t.Fatal(err)
	}
	go control.Serve(api, ln, token)
	t.Cleanup(func() { ln.Close() })
}

func TestFleetInitWritesPKIAndConfig(t *testing.T) {
	dir, out, errb := fleetCLIEnv(t)
	if code := Main([]string{"fleet", "init", "--address", "127.0.0.1,mon.local", "--port", "9555"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	c := loadTestCfg(t)
	if c.FleetRole() != config.RoleMaster || c.Fleet.Address != "127.0.0.1,mon.local" || c.FleetListen() != ":9555" {
		t.Fatalf("fleet cfg = %+v", c.Fleet)
	}
	for _, f := range []string{"ca.crt", "ca.key", "server.crt", "server.key"} {
		if _, err := os.Stat(filepath.Join(dir, "fleet", "pki", f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "fleet", "pki", "ca.key")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("ca.key mode %v", fi.Mode().Perm())
	}
	if !strings.Contains(out.String(), "sha256:") || !strings.Contains(out.String(), "systemctl restart") {
		t.Fatalf("output = %s", out)
	}
	if code := Main([]string{"fleet", "init", "--address", "x"}); code == 0 {
		t.Fatal("second init on a master succeeded")
	}
}

func TestFleetInitRequiresAddress(t *testing.T) {
	fleetCLIEnv(t)
	if code := Main([]string{"fleet", "init"}); code == 0 {
		t.Fatal("init without --address succeeded")
	}
}

func testMasterForCLI(t *testing.T) (code string) {
	t.Helper()
	mdir := t.TempDir()
	if err := fleetInitPKI(mdir, []string{"127.0.0.1"}, "t", time.Now()); err != nil {
		t.Fatal(err)
	}
	pki := fleetPKIDir(mdir)
	ca, _ := fleet.LoadCA(filepath.Join(pki, "ca.crt"), filepath.Join(pki, "ca.key"))
	leaf, _ := tls.LoadX509KeyPair(filepath.Join(pki, "server.crt"), filepath.Join(pki, "server.key"))
	reg, _ := fleet.OpenRegistry(filepath.Join(mdir, "fleet", "registry.json"))
	toks, _ := fleet.OpenTokens(filepath.Join(mdir, "fleet", "tokens.json"))
	m := fleet.NewMaster(fleet.MasterConfig{CA: ca, Leaf: leaf, Registry: reg, Tokens: toks, Sink: newReplicaSink(filepath.Join(mdir, "nodes"), StoreOptions{})})
	srv := httptest.NewUnstartedServer(m.Handler())
	srv.TLS = fleet.ServerTLS(leaf, ca.Cert)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	plain, _, _ := toks.Create(time.Hour, 1, nil, "t", time.Now())
	return fleet.EncodeJoin(fleet.JoinInfo{URL: srv.URL, Token: plain, Pin: fleet.SPKIPin(ca.Cert)})
}

func TestFleetJoinAndLeave(t *testing.T) {
	dir, _, errb := fleetCLIEnv(t)
	code := testMasterForCLI(t)
	if rc := Main([]string{"fleet", "join", code, "--name", "box-1"}); rc != 0 {
		t.Fatalf("join exit %d: %s", rc, errb)
	}
	c := loadTestCfg(t)
	if c.FleetRole() != config.RoleChild || c.Fleet.NodeID == "" || c.Fleet.CAPin == "" || c.Fleet.MasterURL == "" {
		t.Fatalf("fleet cfg = %+v", c.Fleet)
	}
	if _, err := os.Stat(filepath.Join(dir, "fleet-child", "node.key")); err != nil {
		t.Fatal(err)
	}
	if rc := Main([]string{"fleet", "join", code}); rc == 0 {
		t.Fatal("join while already a child succeeded")
	}
	if rc := Main([]string{"fleet", "leave", "--purge"}); rc != 0 {
		t.Fatalf("leave exit %d: %s", rc, errb)
	}
	if c := loadTestCfg(t); c.FleetRole() != config.RoleSolo || c.Fleet.NodeID != "" {
		t.Fatalf("after leave = %+v", c.Fleet)
	}
	if _, err := os.Stat(filepath.Join(dir, "fleet-child")); !os.IsNotExist(err) {
		t.Fatal("purge kept fleet-child")
	}
}

func TestFleetDaemonCommandsExplainWhenDaemonDown(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "nodes"}); rc != 1 {
		t.Fatalf("exit %d", rc)
	}
	if !strings.Contains(errb.String(), "not reachable") {
		t.Fatalf("stderr = %s", errb)
	}
}

func TestFleetUnknownSubcommand(t *testing.T) {
	fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "bogus"}); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
}

// --- purge failure reporting (fix round 1, item 1) ---

func TestFleetLeavePurgeReportsUnremovablePath(t *testing.T) {
	dir, out, errb := fleetCLIEnv(t)
	code := testMasterForCLI(t)
	if rc := Main([]string{"fleet", "join", code}); rc != 0 {
		t.Fatalf("join exit %d: %s", rc, errb)
	}
	out.Reset()
	errb.Reset()
	childDir := filepath.Join(dir, "fleet-child")
	chmodUnwritable(t, childDir)

	rc := Main([]string{"fleet", "leave", "--purge"})
	if rc != 1 {
		t.Fatalf("exit %d, want 1: out=%s err=%s", rc, out, errb)
	}
	if !strings.Contains(errb.String(), "left the fleet, but could not delete "+childDir) {
		t.Fatalf("stderr = %s", errb)
	}
	if !strings.Contains(errb.String(), "remove it manually") {
		t.Fatalf("stderr = %s", errb)
	}
	if strings.Contains(out.String(), "this node's fleet identity") {
		t.Fatalf("Deleted line must not list the target that failed: %s", out)
	}
	if !strings.Contains(out.String(), "Deleted unsent outbox.") {
		t.Fatalf("out = %s (outbox removal should still be reported)", out)
	}
	if c := loadTestCfg(t); c.FleetRole() != config.RoleSolo {
		t.Fatalf("role should already be solo despite the purge failure: %+v", c.Fleet)
	}
}

func TestFleetDisablePurgeReportsUnremovablePath(t *testing.T) {
	dir, out, errb := fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "init", "--address", "127.0.0.1"}); rc != 0 {
		t.Fatalf("init exit %d: %s", rc, errb)
	}
	out.Reset()
	errb.Reset()
	masterDir := filepath.Join(dir, "fleet")
	chmodUnwritable(t, masterDir)

	rc := Main([]string{"fleet", "disable", "--purge"})
	if rc != 1 {
		t.Fatalf("exit %d, want 1: out=%s err=%s", rc, out, errb)
	}
	if !strings.Contains(errb.String(), "disabled the fleet master, but could not delete "+masterDir) {
		t.Fatalf("stderr = %s", errb)
	}
	if !strings.Contains(errb.String(), "remove it manually") {
		t.Fatalf("stderr = %s", errb)
	}
	if strings.Contains(out.String(), "Deleted fleet CA") {
		t.Fatalf("Deleted line must not print on failure: %s", out)
	}
	if c := loadTestCfg(t); c.FleetRole() != config.RoleSolo {
		t.Fatalf("role should already be solo despite the purge failure: %+v", c.Fleet)
	}
}

// --- daemon-backed status/nodes/node/token (fix round 1, item 2) ---

func TestFleetStatusMaster(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{status: core.FleetStatus{
		Role: config.RoleMaster, Listen: ":9443", JoinURL: "https://mon.local:9443",
		CAPin: "sha256:abc", Nodes: 3,
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "status"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	got := out.String()
	for _, want := range []string{"role: master", "listening: :9443", "join URL: https://mon.local:9443", "CA fingerprint: sha256:abc", "nodes: 3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q: %s", want, got)
		}
	}
}

func TestFleetStatusChild(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{status: core.FleetStatus{
		Role: config.RoleChild, NodeID: "node-42", MasterURL: "https://master.local:9443",
		Link: &core.LinkView{State: "linked", LastAck: time.Now().Unix(), LastError: "boom",
			OutboxBytes: 2 << 20, Unacked: 5, Gaps: 1},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "status"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	got := out.String()
	for _, want := range []string{"role: child", "node id: node-42", "master: https://master.local:9443",
		"link: linked", "outbox: 2.0 MB", "5 unsent", "1 gaps", "last error: boom"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q: %s", want, got)
		}
	}
}

func TestFleetNodesTableFiltersByState(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{nodes: []core.NodeSummary{
		{ID: "self", Name: "master-1", Self: true, State: "online"},
		{ID: "node-aaaaaa1111", Name: "web-1", State: "online", Version: "v1"},
		{ID: "node-bbbbbb2222", Name: "web-2", State: "down", Version: "v1"},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "nodes", "--state", "down"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	got := out.String()
	if !strings.Contains(got, "web-2") {
		t.Fatalf("filtered output missing web-2: %s", got)
	}
	if strings.Contains(got, "web-1") || strings.Contains(got, "master-1") {
		t.Fatalf("filtered output should exclude non-down nodes: %s", got)
	}
}

func TestFleetNodeRenameByPrefix(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{nodes: []core.NodeSummary{
		{ID: "self", Name: "master-1", Self: true},
		{ID: "abcdef1234567890", Name: "web-1"},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "node", "rename", "abcdef", "web-01"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.renamedID != "abcdef1234567890" || fake.renamedTo != "web-01" {
		t.Fatalf("renamed = %q -> %q", fake.renamedID, fake.renamedTo)
	}
}

func TestFleetNodeAmbiguousPrefixFails(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{nodes: []core.NodeSummary{
		{ID: "self", Name: "master-1", Self: true},
		{ID: "aaaaaa111111", Name: "web-1"},
		{ID: "aaaaaa222222", Name: "web-2"},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "node", "rename", "aaaaaa", "x"}); rc != 1 {
		t.Fatalf("exit %d, want 1", rc)
	}
	if !strings.Contains(errb.String(), "matches 2 nodes") {
		t.Fatalf("stderr = %s", errb)
	}
}

func TestFleetNodeRefMatchingOnlySelfFails(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{nodes: []core.NodeSummary{
		{ID: "self", Name: "master-1", Self: true},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "node", "revoke", "master-1"}); rc != 1 {
		t.Fatalf("exit %d, want 1", rc)
	}
	if !strings.Contains(errb.String(), "no node matches") {
		t.Fatalf("stderr = %s", errb)
	}
}

func TestFleetNodeRevoke(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{nodes: []core.NodeSummary{
		{ID: "self", Name: "master-1", Self: true},
		{ID: "node-1", Name: "web-1"},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "node", "revoke", "node-1"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.revokedID != "node-1" {
		t.Fatalf("revokedID = %q", fake.revokedID)
	}
	if !strings.Contains(out.String(), "Revoked node-1") {
		t.Fatalf("out = %s", out)
	}
}

func TestFleetNodeCommandSurfacesDaemonError(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{
		nodes:     []core.NodeSummary{{ID: "node-1", Name: "web-1"}},
		revokeErr: errors.New("boom"),
	}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "node", "revoke", "node-1"}); rc != 1 {
		t.Fatalf("exit %d, want 1", rc)
	}
	if !strings.Contains(errb.String(), "fleet: boom") {
		t.Fatalf("stderr = %s, want it to contain %q", errb.String(), "fleet: boom")
	}
}

func TestFleetTokenCreate(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{createResult: core.CreatedToken{JoinCode: "swj1_xyz"}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "token", "create", "--tags", "a,b", "--ttl", "2h", "--uses", "3"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	want := core.TokenSpec{TTLSeconds: 7200, Uses: 3, Tags: []string{"a", "b"}, Creator: "cli"}
	if !reflect.DeepEqual(fake.createSpec, want) {
		t.Fatalf("createSpec = %+v, want %+v", fake.createSpec, want)
	}
	if !strings.Contains(out.String(), "swj1_xyz") {
		t.Fatalf("out = %s", out)
	}
}

func TestFleetTokenList(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{tokens: []core.TokenView{{ID: "tok-1", Uses: 2, Tags: []string{"prod"}}}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "token", "list"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if !strings.Contains(out.String(), "tok-1") {
		t.Fatalf("out = %s", out)
	}
}

func TestFleetTokenDelete(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "token", "delete", "tok-9"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.deletedTokenID != "tok-9" {
		t.Fatalf("deletedTokenID = %q", fake.deletedTokenID)
	}
}

// --- stray positional arguments (fix round 1, item 3) ---

func TestFleetInitRejectsExtraPositional(t *testing.T) {
	fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "init", "--address", "x", "extra"}); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
}

func TestFleetLeaveRejectsExtraPositional(t *testing.T) {
	fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "leave", "extra"}); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
}

func TestFleetDisableRejectsExtraPositional(t *testing.T) {
	fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "disable", "extra"}); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
}

func TestFleetNodesRejectsExtraPositional(t *testing.T) {
	fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "nodes", "extra"}); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
}

func TestFleetTokenCreateRejectsExtraPositional(t *testing.T) {
	fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "token", "create", "extra"}); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
}

func TestFleetTokenListRejectsExtraPositional(t *testing.T) {
	fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "token", "list", "extra"}); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
}

func TestFleetNodeRevokeRejectsExtraArgument(t *testing.T) {
	fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "node", "revoke", "node-1", "extra"}); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
}

func TestFleetNodeRemove(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{nodes: []core.NodeSummary{
		{ID: "self", Name: "master-1", Self: true},
		{ID: "node-1", Name: "web-1"},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "node", "remove", "web-1"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.removedID != "node-1" {
		t.Fatalf("removedID = %q", fake.removedID)
	}
	if !strings.Contains(out.String(), "Removed node-1") {
		t.Fatalf("out = %s", out)
	}
}

// Leaving is local: the master keeps expecting the node, so leave tells the
// operator exactly what to run there.
func TestFleetLeavePrintsMasterRevokeHint(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	code := testMasterForCLI(t)
	if rc := Main([]string{"fleet", "join", code}); rc != 0 {
		t.Fatalf("join exit %d: %s", rc, errb)
	}
	id := loadTestCfg(t).Fleet.NodeID
	out.Reset()
	if rc := Main([]string{"fleet", "leave"}); rc != 0 {
		t.Fatalf("leave exit %d: %s", rc, errb)
	}
	if want := "On the master, run: sudo serverwatch fleet node revoke " + id; !strings.Contains(out.String(), want) {
		t.Fatalf("out = %s, want %q", out, want)
	}
}
