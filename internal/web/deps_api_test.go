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

	// renameErr/tagsErr/revokeErr/removeErr/deleteTokenErr (Task 7, fleet
	// admin) let a test force a mutation method to fail -- e.g. a bogus
	// node id or a daemon that stopped being a master mid-request -- so
	// handlers_fleet_admin_test.go can pin the "FleetAPI errors render as a
	// flash, never a 500" ruling. nil (the zero value) preserves every
	// existing caller's assumption that these methods always succeed.
	renameErr      error
	tagsErr        error
	revokeErr      error
	removeErr      error
	deleteTokenErr error

	// series/seriesErr (task C1b, fleet compare) let a test control
	// FleetSeries' result/error; seriesCalls/lastSeries* record what it was
	// called with, so handlers_fleet_compare_test.go can assert the compare
	// page makes exactly one FleetSeries call per request.
	series           []core.FleetSeriesPoint
	seriesErr        error
	seriesCalls      int
	lastSeriesMetric string
	lastSeriesFilter core.NodeFilter
	lastSeriesAgg    core.Agg
}

func (f *fakeFleet) Status() (core.FleetStatus, error) { return f.status, f.statusErr }

func (f *fakeFleet) Nodes(core.NodeFilter) ([]core.NodeSummary, error) {
	return f.nodes, f.nodesErr
}

func (f *fakeFleet) RenameNode(id, name string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	if f.renamed == nil {
		f.renamed = map[string]string{}
	}
	f.renamed[id] = name
	return nil
}

func (f *fakeFleet) SetNodeTags(id string, tags []string) error {
	if f.tagsErr != nil {
		return f.tagsErr
	}
	if f.tagged == nil {
		f.tagged = map[string][]string{}
	}
	f.tagged[id] = tags
	return nil
}

func (f *fakeFleet) SetNodeDeps(id string, deps []string, actor string) error {
	return nil
}

func (f *fakeFleet) RevokeNode(id string) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	f.revoked = append(f.revoked, id)
	return nil
}

func (f *fakeFleet) RemoveNode(id string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeFleet) Tokens() ([]core.TokenView, error) { return f.tokens, f.tokensErr }

func (f *fakeFleet) CreateToken(core.TokenSpec) (core.CreatedToken, error) {
	return f.createdToken, f.createTokenErr
}

func (f *fakeFleet) DeleteToken(id string) error {
	if f.deleteTokenErr != nil {
		return f.deleteTokenErr
	}
	f.deletedTokens = append(f.deletedTokens, id)
	return nil
}

// Incidents/Incident/AckIncident/Explain/Audit: not yet exercised by any web
// test (fleet phase 2's web surface for incidents lands in a later task);
// these stubs exist only so fakeFleet keeps satisfying core.FleetAPI.
func (f *fakeFleet) Incidents(core.IncidentFilter) ([]core.Incident, error) { return nil, nil }
func (f *fakeFleet) Incident(string) (core.Incident, error)                 { return core.Incident{}, nil }
func (f *fakeFleet) AckIncident(string, string) error                       { return nil }
func (f *fakeFleet) Explain(string) ([]core.IncidentEvent, error)           { return nil, nil }
func (f *fakeFleet) Audit(int) ([]core.AuditEntry, error)                   { return nil, nil }

// Silences/CreateSilence/ExpireSilence/Maintenances/SaveMaintenance/
// DeleteMaintenance: task 4's silences/maintenance windows have no web
// surface yet; these stubs exist only so fakeFleet keeps satisfying
// core.FleetAPI.
func (f *fakeFleet) Silences() ([]core.Silence, error)                            { return nil, nil }
func (f *fakeFleet) CreateSilence(s core.Silence) (core.Silence, error)           { return s, nil }
func (f *fakeFleet) ExpireSilence(string, string) error                           { return nil }
func (f *fakeFleet) Maintenances() ([]core.Maintenance, error)                    { return nil, nil }
func (f *fakeFleet) SaveMaintenance(m core.Maintenance) (core.Maintenance, error) { return m, nil }
func (f *fakeFleet) DeleteMaintenance(string, string) error                       { return nil }

// Alerting/SetAlerting/RouteTest: task 5's routing/escalation config has no
// web surface yet (plan C); these stubs exist only so fakeFleet keeps
// satisfying core.FleetAPI.
func (f *fakeFleet) Alerting() (core.AlertingConfig, error)        { return core.AlertingConfig{}, nil }
func (f *fakeFleet) SetAlerting(core.AlertingConfig, string) error { return nil }
func (f *fakeFleet) RouteTest(core.TestAlert) (core.RouteDecision, error) {
	return core.RouteDecision{}, nil
}

// RuleStates: task 7's aggregate rules have no web surface yet; this stub
// exists only so fakeFleet keeps satisfying core.FleetAPI.
func (f *fakeFleet) RuleStates() ([]core.RuleState, error) { return nil, nil }

