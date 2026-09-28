package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"time"

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

	// renamedActor/taggedActor/revokedActor/removedActor/deletedTokenActor
	// (plan C task C5, actor plumbing) record the actor the LAST
	// RenameNode/SetNodeTags/RevokeNode/RemoveNode/DeleteToken call was
	// made with, so a test can assert it was the signed-in web user's own
	// name (auditUser(r)), never a placeholder -- the same convention
	// ackedIncidentActor/expiredSilenceActor/deletedMaintenanceActor
	// already use for their own mutations.
	renamedActor      string
	taggedActor       string
	revokedActor      string
	removedActor      string
	deletedTokenActor string

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

	// incidents/incidentsErr (task C2, fleet incidents web UI) is Incidents'
	// fixture -- Incidents itself applies filter (State/Node/Tag/Limit)
	// in-memory, mirroring the real FleetAPI's own filtering contract closely
	// enough for this package's handler tests without duplicating the whole
	// engine. incidentsCalls/lastIncidentsFilter record every call, for the
	// nav badge's "at most one call per request" test.
	incidents           []core.Incident
	incidentsErr        error
	incidentsCalls      int
	lastIncidentsFilter core.IncidentFilter

	// incidentErr/ackIncidentErr let a test force Incident/AckIncident to
	// fail; ackedIncidentID/ackedIncidentActor record AckIncident's last
	// call, so a test can assert the ack recorded the SIGNED-IN web user
	// (Review Focus 4), not some placeholder.
	incidentErr        error
	ackIncidentErr     error
	ackedIncidentID    string
	ackedIncidentActor string

	// explainResults/explainErr back Explain(key): a map from alert key to
	// its own canned pipeline trail, or a shared error.
	explainResults map[string][]core.IncidentEvent
	explainErr     error

	// auditEntries/auditErr (task C5, fleet audit page) back Audit(limit) as
	// a settable fixture; lastAuditLimit records the limit the page passed,
	// so a test can pin task-5-brief.md's "Audit(limit=5000)" call.
	auditEntries   []core.AuditEntry
	auditErr       error
	lastAuditLimit int

	// createSilenceErr lets a test force CreateSilence to fail (e.g. a
	// validation rejection, rendered inline per global-constraints.md);
	// createdSilences records every silence actually created, so a test can
	// assert the silence-from-incident form's matchers/Author/window.
	createSilenceErr error
	createdSilences  []core.Silence

	// silences/maintenances (task C4, fleet silences + maintenance windows
	// web UI) back Silences()/Maintenances() as settable list fixtures --
	// CreateSilence/SaveMaintenance/ExpireSilence/DeleteMaintenance mutate
	// these SAME slices (mirroring the real silenceStore, fleet_silences.go)
	// so a create/expire/delete-then-list round trip through this fake
	// behaves like the real backend, not just a canned passthrough.
	// fakeValidateMatchers/fakeValidateMaintenance (below, same file) replicate
	// the real validateMatchers/validateMaintenance checks (empty matcher, bad
	// glob, missing name/weekday, bad HH:MM/TZ) closely enough for this
	// package's handler tests without duplicating the whole engine -- the same
	// convention Incidents' in-memory filtering above already uses.
	silences        []core.Silence
	maintenances    []core.Maintenance
	silencesErr     error
	maintenancesErr error

	// expireSilenceErr/saveMaintenanceErr/deleteMaintenanceErr let a test
	// force those three mutations to fail outright (a FleetAPI-level
	// rejection, as opposed to fakeValidateMatchers/fakeValidateMaintenance's
	// own validation, which CreateSilence/SaveMaintenance always apply).
	// expiredSilenceID/expiredSilenceActor and
	// deletedMaintenanceID/deletedMaintenanceActor record the last such call
	// so a test can assert the actor was the SIGNED-IN web user
	// (auditUser(r)), never a placeholder -- same ruling as AckIncident.
	expireSilenceErr        error
	expiredSilenceID        string
	expiredSilenceActor     string
	saveMaintenanceErr      error
	deleteMaintenanceErr    error
	deletedMaintenanceID    string
	deletedMaintenanceActor string

	// alertingCfg/alertingErr/setAlertingCfg/setAlertingActor/
	// setAlertingErr/setAlertingCalls (task C3, fleet alerting admin web UI)
	// back Alerting/SetAlerting -- see those methods' own docs.
	alertingCfg      core.AlertingConfig
	alertingErr      error
	setAlertingCfg   core.AlertingConfig
	setAlertingActor string
	setAlertingErr   error
	setAlertingCalls int

	// routeTestResult/routeTestErr/lastRouteTest/routeTestCalls back
	// RouteTest.
	routeTestResult core.RouteDecision
	routeTestErr    error
	lastRouteTest   core.TestAlert
	routeTestCalls  int

	// ruleStates/ruleStatesErr back RuleStates.
	ruleStates    []core.RuleState
	ruleStatesErr error

	// managed/managedErr (task C5, fleet managed-config web page) back
	// Managed() as a settable fixture SaveManaged/DeleteManaged also mutate
	// in place (see those methods' own doc, above); managedStatus/
	// managedStatusErr back ManagedStatus(). saveManagedErr/deleteManagedErr
	// let a test force a rejection; savedManagedActor/deletedManagedID/
	// deletedManagedActor record the last successful call.
	managed             []core.ManagedFragment
	managedErr          error
	saveManagedErr      error
	savedManagedActor   string
	deleteManagedErr    error
	deletedManagedID    string
	deletedManagedActor string
	managedStatus       []core.ManagedStatus
	managedStatusErr    error
}

