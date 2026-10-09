package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fakeAPI is a minimal core.API test double for this package's handler tests: each read
// field backs exactly one read method's return value (the zero value/nil error when unset).
type fakeAPI struct {
	snap       core.DashboardView
	snapErr    error
	monitoring core.MonitoringView
	monErr     error
	// series maps a metric name straight to the points Series should return for it (ignoring
	// from/to/res unless seriesErr is set), mirroring the now-removed fakeSeriesStore's shape.
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

	// updateStatus/updateStatusErr back UpdateStatus; updateCheck/updateCheckErr do the same
	// for UpdateCheck.
	updateStatus    core.UpdateStatusView
	updateStatusErr error
	updateCheckErr  error
	// updateApply/updateRollback let a test capture the call (mirroring
	// applyConfig/testChannel's optional-func-field shape below); nil means a harmless no-op.
	updateApply    func(ctx context.Context, version string) error
	updateRollback func() error

	applyConfig func(*config.Config) error
	testChannel func(name string) error
	ackAlert    func(key string) error
	unackAlert  func(key string) error

	// fleet and nodes are optional fleet-routing fixtures: setting fleet makes Fleet() return
	// it (nil -> Fleet() returns a literal nil core.FleetAPI, mirroring "no fleet support").
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

func (f fakeAPI) UpdateStatus() (core.UpdateStatusView, error) {
	return f.updateStatus, f.updateStatusErr
}

func (f fakeAPI) UpdateCheck(ctx context.Context) (core.UpdateStatusView, error) {
	return f.updateStatus, f.updateCheckErr
}

func (f fakeAPI) UpdateApply(ctx context.Context, version string) error {
	if f.updateApply != nil {
		return f.updateApply(ctx, version)
	}
	return nil
}

func (f fakeAPI) UpdateRollback() error {
	if f.updateRollback != nil {
		return f.updateRollback()
	}
	return nil
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

// ValidateChannel is a plain no-op stub: this fakeAPI backs Deps.API (the core.API
// boundary), which is distinct from Deps.ValidateChannel.
func (f fakeAPI) ValidateChannel(cc config.ChannelConfig) error { return nil }

func (f fakeAPI) Subscribe(ctx context.Context) (<-chan core.Event, error) { return nil, nil }

// Fleet and Node make fakeAPI satisfy core.FleetProvider, mirroring control.Client's own
// split: Fleet() is always this fake's canned f.fleet.
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

// fakeFleet is a minimal core.FleetAPI test double: Status/Nodes return canned fixtures.
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

	// renamedActor/taggedActor/revokedActor/removedActor/deletedTokenActor record the actor.
	renamedActor      string
	taggedActor       string
	revokedActor      string
	removedActor      string
	deletedTokenActor string

	// renameErr/tagsErr/revokeErr/removeErr/deleteTokenErr let a test force a mutation method
	// to fail -- e.g. a bogus node id or a daemon that stopped being a master mid-request.
	renameErr      error
	tagsErr        error
	revokeErr      error
	removeErr      error
	deleteTokenErr error

	// series/seriesErr let a test control FleetSeries' result/error.
	series           []core.FleetSeriesPoint
	seriesErr        error
	seriesCalls      int
	lastSeriesMetric string
	lastSeriesFilter core.NodeFilter
	lastSeriesAgg    core.Agg

	// incidents/incidentsErr is Incidents' fixture -- Incidents itself applies filter
	// (State/Node/Tag/Limit) in-memory.
	incidents           []core.Incident
	incidentsErr        error
	incidentsCalls      int
	lastIncidentsFilter core.IncidentFilter

	// incidentErr/ackIncidentErr let a test force Incident/AckIncident to fail;
	// ackedIncidentID/ackedIncidentActor record AckIncident's last call.
	incidentErr        error
	ackIncidentErr     error
	ackedIncidentID    string
	ackedIncidentActor string

	// explainResults/explainErr back Explain(key): a map from alert key to
	// its own canned pipeline trail, or a shared error.
	explainResults map[string][]core.IncidentEvent
	explainErr     error

	// auditEntries/auditErr back Audit(limit) as a settable fixture; lastAuditLimit records
	// the limit the page passed, so a test can pin the "Audit(limit=5000)" call.
	auditEntries   []core.AuditEntry
	auditErr       error
	lastAuditLimit int

	// createSilenceErr lets a test force CreateSilence to fail (e.g. a validation rejection,
	// rendered inline per global-constraints.md).
	createSilenceErr error
	createdSilences  []core.Silence

	// silences/maintenances back Silences()/Maintenances() as settable list fixtures --
	// CreateSilence/SaveMaintenance/ExpireSilence/DeleteMaintenance mutate these SAME slices.
	silences        []core.Silence
	maintenances    []core.Maintenance
	silencesErr     error
	maintenancesErr error

	// expireSilenceErr/saveMaintenanceErr/deleteMaintenanceErr let a test force those three
	// mutations to fail outright.
	expireSilenceErr        error
	expiredSilenceID        string
	expiredSilenceActor     string
	saveMaintenanceErr      error
	deleteMaintenanceErr    error
	deletedMaintenanceID    string
	deletedMaintenanceActor string

	// alertingCfg/alertingErr/setAlertingCfg/setAlertingActor/setAlertingErr/setAlertingCalls
	// back Alerting/SetAlerting -- see those methods' own docs.
	alertingCfg      core.AlertingConfig
	alertingErr      error
	setAlertingCfg   core.AlertingConfig
	setAlertingActor string
	setAlertingErr   error
	setAlertingCalls int

	// routeTestResult/routeTestErr/lastRouteTest/routeTestCalls back RouteTest.
	routeTestResult core.RouteDecision
	routeTestErr    error
	lastRouteTest   core.TestAlert
	routeTestCalls  int

	// ruleStates/ruleStatesErr back RuleStates.
	ruleStates    []core.RuleState
	ruleStatesErr error

	// managed/managedErr back Managed() as a settable fixture SaveManaged/DeleteManaged also
	// mutate in place (see those methods' own doc, above).
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

// Incidents applies filter's State/Node in-memory.
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

// Incident looks id up in f.incidents by ID; an unknown id (or incidentErr) mirrors the
// real FleetAPI's "not found" shape closely enough for this package's handlers.
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

// AckIncident records id/actor (ackedIncidentID/ackedIncidentActor) so a test can assert
// the ack carried the SIGNED-IN web user's own name, not a daemon-side placeholder.
func (f *fakeFleet) AckIncident(id, actor string) error {
	if f.ackIncidentErr != nil {
		return f.ackIncidentErr
	}
	f.ackedIncidentID = id
	f.ackedIncidentActor = actor
	return nil
}

// Explain returns explainResults[key] (nil, not an error, for an unknown key -- exactly
// like a genuinely empty pipeline trail), or explainErr if set.
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

// Silences returns the settable f.silences fixture verbatim -- a test that wants
// active/upcoming/expired tab coverage sets it directly.
func (f *fakeFleet) Silences() ([]core.Silence, error) { return f.silences, f.silencesErr }

// CreateSilence validates via fakeValidateMatchers (empty matcher/bad glob) and
// end-after-start, mints a fake ID.
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

// ExpireSilence pulls id's End back to now (mirroring the real silenceStore.Expire) and
// records actor/id, or fails with expireSilenceErr /"no such silence" for an unknown id.
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

// Maintenances returns the settable f.maintenances fixture verbatim.
func (f *fakeFleet) Maintenances() ([]core.Maintenance, error) {
	return f.maintenances, f.maintenancesErr
}

// SaveMaintenance validates via fakeValidateMaintenance (name, weekdays, From/To HH:MM, TZ,
// plus fakeValidateMatchers' own matcher checks), then mints a fake ID.
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

// DeleteMaintenance removes id and records actor, or fails with
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

// fakeValidateMatchers mirrors internal/trinetra/fleet_silences.go's own validateMatchers
// closely enough for this package's handler tests without importing that package.
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
// validateMaintenance.
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

// Alerting/SetAlerting: alertingCfg/alertingErr back Alerting() directly; SetAlerting
// records every call.
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

// RouteTest records every call (lastRouteTest/routeTestCalls) and returns
// routeTestResult/routeTestErr -- a canned core.RouteDecision.
func (f *fakeFleet) RouteTest(alert core.TestAlert) (core.RouteDecision, error) {
	f.routeTestCalls++
	f.lastRouteTest = alert
	return f.routeTestResult, f.routeTestErr
}

// RuleStates: ruleStates/ruleStatesErr back GET /fleet/rules/state's fragment.
func (f *fakeFleet) RuleStates() ([]core.RuleState, error) { return f.ruleStates, f.ruleStatesErr }

// Managed/SaveManaged/DeleteManaged/ManagedStatus: f.managed is the settable fragment-list
// fixture.
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

// FleetSeries records every call for handlers_fleet_compare_test.go's "single
// FleetSeries call" assertions.
func (f *fakeFleet) FleetSeries(metric string, filter core.NodeFilter, agg core.Agg, from, to int64, res core.Resolution) ([]core.FleetSeriesPoint, error) {
	f.seriesCalls++
	f.lastSeriesMetric, f.lastSeriesFilter, f.lastSeriesAgg = metric, filter, agg
	return f.series, f.seriesErr
}

var _ core.FleetAPI = (*fakeFleet)(nil)

// countingAPI wraps a core.API and counts every call made through it, so a test can assert
// a request never touched the underlying (fake) daemon API at all.
type countingAPI struct {
	api   core.API
	calls *int
	// snapshotCalls/activeAlertsCalls are per-method counters alongside the aggregate calls
	// above, so a test can pin "at most one Snapshot()/ActiveAlerts() round trip per request".
	snapshotCalls     *int
	activeAlertsCalls *int
}

// newCountingAPI wraps api with a fresh, zeroed call counter.
func newCountingAPI(api core.API) *countingAPI {
	n, s, a := 0, 0, 0
	return &countingAPI{api: api, calls: &n, snapshotCalls: &s, activeAlertsCalls: &a}
}

// count returns how many core.API calls have gone through this wrapper so far.
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

func (c *countingAPI) UpdateStatus() (core.UpdateStatusView, error) {
	*c.calls++
	return c.api.UpdateStatus()
}

func (c *countingAPI) UpdateCheck(ctx context.Context) (core.UpdateStatusView, error) {
	*c.calls++
	return c.api.UpdateCheck(ctx)
}

func (c *countingAPI) UpdateApply(ctx context.Context, version string) error {
	*c.calls++
	return c.api.UpdateApply(ctx, version)
}

func (c *countingAPI) UpdateRollback() error {
	*c.calls++
	return c.api.UpdateRollback()
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

// TestDashboardReadsFromAPI pins that once Deps.API is set, GET / renders the fake API's
// Snapshot() data rather than Deps.Snapshot().
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

// fakeStatusPage is an in-memory core.StatusPageAPI for web tests.
type fakeStatusPage struct {
	mu      sync.Mutex
	svcs    []core.StatusService
	evals   []core.ServiceEvaluation
	incs    []core.StatusIncident
	pub     core.PublicStatus
	err     error // returned by every call when set (e.g. core.ErrStatusPageOnChild)
	actions []string
}

var _ core.StatusPageAPI = (*fakeStatusPage)(nil)

func (f *fakeStatusPage) Services() ([]core.StatusService, error) { return f.svcs, f.err }
func (f *fakeStatusPage) SetService(s core.StatusService, actor string) (core.StatusService, error) {
	if f.err != nil {
		return s, f.err
	}
	if err := core.ValidateStatusService(s); err != nil {
		return s, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, "set:"+s.ID+":"+actor)
	for i := range f.svcs {
		if f.svcs[i].ID == s.ID {
			f.svcs[i] = s
			return s, nil
		}
	}
	f.svcs = append(f.svcs, s)
	return s, nil
}
func (f *fakeStatusPage) DeleteService(id, actor string) error {
	f.actions = append(f.actions, "delsvc:"+id+":"+actor)
	return f.err
}
func (f *fakeStatusPage) Evaluation() ([]core.ServiceEvaluation, error)     { return f.evals, f.err }
func (f *fakeStatusPage) Incidents(all bool) ([]core.StatusIncident, error) { return f.incs, f.err }
func (f *fakeStatusPage) Incident(id string) (core.StatusIncident, error) {
	for _, inc := range f.incs {
		if inc.ID == id {
			return inc, f.err
		}
	}
	return core.StatusIncident{}, fmt.Errorf("no such incident: %w", core.ErrNotFound)
}
func (f *fakeStatusPage) CreateIncident(in core.NewIncident, actor string) (core.StatusIncident, error) {
	if err := core.ValidateNewIncident(in); err != nil {
		return core.StatusIncident{}, err
	}
	inc := core.StatusIncident{ID: fmt.Sprintf("inc%d", len(f.incs)+1), Title: in.Title, Impact: in.Impact, Services: in.Services, Status: in.Update.Status,
		Updates: []core.IncidentUpdate{{ID: "u1", Status: in.Update.Status, Message: in.Update.Message, Author: actor}}}
	f.incs = append(f.incs, inc)
	f.actions = append(f.actions, "create:"+actor)
	return inc, f.err
}
func (f *fakeStatusPage) PostUpdate(id string, u core.NewUpdate, actor string) (core.StatusIncident, error) {
	if err := core.ValidateUpdate(u); err != nil {
		return core.StatusIncident{}, err
	}
	for i := range f.incs {
		if f.incs[i].ID == id {
			f.incs[i].Status = u.Status
			f.incs[i].Updates = append(f.incs[i].Updates, core.IncidentUpdate{ID: fmt.Sprintf("u%d", len(f.incs[i].Updates)+1), Status: u.Status, Message: u.Message, Author: actor})
			f.actions = append(f.actions, "post:"+id+":"+actor)
			return f.incs[i], f.err
		}
	}
	return core.StatusIncident{}, fmt.Errorf("no such incident: %w", core.ErrNotFound)
}
func (f *fakeStatusPage) EditUpdate(id, uid string, u core.NewUpdate, actor string) (core.StatusIncident, error) {
	f.actions = append(f.actions, "edit:"+id+":"+uid+":"+actor)
	return f.Incident(id)
}
func (f *fakeStatusPage) EditIncident(id, title string, svcs []string, actor string) (core.StatusIncident, error) {
	f.actions = append(f.actions, "editinc:"+id+":"+actor)
	return f.Incident(id)
}
func (f *fakeStatusPage) DeleteIncident(id, actor string) error {
	f.actions = append(f.actions, "delinc:"+id+":"+actor)
	return f.err
}
func (f *fakeStatusPage) Public() (core.PublicStatus, error) { return f.pub, f.err }