// Managed/SaveManaged/DeleteManaged/ManagedStatus: task 8's managed-config
// fragment CRUD has no admin web surface yet -- the read-only enforcement
// this task DOES add to the config page goes through Status().Link.Managed
// (see fakeFleet.status), not these; these stubs exist only so fakeFleet
// keeps satisfying core.FleetAPI.
func (f *fakeFleet) Managed() ([]core.ManagedFragment, error) { return nil, nil }
func (f *fakeFleet) SaveManaged(frag core.ManagedFragment, actor string) (core.ManagedFragment, error) {
	return frag, nil
}
func (f *fakeFleet) DeleteManaged(string, string) error           { return nil }
func (f *fakeFleet) ManagedStatus() ([]core.ManagedStatus, error) { return nil, nil }

// FleetSeries (task C1b, fleet compare) records every call for
// handlers_fleet_compare_test.go's "single FleetSeries call" assertions.
func (f *fakeFleet) FleetSeries(metric string, filter core.NodeFilter, agg core.Agg, from, to int64, res core.Resolution) ([]core.FleetSeriesPoint, error) {
	f.seriesCalls++
	f.lastSeriesMetric, f.lastSeriesFilter, f.lastSeriesAgg = metric, filter, agg
	return f.series, f.seriesErr
}

var _ core.FleetAPI = (*fakeFleet)(nil)

// countingAPI wraps a core.API and counts every call made through it, so a
// test can assert a request never touched the underlying (fake) daemon API
// at all -- the property node_scope_test.go's round-1-review tests pin:
// withNodeRouter must reject/redirect an anonymous or non-master /n/...
// request before any handler (and therefore before newPageData/
// renderNotFound, which call ActiveAlerts/Version) ever calls into
// core.API. Every core.API method increments the shared counter, then
// delegates to the wrapped fake -- so count() reflects real usage, not just
// the couple of methods newPageData happens to call today.
type countingAPI struct {
	api   core.API
	calls *int
}

// newCountingAPI wraps api with a fresh, zeroed call counter.
func newCountingAPI(api core.API) *countingAPI {
	n := 0
	return &countingAPI{api: api, calls: &n}
}

// count returns how many core.API calls have gone through this wrapper so
// far.
func (c *countingAPI) count() int { return *c.calls }

func (c *countingAPI) Snapshot() (core.DashboardView, error) {
	*c.calls++
	return c.api.Snapshot()
}

func (c *countingAPI) Monitoring() (core.MonitoringView, error) {
	*c.calls++
	return c.api.Monitoring()
}

func (c *countingAPI) Series(metric string, from, to int64, res core.Resolution) ([]core.SeriesPoint, error) {
	*c.calls++
	return c.api.Series(metric, from, to, res)
}

func (c *countingAPI) Events(from, to int64) ([]core.DownEventView, error) {
	*c.calls++
	return c.api.Events(from, to)
}

func (c *countingAPI) ActiveAlerts() ([]core.AlertRecord, error) {
	*c.calls++
	return c.api.ActiveAlerts()
}

func (c *countingAPI) AlertHistory(since int64, limit int) ([]core.AlertRecord, error) {
	*c.calls++
	return c.api.AlertHistory(since, limit)
}

func (c *countingAPI) Config() (*config.Config, error) {
	*c.calls++
	return c.api.Config()
}

func (c *countingAPI) Doctor() (core.DoctorReport, error) {
	*c.calls++
	return c.api.Doctor()
}

func (c *countingAPI) HostInfo() (core.HostInfoView, error) {
	*c.calls++
	return c.api.HostInfo()
}

func (c *countingAPI) Version() (string, error) {
	*c.calls++
	return c.api.Version()
}

func (c *countingAPI) ContainerLogs(name string, lines int) (string, error) {
	*c.calls++
	return c.api.ContainerLogs(name, lines)
}

func (c *countingAPI) EnrollmentPIN(ctx context.Context) (string, bool, error) {
	*c.calls++
	return c.api.EnrollmentPIN(ctx)
}

func (c *countingAPI) MonitorTargets(ctx context.Context) ([]core.TargetView, error) {
	*c.calls++
	return c.api.MonitorTargets(ctx)
}

func (c *countingAPI) ApplyConfig(cfg *config.Config) error {
	*c.calls++
	return c.api.ApplyConfig(cfg)
}

func (c *countingAPI) AckAlert(key string) error {
	*c.calls++
	return c.api.AckAlert(key)
}

func (c *countingAPI) UnackAlert(key string) error {
	*c.calls++
	return c.api.UnackAlert(key)
}

func (c *countingAPI) TestChannel(name string) error {
	*c.calls++
	return c.api.TestChannel(name)
}

func (c *countingAPI) ValidateChannel(cc config.ChannelConfig) error {
	*c.calls++
	return c.api.ValidateChannel(cc)
}

func (c *countingAPI) Subscribe(ctx context.Context) (<-chan core.Event, error) {
	*c.calls++
	return c.api.Subscribe(ctx)
}

var _ core.API = (*countingAPI)(nil)

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
