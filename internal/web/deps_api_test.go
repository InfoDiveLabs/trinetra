package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fakeAPI is a minimal core.API test double for this package's handler
// tests: each read field backs exactly one read method's return value (the
// zero value/nil error when unset). The write methods (task 8) delegate to
// an optional func field each -- applyConfig/testChannel/ackAlert/
// unackAlert -- so a test that cares (configTestDeps wiring applyConfig to
// the same cfg-mutating closure Reload uses, or
// TestChannelsTestHandlerCallsTestChannel wiring testChannel to capture its
// argument) can observe the call, while every other test gets a harmless
// no-op (nil error) by leaving the field unset. Doctor/Subscribe stay plain
// no-op stubs -- no handler in this package calls them yet.
type fakeAPI struct {
	snap       core.DashboardView
	snapErr    error
	monitoring core.MonitoringView
	monErr     error
	// series maps a metric name straight to the points Series should return
	// for it (ignoring from/to/res unless seriesErr is set), mirroring the
	// now-removed fakeSeriesStore's shape.
	series        map[string][]core.SeriesPoint
	seriesErr     error
	events        []core.DownEventView
	eventsErr     error
	active        []core.AlertRecord
	activeErr     error
	history       []core.AlertRecord
	histErr       error
	hostInfo      core.HostInfoView
	containerLogs string
	logErr        error
	version       string

	applyConfig func(*config.Config) error
	testChannel func(name string) error
	ackAlert    func(key string) error
	unackAlert  func(key string) error

	// fleet and nodes are optional fleet-routing fixtures (fleet-web-a task
	// 1, node_scope_test.go): setting fleet makes Fleet() return it (nil ->
	// Fleet() returns a literal nil core.FleetAPI, mirroring "no fleet
	// support"), and nodes maps a node id straight to the core.API a test
	// wants apiFor to resolve to for it (mirroring control.Client.ForNode's
	// shape) -- neither is read by any handler test that predates fleet
	// routing.
	fleet *fakeFleet
	nodes map[string]core.API
}

func (f fakeAPI) Snapshot() (core.DashboardView, error)    { return f.snap, f.snapErr }
func (f fakeAPI) Monitoring() (core.MonitoringView, error) { return f.monitoring, f.monErr }

func (f fakeAPI) Series(metric string, from, to int64, res core.Resolution) ([]core.SeriesPoint, error) {
	if f.seriesErr != nil {
		return nil, f.seriesErr
	}
	return f.series[metric], nil
}

func (f fakeAPI) Events(from, to int64) ([]core.DownEventView, error) {
	return f.events, f.eventsErr
}

func (f fakeAPI) ActiveAlerts() ([]core.AlertRecord, error) { return f.active, f.activeErr }

func (f fakeAPI) AlertHistory(since int64, limit int) ([]core.AlertRecord, error) {
	return f.history, f.histErr
}

func (f fakeAPI) Config() (*config.Config, error)      { return nil, nil }
func (f fakeAPI) Doctor() (core.DoctorReport, error)   { return core.DoctorReport{}, nil }
func (f fakeAPI) HostInfo() (core.HostInfoView, error) { return f.hostInfo, nil }
func (f fakeAPI) ContainerLogs(name string, lines int) (string, error) {
	return f.containerLogs, f.logErr
}
func (f fakeAPI) Version() (string, error)                                { return f.version, nil }
func (f fakeAPI) EnrollmentPIN(ctx context.Context) (string, bool, error) { return "", false, nil }
func (f fakeAPI) MonitorTargets(ctx context.Context) ([]core.TargetView, error) {
	return nil, nil
}

func (f fakeAPI) ApplyConfig(c *config.Config) error {
	if f.applyConfig != nil {
		return f.applyConfig(c)
	}
	return nil
}

func (f fakeAPI) AckAlert(key string) error {
	if f.ackAlert != nil {
		return f.ackAlert(key)
	}
	return nil
}

func (f fakeAPI) UnackAlert(key string) error {
	if f.unackAlert != nil {
		return f.unackAlert(key)
	}
	return nil
}

func (f fakeAPI) TestChannel(name string) error {
	if f.testChannel != nil {
		return f.testChannel(name)
	}
	return nil
}