func (f *fakeFleet) Status() (core.FleetStatus, error) { return f.status, f.statusErr }

func (f *fakeFleet) Nodes(core.NodeFilter) ([]core.NodeSummary, error) {
	return f.nodes, f.nodesErr
}

func (f *fakeFleet) RenameNode(id, name, actor string) error {
	f.renamedActor = actor
	if f.renameErr != nil {
		return f.renameErr
	}
	if f.renamed == nil {
		f.renamed = map[string]string{}
	}
	f.renamed[id] = name
	return nil
}

func (f *fakeFleet) SetNodeTags(id string, tags []string, actor string) error {
	f.taggedActor = actor
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

func (f *fakeFleet) RevokeNode(id, actor string) error {
	f.revokedActor = actor
	if f.revokeErr != nil {
		return f.revokeErr
	}
	f.revoked = append(f.revoked, id)
	return nil
}

func (f *fakeFleet) RemoveNode(id, actor string) error {
	f.removedActor = actor
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

func (f *fakeFleet) DeleteToken(id, actor string) error {
	f.deletedTokenActor = actor
	if f.deleteTokenErr != nil {
		return f.deleteTokenErr
	}
	f.deletedTokens = append(f.deletedTokens, id)
	return nil
}

// Incidents (task C2, fleet incidents web UI) applies filter's
// State/Node in-memory (Tag is left unfiltered -- core.Incident carries no
// tag of its own, and no test here exercises tag filtering; the real
// FleetAPI resolves it via each member's node, out of scope for this
// package's fake) so handlers_fleet_incidents_test.go can exercise the
// list's actual filtering/pagination/badge behavior, not just a canned
// passthrough. incidentsCalls/lastIncidentsFilter record every call, for the
// nav badge's "at most one call per request" test.
func (f *fakeFleet) Incidents(filter core.IncidentFilter) ([]core.Incident, error) {
	f.incidentsCalls++
	f.lastIncidentsFilter = filter
	if f.incidentsErr != nil {
		return nil, f.incidentsErr
	}
	out := make([]core.Incident, 0, len(f.incidents))
	for _, inc := range f.incidents {
		if filter.State != "" && inc.State != filter.State {
			continue
		}
		if filter.Node != "" {
			matched := false
			for _, a := range inc.Alerts {
				if a.Node == filter.Node || a.NodeName == filter.Node {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, inc)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

// Incident looks id up in f.incidents by ID; an unknown id (or incidentErr)
// mirrors the real FleetAPI's "not found" shape closely enough for this
// package's handlers, which treat ANY Incident() error identically (a plain
// 404 page, never a flash -- see renderIncidentNotFound).
func (f *fakeFleet) Incident(id string) (core.Incident, error) {
	if f.incidentErr != nil {
		return core.Incident{}, f.incidentErr
	}
	for _, inc := range f.incidents {
		if inc.ID == id {
			return inc, nil
		}
	}
	return core.Incident{}, fmt.Errorf("no such incident: %s: %w", id, core.ErrNotFound)
}

// AckIncident records id/actor (ackedIncidentID/ackedIncidentActor) so a
// test can assert the ack carried the SIGNED-IN web user's own name, not a
// daemon-side placeholder (Review Focus 4).
func (f *fakeFleet) AckIncident(id, actor string) error {
	if f.ackIncidentErr != nil {
		return f.ackIncidentErr
	}
	f.ackedIncidentID = id
	f.ackedIncidentActor = actor
	return nil
}

// Explain returns explainResults[key] (nil, not an error, for an unknown
// key -- exactly like a genuinely empty pipeline trail), or explainErr if
// set.
func (f *fakeFleet) Explain(key string) ([]core.IncidentEvent, error) {
	if f.explainErr != nil {
		return nil, f.explainErr
	}
	return f.explainResults[key], nil
}

func (f *fakeFleet) Audit(limit int) ([]core.AuditEntry, error) {
	f.lastAuditLimit = limit
	return f.auditEntries, f.auditErr
}

// Silences (task C4) returns the settable f.silences fixture verbatim -- a
// test that wants active/upcoming/expired tab coverage sets it directly
// (each entry's own Start/End decides which tab it lands on), exactly like
// f.incidents backs Incidents().
func (f *fakeFleet) Silences() ([]core.Silence, error) { return f.silences, f.silencesErr }

// CreateSilence (task C2, silence-from-incident; task C4, the silences page's
// own create form) validates via fakeValidateMatchers (empty matcher/bad
// glob) and end-after-start, mints a fake ID, and records the created
// silence into BOTH createdSilences (task C2's own assertions) and
// f.silences (task C4's list/tabs), so a create-then-list round trip through
// this fake behaves like the real backend. Fails outright with
// createSilenceErr when a test wants to pin the "validation errors render
// inline, never a 500" ruling without exercising fakeValidateMatchers itself.
func (f *fakeFleet) CreateSilence(s core.Silence) (core.Silence, error) {
	if f.createSilenceErr != nil {
		return core.Silence{}, f.createSilenceErr
	}
	if err := fakeValidateMatchers(s.Matchers); err != nil {
		return core.Silence{}, err
	}
	if s.End <= s.Start {
		return core.Silence{}, errors.New("a silence's end must be after its start")
	}
	s.ID = fmt.Sprintf("sil%d", len(f.createdSilences)+1)
	f.createdSilences = append(f.createdSilences, s)
	f.silences = append(f.silences, s)
	return s, nil
}

// ExpireSilence (task C4) pulls id's End back to now (mirroring the real
// silenceStore.Expire) and records actor/id, or fails with expireSilenceErr /
// "no such silence" for an unknown id.
func (f *fakeFleet) ExpireSilence(id, actor string) error {
	if f.expireSilenceErr != nil {
		return f.expireSilenceErr
	}
	for i := range f.silences {
		if f.silences[i].ID != id {
			continue
		}
		f.expiredSilenceID, f.expiredSilenceActor = id, actor
		now := time.Now().Unix()
		if f.silences[i].End > now {
			f.silences[i].End = now
		}
		return nil
	}
	return fmt.Errorf("no such silence %q: %w", id, core.ErrNotFound)
}

// Maintenances (task C4) returns the settable f.maintenances fixture
// verbatim.
func (f *fakeFleet) Maintenances() ([]core.Maintenance, error) {
	return f.maintenances, f.maintenancesErr
}

// SaveMaintenance (task C4) validates via fakeValidateMaintenance (name,
// weekdays, From/To HH:MM, TZ, plus fakeValidateMatchers' own matcher
// checks), then mints a fake ID (m.ID == "") or updates the existing window
// in place, mirroring the real silenceStore.SaveMaintenance. Fails outright
// with saveMaintenanceErr when a test wants to bypass fakeValidateMaintenance
// itself.
func (f *fakeFleet) SaveMaintenance(m core.Maintenance) (core.Maintenance, error) {
	if f.saveMaintenanceErr != nil {
		return core.Maintenance{}, f.saveMaintenanceErr
	}
	if err := fakeValidateMaintenance(m); err != nil {
		return core.Maintenance{}, err
	}
	if m.ID == "" {
		m.ID = fmt.Sprintf("mnt%d", len(f.maintenances)+1)
		f.maintenances = append(f.maintenances, m)
		return m, nil
	}
	for i, mm := range f.maintenances {
		if mm.ID == m.ID {
			f.maintenances[i] = m
			return m, nil
		}
	}
	return core.Maintenance{}, fmt.Errorf("no such maintenance %q: %w", m.ID, core.ErrNotFound)
}

// DeleteMaintenance (task C4) removes id and records actor, or fails with
// deleteMaintenanceErr / "no such maintenance" for an unknown id.
func (f *fakeFleet) DeleteMaintenance(id, actor string) error {
	if f.deleteMaintenanceErr != nil {
		return f.deleteMaintenanceErr
	}
	for i, m := range f.maintenances {
		if m.ID != id {
			continue
		}
		f.deletedMaintenanceID, f.deletedMaintenanceActor = id, actor
		f.maintenances = append(f.maintenances[:i], f.maintenances[i+1:]...)
		return nil
	}
	return fmt.Errorf("no such maintenance %q: %w", id, core.ErrNotFound)
}

// fakeValidateMatchers mirrors internal/trinetra/fleet_silences.go's own
// validateMatchers (same checks: an empty list or an entirely-empty Matcher
// is rejected, and -- task C4's brief -- a non-empty Node/Rule must be a
// syntactically valid path.Match glob) closely enough for this package's
// handler tests without importing that package (module graph is one-way,
// internal/trinetra -> internal/web, never back -- server.go's Deps doc).
func fakeValidateMatchers(ms []core.Matcher) error {
	if len(ms) == 0 {
		return errors.New("a silence must match something")
	}
	for _, m := range ms {
		if m.Empty() {
			return errors.New("a silence must match something")
		}
		if m.Node != "" {
			if _, err := path.Match(m.Node, ""); err != nil {
				return fmt.Errorf("invalid node glob %q", m.Node)
			}
		}
		if m.Rule != "" {
			if _, err := path.Match(m.Rule, ""); err != nil {
				return fmt.Errorf("invalid rule glob %q", m.Rule)
			}
		}
	}
	return nil
}

// fakeValidHHMM mirrors internal/trinetra's validHHMM/parseHHMM (same
// permissive Sscanf shape plus the 0-23/0-59 range check).
func fakeValidHHMM(s string) bool {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return false
	}
	return h >= 0 && h <= 23 && m >= 0 && m <= 59
}

// fakeValidateMaintenance mirrors internal/trinetra/fleet_silences.go's own
// validateMaintenance (name required, at least one weekday in 0..6, From/To
// valid HH:MM, TZ loadable) -- see fakeValidateMatchers' doc for why this is
// duplicated here rather than imported.
func fakeValidateMaintenance(m core.Maintenance) error {
	if err := fakeValidateMatchers(m.Matchers); err != nil {
		return err
	}
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("a maintenance window needs a name")
	}
	if len(m.Weekdays) == 0 {
		return errors.New("a maintenance window needs at least one weekday")
	}
	for _, wd := range m.Weekdays {
		if wd < 0 || wd > 6 {
			return fmt.Errorf("invalid weekday %d (want 0=Sunday..6=Saturday)", wd)
		}
	}
	if !fakeValidHHMM(m.From) {
		return fmt.Errorf("invalid --from %q (want HH:MM)", m.From)
	}
	if !fakeValidHHMM(m.To) {
		return fmt.Errorf("invalid --to %q (want HH:MM)", m.To)
	}
	if _, err := time.LoadLocation(m.TZ); err != nil {
		return fmt.Errorf("invalid --tz %q: %w", m.TZ, err)
	}
	return nil
}

// Alerting/SetAlerting (task C3, fleet alerting admin web UI):
// alertingCfg/alertingErr back Alerting() directly; SetAlerting records
// every call (setAlertingCfg/setAlertingActor/setAlertingCalls) so a test
// can assert the actor it received was the SIGNED-IN web user, then --
// mirroring the real alertingStore.Set exactly (internal/trinetra/
// fleet_routing.go) -- bumps alertingCfg's Version to alertingCfg.Version+1
// and stores the (now-canonical) result, so a save-then-reload round trip
// through this same fake sees the new version. setAlertingErr lets a test
// force a rejection (a plain validation error, or core.ErrConflict for the
// "stale Version" ruling) without touching alertingCfg at all.
func (f *fakeFleet) Alerting() (core.AlertingConfig, error) { return f.alertingCfg, f.alertingErr }

func (f *fakeFleet) SetAlerting(cfg core.AlertingConfig, actor string) error {
	f.setAlertingCalls++
	f.setAlertingCfg = cfg
	f.setAlertingActor = actor
	if f.setAlertingErr != nil {
		return f.setAlertingErr
	}
	cfg.Version = f.alertingCfg.Version + 1
	f.alertingCfg = cfg
	return nil
}

// RouteTest (task C3) records every call (lastRouteTest/routeTestCalls) and
// returns routeTestResult/routeTestErr -- a canned core.RouteDecision (with
// as many Policies as a test wants, for the "multiple matched policies via
// Continue fan-out" case) rather than actually resolving anything, since
// this fake carries no routing config of its own to resolve against.
func (f *fakeFleet) RouteTest(alert core.TestAlert) (core.RouteDecision, error) {
	f.routeTestCalls++
	f.lastRouteTest = alert
	return f.routeTestResult, f.routeTestErr
}

// RuleStates (task 7/C3): ruleStates/ruleStatesErr back GET
// /fleet/rules/state's fragment.
func (f *fakeFleet) RuleStates() ([]core.RuleState, error) { return f.ruleStates, f.ruleStatesErr }

// Managed/SaveManaged/DeleteManaged/ManagedStatus (task 8's fragment CRUD;
// plan C task C5's managed-config web page over it): f.managed is the
// settable fragment-list fixture; SaveManaged mimics the real
// managedFragmentStore.Save closely enough for handlers_fleet_managed_test.go
// to exercise the real behavior this package's handler builds on, not just a
// canned passthrough -- reject any key outside core.ManagedKeys (naming it,
// in the SAME wording the real backend uses, fleet_managed.go's
// validateManagedFragmentValues, so a test can't tell the two apart), then
// upsert by TAG when frag.ID is "" (an existing fragment with that same tag
// is updated in place, matching the real "upsert by tag" contract) or by ID
// otherwise, bumping Version and setting Author to actor. managedErr/
// saveManagedErr/deleteManagedErr let a test force a lookup/mutation
// rejection; savedManagedActor/deletedManagedID/deletedManagedActor record
// the last successful call so a test can assert the actor was the
// SIGNED-IN web user, never a placeholder -- the same convention this
// file's other mutation fakes use.
func (f *fakeFleet) Managed() ([]core.ManagedFragment, error) { return f.managed, f.managedErr }

func (f *fakeFleet) SaveManaged(frag core.ManagedFragment, actor string) (core.ManagedFragment, error) {
	if f.saveManagedErr != nil {
		return core.ManagedFragment{}, f.saveManagedErr
	}
	allowed := map[string]bool{}
	for _, k := range core.ManagedKeys {
		allowed[k] = true
	}
	for k := range frag.Values {
		if !allowed[k] {
			return core.ManagedFragment{}, fmt.Errorf("%q is not a managed-config key (allowed: %s)", k, strings.Join(core.ManagedKeys, ", "))
		}
	}
	f.savedManagedActor = actor
	frag.Author = actor
	if frag.ID == "" {
		for i, existing := range f.managed {
			if existing.Tag == frag.Tag {
				frag.ID = existing.ID
				frag.Version = existing.Version + 1
				f.managed[i] = frag
				return frag, nil
			}
		}
		frag.ID = fmt.Sprintf("frag%d", len(f.managed)+1)
		frag.Version = 1
		f.managed = append(f.managed, frag)
		return frag, nil
	}
	for i, existing := range f.managed {
		if existing.ID == frag.ID {
			frag.Version = existing.Version + 1
			f.managed[i] = frag
			return frag, nil
		}
	}
	return core.ManagedFragment{}, fmt.Errorf("no such managed-config fragment %q: %w", frag.ID, core.ErrNotFound)
}

func (f *fakeFleet) DeleteManaged(id, actor string) error {
	if f.deleteManagedErr != nil {
		return f.deleteManagedErr
	}
	for i, existing := range f.managed {
		if existing.ID == id {
			f.managed = append(f.managed[:i], f.managed[i+1:]...)
			f.deletedManagedID, f.deletedManagedActor = id, actor
			return nil
		}
	}
	return fmt.Errorf("no such managed-config fragment %q: %w", id, core.ErrNotFound)
}

func (f *fakeFleet) ManagedStatus() ([]core.ManagedStatus, error) {
	return f.managedStatus, f.managedStatusErr
}

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
	// snapshotCalls/activeAlertsCalls (round-2 review finding I2) are
	// per-method counters alongside the aggregate calls above, so a test can
	// pin "at most one Snapshot()/ActiveAlerts() round trip per request" --
	// the exact redundancy the request-memo extension fixes -- without that
	// assertion being diluted by every OTHER core.API call a page also
	// happens to make (e.g. coreVersionViaAPI's Version()).
	snapshotCalls     *int
	activeAlertsCalls *int
}

// newCountingAPI wraps api with a fresh, zeroed call counter.
func newCountingAPI(api core.API) *countingAPI {
	n, s, a := 0, 0, 0
	return &countingAPI{api: api, calls: &n, snapshotCalls: &s, activeAlertsCalls: &a}
}

// count returns how many core.API calls have gone through this wrapper so
// far.
func (c *countingAPI) count() int { return *c.calls }

// snapshotCallCount returns how many Snapshot() calls have gone through this
// wrapper so far.
func (c *countingAPI) snapshotCallCount() int { return *c.snapshotCalls }

// activeAlertsCallCount returns how many ActiveAlerts() calls have gone
// through this wrapper so far.
func (c *countingAPI) activeAlertsCallCount() int { return *c.activeAlertsCalls }

func (c *countingAPI) Snapshot() (core.DashboardView, error) {
	*c.calls++
	*c.snapshotCalls++
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
	*c.activeAlertsCalls++
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
