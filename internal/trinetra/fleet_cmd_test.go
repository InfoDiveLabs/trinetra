package trinetra

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
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
	depsID               string
	depsSet              []string
	depsActor            string
	depsErr              error
	revokedID            string
	revokeErr            error
	removedID            string
	deletedTokenID       string
	createSpec           core.TokenSpec
	createResult         core.CreatedToken

	incidentsFilter core.IncidentFilter
	incidents       []core.Incident
	incident        core.Incident
	incidentErr     error
	ackedID         string
	ackedActor      string
	ackErr          error
	explainKey      string
	explainResult   []core.IncidentEvent
	explainErr      error
	auditLimit      int
	auditResult     []core.AuditEntry

	silences         []core.Silence
	createdSilence   core.Silence
	createSilenceErr error
	expiredID        string
	expiredActor     string

	maintenances      []core.Maintenance
	savedMaintenance  core.Maintenance
	saveMaintErr      error
	deletedMaintID    string
	deletedMaintActor string

	alertingCfg      core.AlertingConfig
	alertingErr      error
	setAlertingCfg   core.AlertingConfig
	setAlertingActor string
	setAlertingErr   error
	routeTestAlert   core.TestAlert
	routeTestResult  core.RouteDecision
	routeTestErr     error

	ruleStates    []core.RuleState
	ruleStatesErr error

	managed             []core.ManagedFragment
	managedErr          error
	savedManaged        core.ManagedFragment
	savedManagedActor   string
	saveManagedResult   core.ManagedFragment
	saveManagedErr      error
	deletedManagedID    string
	deletedManagedActor string
	deleteManagedErr    error
	managedStatus       []core.ManagedStatus
	managedStatusErr    error
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