// ValidateChannel is a plain no-op stub: this fakeAPI backs Deps.API (the
// core.API boundary), which is distinct from Deps.ValidateChannel (the
// local buildNotifier dry-run func field the #79 channel-editor tests
// exercise directly, see handlers_channels_test.go's undeliverableTelegram)
// -- no handler in this package's tests calls core.API.ValidateChannel
// itself.
func (f fakeAPI) ValidateChannel(cc config.ChannelConfig) error { return nil }

func (f fakeAPI) Subscribe(ctx context.Context) (<-chan core.Event, error) { return nil, nil }

// Fleet and Node make fakeAPI satisfy core.FleetProvider (fleet-web-a task
// 1), mirroring control.Client's own split: Fleet() is always this fake's
// canned f.fleet (a literal nil core.FleetAPI when unset, not a typed-nil
// *fakeFleet, so callers' `d.Fleet() == nil`-shaped checks behave like the
// real "no fleet support" case); Node(id) mirrors
// (*control.Client).Node/ForNode -- "self"/"" resolves to this fake itself,
// any id present in f.nodes resolves to that fixture, anything else is
// core.ErrNoSuchNode.
func (f fakeAPI) Fleet() core.FleetAPI {
	if f.fleet == nil {
		return nil
	}
	return f.fleet
}

func (f fakeAPI) Node(id string) (core.API, error) {
	if id == "" || id == core.SelfNodeID {
		return f, nil
	}
	if api, ok := f.nodes[id]; ok {
		return api, nil
	}
	return nil, core.ErrNoSuchNode
}

var _ core.FleetProvider = fakeAPI{}

// fakeFleet is a minimal core.FleetAPI test double: Status/Nodes return
// canned fixtures (plus canned errors, for the "old daemon" and
// roster-lookup-fails cases node_scope_test.go exercises); the
// fleet-master mutation methods record what they were called with so a
// later task's admin-page tests can assert against them, defaulting to a
// harmless no-op/zero-value response.
type fakeFleet struct {
	status    core.FleetStatus
	statusErr error
	nodes     []core.NodeSummary
	nodesErr  error

	renamed        map[string]string
	tagged         map[string][]string
	revoked        []string
	removed        []string
	tokens         []core.TokenView
	tokensErr      error
	createdToken   core.CreatedToken
	createTokenErr error
	deletedTokens  []string
}

func (f *fakeFleet) Status() (core.FleetStatus, error) { return f.status, f.statusErr }

func (f *fakeFleet) Nodes(core.NodeFilter) ([]core.NodeSummary, error) {
	return f.nodes, f.nodesErr
}

func (f *fakeFleet) RenameNode(id, name string) error {
	if f.renamed == nil {
		f.renamed = map[string]string{}
	}
	f.renamed[id] = name
	return nil
}

func (f *fakeFleet) SetNodeTags(id string, tags []string) error {
	if f.tagged == nil {
		f.tagged = map[string][]string{}
	}
	f.tagged[id] = tags
	return nil
}

func (f *fakeFleet) RevokeNode(id string) error {
	f.revoked = append(f.revoked, id)
	return nil
}

func (f *fakeFleet) RemoveNode(id string) error {
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeFleet) Tokens() ([]core.TokenView, error) { return f.tokens, f.tokensErr }

func (f *fakeFleet) CreateToken(core.TokenSpec) (core.CreatedToken, error) {
	return f.createdToken, f.createTokenErr
}

func (f *fakeFleet) DeleteToken(id string) error {
	f.deletedTokens = append(f.deletedTokens, id)
	return nil
}

var _ core.FleetAPI = (*fakeFleet)(nil)

// TestDashboardReadsFromAPI pins the core TDD obligation for this task: once
// Deps.API is set, GET / renders the fake API's Snapshot() data rather than
// Deps.Snapshot(); the dashboard handler must be reading state through
// core.API, not the pre-Task-5 closures.
func TestDashboardReadsFromAPI(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{snap: core.DashboardView{CPU: 77}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "77") {
		t.Errorf("dashboard body missing fake API CPU value 77:\n%s", rr.Body.String())
	}
}