func (a fleetCLIFakeFleetAPI) SetNodeDeps(id string, deps []string, actor string) error {
	a.f.depsID, a.f.depsSet, a.f.depsActor = id, deps, actor
	return a.f.depsErr
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

func (a fleetCLIFakeFleetAPI) Incidents(filter core.IncidentFilter) ([]core.Incident, error) {
	a.f.incidentsFilter = filter
	return a.f.incidents, nil
}

func (a fleetCLIFakeFleetAPI) Incident(id string) (core.Incident, error) {
	return a.f.incident, a.f.incidentErr
}

func (a fleetCLIFakeFleetAPI) AckIncident(id, actor string) error {
	a.f.ackedID, a.f.ackedActor = id, actor
	return a.f.ackErr
}

func (a fleetCLIFakeFleetAPI) Explain(key string) ([]core.IncidentEvent, error) {
	a.f.explainKey = key
	return a.f.explainResult, a.f.explainErr
}

func (a fleetCLIFakeFleetAPI) Audit(limit int) ([]core.AuditEntry, error) {
	a.f.auditLimit = limit
	return a.f.auditResult, nil
}

func (a fleetCLIFakeFleetAPI) Silences() ([]core.Silence, error) { return a.f.silences, nil }

func (a fleetCLIFakeFleetAPI) CreateSilence(s core.Silence) (core.Silence, error) {
	a.f.createdSilence = s
	if a.f.createSilenceErr != nil {
		return core.Silence{}, a.f.createSilenceErr
	}
	s.ID = "sil123"
	return s, nil
}

func (a fleetCLIFakeFleetAPI) ExpireSilence(id, actor string) error {
	a.f.expiredID, a.f.expiredActor = id, actor
	return nil
}

func (a fleetCLIFakeFleetAPI) Maintenances() ([]core.Maintenance, error) {
	return a.f.maintenances, nil
}

func (a fleetCLIFakeFleetAPI) SaveMaintenance(m core.Maintenance) (core.Maintenance, error) {
	a.f.savedMaintenance = m
	if a.f.saveMaintErr != nil {
		return core.Maintenance{}, a.f.saveMaintErr
	}
	if m.ID == "" {
		m.ID = "maint123"
	}
	return m, nil
}

func (a fleetCLIFakeFleetAPI) DeleteMaintenance(id, actor string) error {
	a.f.deletedMaintID, a.f.deletedMaintActor = id, actor
	return nil
}

func (a fleetCLIFakeFleetAPI) Alerting() (core.AlertingConfig, error) {
	return a.f.alertingCfg, a.f.alertingErr
}

func (a fleetCLIFakeFleetAPI) SetAlerting(cfg core.AlertingConfig, actor string) error {
	a.f.setAlertingCfg, a.f.setAlertingActor = cfg, actor
	return a.f.setAlertingErr
}

func (a fleetCLIFakeFleetAPI) RouteTest(alert core.TestAlert) (core.RouteDecision, error) {
	a.f.routeTestAlert = alert
	return a.f.routeTestResult, a.f.routeTestErr
}

func (a fleetCLIFakeFleetAPI) RuleStates() ([]core.RuleState, error) {
	return a.f.ruleStates, a.f.ruleStatesErr
}

func (a fleetCLIFakeFleetAPI) Managed() ([]core.ManagedFragment, error) {
	return a.f.managed, a.f.managedErr
}

func (a fleetCLIFakeFleetAPI) SaveManaged(frag core.ManagedFragment, actor string) (core.ManagedFragment, error) {
	a.f.savedManaged, a.f.savedManagedActor = frag, actor
	if a.f.saveManagedErr != nil {
		return core.ManagedFragment{}, a.f.saveManagedErr
	}
	if a.f.saveManagedResult.ID != "" {
		return a.f.saveManagedResult, nil
	}
	if frag.ID == "" {
		frag.ID = "mf123"
	}
	return frag, nil
}

func (a fleetCLIFakeFleetAPI) DeleteManaged(id, actor string) error {
	a.f.deletedManagedID, a.f.deletedManagedActor = id, actor
	return a.f.deleteManagedErr
}

func (a fleetCLIFakeFleetAPI) ManagedStatus() ([]core.ManagedStatus, error) {
	return a.f.managedStatus, a.f.managedStatusErr
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
	m := fleet.NewMaster(fleet.MasterConfig{CA: ca, Leaf: leaf, Registry: reg, Tokens: toks, Sink: newReplicaSink(filepath.Join(mdir, "nodes"), StoreOptions{}, nil)})
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

// testOldMasterForCLI stands up a bare-bones fake master that answers
// POST /fleet/v1/join with a JoinResponse containing no "name" field at
// all -- exactly what an older master (from before JoinResponse.Name
// existed) would send. It signs the child's CSR for real (via the same CA
// fleet.Join validates the returned cert against), but has none of the real
// master's token/registry bookkeeping: it exists purely to test the CLI's
// handling of a response with the name field entirely absent.
func testOldMasterForCLI(t *testing.T) (code string) {
	t.Helper()
	mdir := t.TempDir()
	if err := fleetInitPKI(mdir, []string{"127.0.0.1"}, "t", time.Now()); err != nil {
		t.Fatal(err)
	}
	pki := fleetPKIDir(mdir)
	ca, err := fleet.LoadCA(filepath.Join(pki, "ca.crt"), filepath.Join(pki, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tls.LoadX509KeyPair(filepath.Join(pki, "server.crt"), filepath.Join(pki, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(filepath.Join(pki, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+fleet.PathJoin, func(w http.ResponseWriter, r *http.Request) {
		var req fleet.JoinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		id, err := fleet.NewNodeID()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		certPEM, _, _, err := ca.SignClient([]byte(req.CSR), id, time.Now(), fleet.ClientCertLife)
		if err != nil {
			http.Error(w, "bad csr", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Deliberately no "name" key at all -- an older master's shape.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"node_id": id,
			"cert":    string(certPEM),
			"ca":      string(caPEM),
		})
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = fleet.ServerTLS(leaf, ca.Cert)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return fleet.EncodeJoin(fleet.JoinInfo{URL: srv.URL, Token: "swt_unused", Pin: fleet.SPKIPin(ca.Cert)})
}

// TestFleetJoinAgainstOlderMasterPrintsRequestedName is the review round-3
// item 1 regression test: an older master's JoinResponse has no "name"
// field, which decodes as "" -- the CLI must treat that as "not reported"
// (fall back to the requested name) rather than printing a false
// "registered as \"\" instead" note.
func TestFleetJoinAgainstOlderMasterPrintsRequestedName(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	code := testOldMasterForCLI(t)
	if rc := Main([]string{"fleet", "join", code, "--name", "box-1"}); rc != 0 {
		t.Fatalf("join exit %d: %s", rc, errb)
	}
	got := out.String()
	if strings.Contains(got, "Note:") {
		t.Fatalf("out = %s, want no Note line against an older master that never reports a name", got)
	}
	if !strings.Contains(got, "(box-1)") {
		t.Fatalf("out = %s, want the success line to show the requested name box-1", got)
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

// TestFleetNodeDependsSetsAndClears covers task 6 part 3's CLI:
// `fleet node depends <node> <dep,...>`, with an empty value clearing it.
func TestFleetNodeDependsSetsAndClears(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{nodes: []core.NodeSummary{
		{ID: "self", Name: "master-1", Self: true},
		{ID: "node-1", Name: "web-1"},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "node", "depends", "node-1", "node-2,tag:db"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.depsID != "node-1" || fake.depsActor != "cli" || strings.Join(fake.depsSet, ",") != "node-2,tag:db" {
		t.Fatalf("depsID=%q depsActor=%q depsSet=%v", fake.depsID, fake.depsActor, fake.depsSet)
	}
	_ = out

	if rc := Main([]string{"fleet", "node", "depends", "node-1", ""}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.depsID != "node-1" || len(fake.depsSet) != 0 {
		t.Fatalf("clearing deps: depsID=%q depsSet=%v, want empty", fake.depsID, fake.depsSet)
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
	if want := "On the master, run: sudo trinetra fleet node revoke " + id; !strings.Contains(out.String(), want) {
		t.Fatalf("out = %s, want %q", out, want)
	}
}

func TestFleetNodesShowsSkew(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{nodes: []core.NodeSummary{
		{ID: "self", Name: "master-1", Self: true, State: "online"},
		{ID: "node-aaaaaa1111", Name: "web-1", State: "lagging", SkewSec: -45},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "nodes"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if got := out.String(); !strings.Contains(got, "SKEW") || !strings.Contains(got, "-45s") {
		t.Fatalf("output = %s", got)
	}
}

func TestFleetStatusMasterNotesDropsAndSkew(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{
		status: core.FleetStatus{Role: config.RoleMaster, Nodes: 3},
		nodes: []core.NodeSummary{
			{ID: "self", Name: "master-1", Self: true},
			{ID: "node-1", Name: "web-1", DroppedOutOfOrder: 7, DroppedCardinality: 2, DroppedDuplicate: 5},
			{ID: "node-2", Name: "web-2", SkewSec: 120},
			{ID: "node-3", Name: "web-3"},
		},
	}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "status"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	got := out.String()
	for _, want := range []string{"web-1", "7 out of order", "2 over the series limit", "5 duplicates", "web-2", "120s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q: %s", want, got)
		}
	}
	// Cumulative drop counts are shown, not warned about: the master logs a
	// warning when they grow.
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "web-1") && strings.Contains(l, "warning") {
			t.Fatalf("drop counts printed as a warning: %q", l)
		}
	}
	if strings.Contains(got, "web-3") {
		t.Fatalf("healthy node listed: %s", got)
	}
}

// A child whose live updates reach the master while its data lane retries
// says so, rather than claiming the link is fine or down.
func TestFleetStatusChildCatchingUp(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{status: core.FleetStatus{
		Role: config.RoleChild, NodeID: "node-42", MasterURL: "https://master.local:9443",
		Link: &core.LinkView{State: "catching up", LastAck: time.Now().Unix(), LastError: "fleet: master busy (503)", Unacked: 40},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "status"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	got := out.String()
	for _, want := range []string{"link: catching up (master reachable; unsent data is being retried)", "40 unsent", "last error: fleet: master busy (503)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q: %s", want, got)
		}
	}
}

func TestFleetIncidentsListsAndFilters(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{incidents: []core.Incident{
		{ID: "abc123def456", State: "firing", Severity: "critical", Title: "cpu high", Nodes: []string{"n1"}, Opened: time.Now().Unix(), Updated: time.Now().Unix()},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "incidents", "--state", "firing"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.incidentsFilter.State != "firing" {
		t.Fatalf("filter = %+v", fake.incidentsFilter)
	}
	got := out.String()
	if !strings.Contains(got, "abc123def456") || !strings.Contains(got, "cpu high") {
		t.Fatalf("out = %s", got)
	}
}

func TestFleetIncidentShowsDetail(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{incident: core.Incident{
		ID: "abc123def456", State: "resolved", Severity: "warning", Title: "cpu high", Nodes: []string{"n1"},
		Opened: 1000, Updated: 1050, Resolved: 1050,
		Alerts:   []core.IncidentAlert{{Node: "n1", Key: "cpu", Severity: "warning", FiredAt: 1000, ResolvedAt: 1050}},
		Timeline: []core.IncidentEvent{{TS: 1000, Kind: "fired", Detail: "fired on n1", Actor: "system"}},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "incident", "abc123def456"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	got := out.String()
	for _, want := range []string{"id: abc123def456", "state: resolved", "n1 cpu (warning)", "fired: fired on n1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q: %s", want, got)
		}
	}
}

func TestFleetAckCallsAckIncidentWithCLIActor(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "ack", "abc123def456"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.ackedID != "abc123def456" || fake.ackedActor != "cli" {
		t.Fatalf("ackedID = %q, ackedActor = %q", fake.ackedID, fake.ackedActor)
	}
	if !strings.Contains(out.String(), "Acknowledged incident abc123def456") {
		t.Fatalf("out = %s", out)
	}
}

func TestFleetExplainPrintsTimeline(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{explainResult: []core.IncidentEvent{
		{TS: 1000, Kind: "fired", Detail: "fired on n1", Actor: "system"},
		{TS: 1001, Kind: "delivered", Detail: "sent via the master's dispatcher"},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "explain", "cpu"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.explainKey != "cpu" {
		t.Fatalf("explainKey = %q", fake.explainKey)
	}
	got := out.String()
	if !strings.Contains(got, "fired: fired on n1 (system)") || !strings.Contains(got, "delivered: sent via the master's dispatcher") {
		t.Fatalf("out = %s", got)
	}
}

func TestFleetExplainSurfacesDaemonError(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{explainErr: errors.New("no incident or alert key \"cpu\" found")}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "explain", "cpu"}); rc != 1 {
		t.Fatalf("exit %d, want 1", rc)
	}
	if !strings.Contains(errb.String(), "no incident or alert key") {
		t.Fatalf("stderr = %s", errb)
	}
}

// --- silence / maintenance CLI ---------------------------------------------

func TestFleetSilenceAddParsesMatchAndFor(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "silence", "add", "--match", "tag=web,rule=cpu*,severity=critical", "--for", "2h", "--comment", "known issue"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	s := fake.createdSilence
	if len(s.Matchers) != 1 || s.Matchers[0].Tag != "web" || s.Matchers[0].Rule != "cpu*" || s.Matchers[0].Severity != "critical" {
		t.Fatalf("matchers = %+v", s.Matchers)
	}
	if s.Author != "cli" || s.Comment != "known issue" {
		t.Fatalf("author/comment = %q/%q", s.Author, s.Comment)
	}
	if s.End-s.Start < 2*3600-5 || s.End-s.Start > 2*3600+5 {
		t.Fatalf("duration = %ds, want ~2h", s.End-s.Start)
	}
	if !strings.Contains(out.String(), "sil123") {
		t.Fatalf("out = %s", out)
	}
}

func TestFleetSilenceAddWithUntil(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{}
	startFleetDaemon(t, fake)
	until := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	if rc := Main([]string{"fleet", "silence", "add", "--match", "node=db1", "--until", until}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	wantEnd, _ := time.Parse(time.RFC3339, until)
	if fake.createdSilence.End != wantEnd.Unix() {
		t.Fatalf("end = %d, want %d", fake.createdSilence.End, wantEnd.Unix())
	}
}

func TestFleetSilenceAddRequiresMatch(t *testing.T) {
	fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "silence", "add", "--for", "1h"}); rc != 2 {
		t.Fatalf("exit %d, want 2 (no daemon needed: parsed before dialing)", rc)
	}
}

func TestFleetSilenceAddRequiresForOrUntil(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "silence", "add", "--match", "tag=web"}); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
	if !strings.Contains(errb.String(), "--for or --until") {
		t.Fatalf("stderr = %s", errb)
	}
}

func TestFleetSilenceList(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{silences: []core.Silence{
		{ID: "sabc", Matchers: []core.Matcher{{Tag: "web"}}, Start: 1000, End: 2000, Author: "cli", Comment: "c1"},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "silence", "list"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	got := out.String()
	if !strings.Contains(got, "sabc") || !strings.Contains(got, "tag=web") || !strings.Contains(got, "c1") {
		t.Fatalf("out = %s", got)
	}
}

func TestFleetSilenceExpire(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "silence", "expire", "sabc"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.expiredID != "sabc" || fake.expiredActor != "cli" {
		t.Fatalf("expiredID = %q actor = %q", fake.expiredID, fake.expiredActor)
	}
	if !strings.Contains(out.String(), "Expired silence sabc") {
		t.Fatalf("out = %s", out)
	}
}

func TestFleetMaintenanceAdd(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{}
	startFleetDaemon(t, fake)
	rc := Main([]string{"fleet", "maintenance", "add", "--name", "patch window", "--match", "tag=web",
		"--days", "mon,tue", "--from", "22:00", "--to", "02:00", "--tz", "Asia/Kolkata"})
	if rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	m := fake.savedMaintenance
	if m.Name != "patch window" || m.From != "22:00" || m.To != "02:00" || m.TZ != "Asia/Kolkata" {
		t.Fatalf("maintenance = %+v", m)
	}
	if len(m.Weekdays) != 2 || m.Weekdays[0] != 1 || m.Weekdays[1] != 2 {
		t.Fatalf("weekdays = %v, want [1 2] (mon,tue)", m.Weekdays)
	}
	if m.Author != "cli" {
		t.Fatalf("author = %q", m.Author)
	}
	if !strings.Contains(out.String(), "maint123") {
		t.Fatalf("out = %s", out)
	}
}

func TestFleetMaintenanceList(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{maintenances: []core.Maintenance{
		{ID: "mabc", Name: "patch", Weekdays: []int{1, 2}, From: "22:00", To: "02:00", TZ: "UTC", Author: "cli"},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "maintenance", "list"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	got := out.String()
	if !strings.Contains(got, "mabc") || !strings.Contains(got, "mon,tue") {
		t.Fatalf("out = %s", got)
	}
}

func TestFleetMaintenanceDelete(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "maintenance", "delete", "mabc"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.deletedMaintID != "mabc" || fake.deletedMaintActor != "cli" {
		t.Fatalf("deletedMaintID = %q actor = %q", fake.deletedMaintID, fake.deletedMaintActor)
	}
	if !strings.Contains(out.String(), "Deleted maintenance window mabc") {
		t.Fatalf("out = %s", out)
	}
}

// TestFleetRouteTestPrintsDecision covers the B5 fix round 1 CLI output:
// every matched policy (here, two -- as a Continue chain would produce)
// prints its own steps and repeat_every separately, since each escalates
// independently.
func TestFleetRouteTestPrintsDecision(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{routeTestResult: core.RouteDecision{
		Route: "web-cpu",
		Policies: []core.Policy{
			{Name: "p1", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"slack"}}, {After: "5m", Channels: []string{"pager"}}}},
			{Name: "p2", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"email"}}}, RepeatEvery: "30m"},
		},
		Suppressed: "silence sabc by cli",
	}}
	startFleetDaemon(t, fake)
	rc := Main([]string{"fleet", "route", "test", "--node", "web1", "--tag", "prod", "--rule", "cpu", "--severity", "critical"})
	if rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.routeTestAlert.Node != "web1" || fake.routeTestAlert.Rule != "cpu" || fake.routeTestAlert.Severity != "critical" ||
		len(fake.routeTestAlert.Tags) != 1 || fake.routeTestAlert.Tags[0] != "prod" {
		t.Fatalf("routeTestAlert = %+v", fake.routeTestAlert)
	}
	got := out.String()
	for _, want := range []string{
		"web-cpu", "policy: p1", "step 0: after 0s -> slack", "step 1: after 5m -> pager",
		"policy: p2", "step 0: after 0s -> email", "repeat_every: 30m",
		"suppressed: silence sabc by cli",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("out missing %q: %s", want, got)
		}
	}
}

func TestFleetRouteTestNoMatchPrintsDefaultNote(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{routeTestResult: core.RouteDecision{
		Policies: []core.Policy{{Name: "default", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}}},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "route", "test", "--node", "web1", "--rule", "cpu", "--severity", "critical"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	got := out.String()
	if !strings.Contains(got, "no route matched") || !strings.Contains(got, "suppressed: no") {
		t.Fatalf("out = %s", got)
	}
}

func TestFleetAlertingShow(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{alertingCfg: core.AlertingConfig{
		Version: 3, DefaultPolicy: "default",
		Policies: []core.Policy{{Name: "default", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}}},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "alerting", "show"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	var got core.AlertingConfig
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if got.Version != 3 || got.DefaultPolicy != "default" {
		t.Fatalf("decoded = %+v", got)
	}
}

func TestFleetRulesCmd(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{ruleStates: []core.RuleState{
		{Name: "hot-web", Expr: "count(tag:web, cpu > 90) >= 1 for 5m", Value: 1, HasValue: true, Firing: true, Since: 1_700_000_000},
		{Name: "db-mem", Expr: "avg(tag:db, mem) > 85 for 10m"},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "rules"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	got := out.String()
	if !strings.Contains(got, "hot-web") || !strings.Contains(got, "firing") || !strings.Contains(got, "count(tag:web, cpu > 90) >= 1 for 5m") {
		t.Fatalf("output missing firing rule row: %s", got)
	}
	if !strings.Contains(got, "db-mem") || !strings.Contains(got, "ok") {
		t.Fatalf("output missing idle rule row: %s", got)
	}
}

func TestFleetAlertingApply(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{}
	startFleetDaemon(t, fake)
	path := filepath.Join(t.TempDir(), "alerting.json")
	body := `{"version":99,"default_policy":"p","policies":[{"name":"p","steps":[{"after":"0s","channels":["slack"]}]}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := Main([]string{"fleet", "alerting", "apply", path}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.setAlertingCfg.Version != 0 {
		t.Fatalf("applied Version = %d, want 0 (CLI apply is always unconditional)", fake.setAlertingCfg.Version)
	}
	if fake.setAlertingCfg.DefaultPolicy != "p" || fake.setAlertingActor != "cli" {
		t.Fatalf("applied cfg = %+v actor = %q", fake.setAlertingCfg, fake.setAlertingActor)
	}
	if !strings.Contains(out.String(), "Applied alerting config") {
		t.Fatalf("out = %s", out)
	}
}

func TestFleetAlertingApplyRejectsBadJSON(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{}
	startFleetDaemon(t, fake)
	path := filepath.Join(t.TempDir(), "alerting.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := Main([]string{"fleet", "alerting", "apply", path}); rc != 1 {
		t.Fatalf("exit %d, want 1", rc)
	}
	if !strings.Contains(errb.String(), "parse") {
		t.Fatalf("stderr = %s", errb)
	}
}

func TestFleetAlertingApplySurfacesValidationError(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{setAlertingErr: errors.New(`policy "p" step 0: unknown channel "bogus"`)}
	startFleetDaemon(t, fake)
	path := filepath.Join(t.TempDir(), "alerting.json")
	body := `{"default_policy":"p","policies":[{"name":"p","steps":[{"after":"0s","channels":["bogus"]}]}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := Main([]string{"fleet", "alerting", "apply", path}); rc != 1 {
		t.Fatalf("exit %d, want 1", rc)
	}
	if !strings.Contains(errb.String(), "unknown channel") {
		t.Fatalf("stderr = %s", errb)
	}
}

// --- task 8: managed config CLI, read-only enforcement, fleet leave ------

func TestFleetManagedList(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{managed: []core.ManagedFragment{
		{ID: "f1", Tag: "web", Values: map[string]string{"thresholds.cpu_pct": "85"}, Version: 2, Author: "cli"},
		{ID: "f2", Values: map[string]string{"baseline_sigma": "3"}, Version: 1, Author: "alice"},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "managed", "list"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	got := out.String()
	for _, want := range []string{"f1", "web", "thresholds.cpu_pct=85", "f2", "*", "baseline_sigma=3", "alice"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestFleetManagedSetCreatesWhenNoExistingTag(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "managed", "set", "--tag", "web", "thresholds.cpu_pct=85"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.savedManaged.ID != "" {
		t.Fatalf("savedManaged.ID = %q, want empty (create) when no existing fragment carries the tag", fake.savedManaged.ID)
	}
	if fake.savedManaged.Tag != "web" || fake.savedManaged.Values["thresholds.cpu_pct"] != "85" || fake.savedManagedActor != "cli" {
		t.Fatalf("savedManaged = %+v actor=%q", fake.savedManaged, fake.savedManagedActor)
	}
	if !strings.Contains(out.String(), "mf123") {
		t.Fatalf("out = %s, want the assigned id", out)
	}
}

func TestFleetManagedSetUpdatesExistingTagFragment(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{managed: []core.ManagedFragment{
		{ID: "existing1", Tag: "web", Values: map[string]string{"thresholds.cpu_pct": "70"}, Version: 1},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "managed", "set", "--tag", "web", "thresholds.cpu_pct=90"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.savedManaged.ID != "existing1" {
		t.Fatalf("savedManaged.ID = %q, want existing1 (update, one fragment per tag)", fake.savedManaged.ID)
	}
}

func TestFleetManagedSetRejectsMalformedKV(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "managed", "set", "not-a-kv-pair"}); rc != 2 {
		t.Fatalf("exit %d, want 2: %s", rc, errb)
	}
}

func TestFleetManagedDelete(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "managed", "delete", "f1"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	if fake.deletedManagedID != "f1" || fake.deletedManagedActor != "cli" {
		t.Fatalf("deletedManagedID=%q actor=%q", fake.deletedManagedID, fake.deletedManagedActor)
	}
	if !strings.Contains(out.String(), "f1") {
		t.Fatalf("out = %s", out)
	}
}

func TestFleetManagedStatusShowsDriftAndConflicts(t *testing.T) {
	_, out, errb := fleetCLIEnv(t)
	fake := &fleetCLIFake{managedStatus: []core.ManagedStatus{
		{Node: "n1", Version: 1, Desired: 2, Applied: true, Drift: []string{"thresholds.cpu_pct"},
			Conflicts: []core.ManagedConflict{{Key: "thresholds.cpu_pct", Fragments: []string{"f1", "f2"}}}},
	}}
	startFleetDaemon(t, fake)
	if rc := Main([]string{"fleet", "managed", "status"}); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb)
	}
	got := out.String()
	for _, want := range []string{"n1", "thresholds.cpu_pct", "f1,f2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

// TestConfigSetRefusedOnManagedChild pins the read-only enforcement `config
// set`/`config unset` must show on a fleet child once a key is managed: the
// plain one-shot CLI reads the durable sidecar directly (no running daemon
// required), refuses with the fragment id, and never touches config.json.
func TestConfigSetRefusedOnManagedChild(t *testing.T) {
	dir, out, errb := fleetCLIEnv(t)
	if rc := Main([]string{"config", "set", "thresholds.cpu_pct", "70"}); rc != 0 {
		t.Fatalf("baseline set exit %d: %s", rc, errb)
	}
	before := loadTestCfg(t).Thresholds.CPUPct

	childDir := filepath.Join(dir, "fleet-child")
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sidecar := managedChildFileV1{Version: 1, Values: map[string]string{"thresholds.cpu_pct": "80"}, Fragments: map[string]string{"thresholds.cpu_pct": "frag789abc012"}}
	b, _ := json.Marshal(sidecar)
	if err := os.WriteFile(filepath.Join(childDir, "managed.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	if rc := Main([]string{"config", "set", "thresholds.cpu_pct", "50"}); rc != 1 {
		t.Fatalf("exit %d, want 1: out=%s err=%s", rc, out, errb)
	}
	if !strings.Contains(errb.String(), "managed by the fleet master") || !strings.Contains(errb.String(), "frag789abc012") {
		t.Fatalf("stderr = %s, want the managed-by-master refusal naming the fragment", errb)
	}
	if got := loadTestCfg(t).Thresholds.CPUPct; got != before {
		t.Fatalf("CPUPct = %v, want unchanged %v (refused before ever touching config.json)", got, before)
	}

	// config unset is refused the same way.
	errb.Reset()
	if rc := Main([]string{"config", "unset", "thresholds.cpu_pct"}); rc != 1 {
		t.Fatalf("unset exit %d, want 1: %s", rc, errb)
	}
	if !strings.Contains(errb.String(), "managed by the fleet master") {
		t.Fatalf("unset stderr = %s", errb)
	}

	// An unmanaged key is completely unaffected.
	out.Reset()
	errb.Reset()
	if rc := Main([]string{"config", "set", "thresholds.mem_pct", "77"}); rc != 0 {
		t.Fatalf("unmanaged key set exit %d: %s", rc, errb)
	}
	if got := loadTestCfg(t).Thresholds.MemPct; got != 77 {
		t.Fatalf("MemPct = %v, want 77", got)
	}
}

// TestFleetLeaveKeepsManagedValuesRemovesSidecar pins the task-8 ruling for
// `fleet leave`: the last managed values stay as ordinary local config (they
// already are -- leave never touches Thresholds/etc.), only the
// managed-config sidecar is removed, so this host stops enforcing them as
// read-only. Uses a plain (non --purge) leave so the rest of fleet-child
// survives, isolating the assertion to the sidecar alone.
func TestFleetLeaveKeepsManagedValuesRemovesSidecar(t *testing.T) {
	dir, _, errb := fleetCLIEnv(t)
	code := testMasterForCLI(t)
	if rc := Main([]string{"fleet", "join", code}); rc != 0 {
		t.Fatalf("join exit %d: %s", rc, errb)
	}

	// Simulate a managed value already applied and persisted (exactly what
	// managedChild.setApplied does in production): an ordinary config field
	// plus the sidecar recording it as managed.
	c := loadTestCfg(t)
	if err := c.Set("thresholds.cpu_pct", "77"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	childDir := filepath.Join(dir, "fleet-child")
	sidecar := managedChildFileV1{Version: 1, Values: map[string]string{"thresholds.cpu_pct": "77"}, Fragments: map[string]string{"thresholds.cpu_pct": "frag1"}}
	b, _ := json.Marshal(sidecar)
	if err := os.WriteFile(filepath.Join(childDir, "managed.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, managed := ManagedFragmentFor(dir, "thresholds.cpu_pct"); !managed {
		t.Fatal("sanity: sidecar should mark thresholds.cpu_pct managed before leave")
	}

	if rc := Main([]string{"fleet", "leave"}); rc != 0 {
		t.Fatalf("leave exit %d: %s", rc, errb)
	}

	if got := loadTestCfg(t).Thresholds.CPUPct; got != 77 {
		t.Fatalf("CPUPct after leave = %v, want unchanged 77 (kept as ordinary local config)", got)
	}
	if _, err := os.Stat(filepath.Join(childDir, "managed.json")); !os.IsNotExist(err) {
		t.Fatalf("managed.json sidecar should be removed by leave, stat err = %v", err)
	}
	if _, managed := ManagedFragmentFor(dir, "thresholds.cpu_pct"); managed {
		t.Fatal("thresholds.cpu_pct must no longer be reported managed after leave")
	}
	if _, err := os.Stat(childDir); err != nil {
		t.Fatalf("a plain (non --purge) leave must keep the rest of fleet-child: %v", err)
	}
}
