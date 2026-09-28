package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// ---- fixtures ---------------------------------------------------------

// sampleFiringIncident is this file's main fixture: one firing incident with
// members across three nodes exercising every field the detail page's
// member table/silence form care about -- an open, delivered member
// (db1/disk_pct), a suppressed (dependency-folded) open member
// (web1/cpu_pct), a silenced open member (db2/mem_pct), and a RESOLVED
// member (web2/load1) that must never show up in the silence-from-incident
// matcher list. Its Timeline covers every kind task-2-brief.md's test
// bullet names: fired, grouped, suppressed (with reason), delivered (via
// channels), escalated, acked (by), receipt, resolved -- a mix of
// structured (Leg/Policy set) and one deliberately legacy (neither set)
// event, so incidentEventText's fallback-to-Detail path is exercised too.
func sampleFiringIncident() core.Incident {
	return core.Incident{
		ID:       "inc1",
		GroupKey: "grp1",
		Title:    "disk full on db1",
		Severity: "critical",
		State:    "firing",
		Nodes:    []string{"db1", "web1", "db2"},
		Opened:   1000,
		Updated:  1050,
		Alerts: []core.IncidentAlert{
			{Node: "db1", NodeName: "db1", Key: "disk_pct", Title: "disk full", Severity: "critical", FiredAt: 1000, DeliveredLocally: true},
			{Node: "web1", NodeName: "web1", Key: "cpu_pct", Title: "cpu high", Severity: "warning", FiredAt: 1010, Suppressed: "parent db1 down"},
			{Node: "db2", NodeName: "db2", Key: "mem_pct", Title: "mem high", Severity: "warning", FiredAt: 1020, SilencedBy: "maintenance nightly"},
			{Node: "web2", NodeName: "web2", Key: "load1", Title: "load high", Severity: "warning", FiredAt: 900, ResolvedAt: 950},
		},
		Timeline: []core.IncidentEvent{
			{TS: 1000, Kind: "fired", Leg: "fire", AlertKey: "disk_pct", Node: "db1", FiredAt: 1000},
			{TS: 1001, Kind: "grouped", Detail: "grouped into inc1"},
			{TS: 1002, Kind: "delivered", Leg: "fire", AlertKey: "disk_pct", Node: "db1", Policy: "default", Step: 0, Channels: []string{"telegram"}},
			{TS: 1003, Kind: "escalated", Leg: "fire", AlertKey: "disk_pct", Node: "db1", Policy: "default", Step: 1, Channels: []string{"email"}},
			{TS: 1004, Kind: "suppressed", Leg: "fire", AlertKey: "cpu_pct", Node: "web1", Detail: "parent db1 down"},
			{TS: 1005, Kind: "acked", Actor: "root"},
			{TS: 1006, Kind: "receipt", Leg: "fire", AlertKey: "disk_pct", Node: "db1", Channels: []string{"telegram"}},
			{TS: 1007, Kind: "resolved", Leg: "recover", AlertKey: "disk_pct", Node: "db1"},
		},
	}
}

// ---- list ---------------------------------------------------------------

// TestFleetIncidentsListShowsStateTitleSeverityNodesOpenedDurationChips pins
// task-2-brief.md's list test bullet verbatim: state, title, severity,
// nodes, opened, duration, delivered/suppressed chips. Uses a RESOLVED
// incident (fixed Opened/Resolved) so DurationText is deterministic --
// unlike a still-firing incident, whose duration depends on time.Now().
// Pins time.Local (withLocalTZ) since round-2 review finding I1 moved
// incidentTimeText's Opened rendering to the master-local-zone convention
// (silenceTimeText) -- see TestIncidentTimeTextUsesMasterLocalZone below for
// the focused pin of that convention itself.
func TestFleetIncidentsListShowsStateTitleSeverityNodesOpenedDurationChips(t *testing.T) {
	withLocalTZ(t, "Asia/Kolkata")
	resolved := core.Incident{
		ID: "inc2", Title: "swap high on web1", Severity: "warning", State: "resolved",
		Opened: 1000, Resolved: 1000 + 5400, // 5400s = bucketed to "1h" by incidentDurationText
		Alerts: []core.IncidentAlert{
			{Node: "web1", NodeName: "web1", Key: "swap_pct", DeliveredLocally: true},
			{Node: "web2", NodeName: "web2", Key: "swap_pct", Suppressed: "parent web1 down"},
		},
	}
	fleet := &fakeFleet{incidents: []core.Incident{resolved}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/incidents")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet/incidents status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		">resolved<", "swap high on web1", ">warning<", "web1", "web2",
		silenceTimeText(1000), // incidentTimeText(1000) in master-local zone
		">1h<",                // incidentDurationText(5400)
		"delivered 1", "suppressed 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /fleet/incidents: missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestFleetIncidentsListChipsCountFromTimelineWhenMemberFlagsUnset pins B5
// (2026-09-25 UI audit): "delivered N / suppressed N" showed "0/0" even
// though the incident's own Timeline recorded 1 delivered + 1 suppressed
// event. Root cause -- IncidentAlert.DeliveredLocally only ever gets set for
// a CHILD's own local delivery (fleet_incidents.go's Apply/
// MarkDeliveredLocally); the master's own dispatch, the common case
// (fleetAlertEngine.deliverAndReceipt), records ONLY a Timeline "delivered"
// event via AppendEvent and never touches any IncidentAlert field. Likewise
// a silence (as opposed to a dependency fold) sets SilencedBy, not
// Suppressed, which the old count ignored entirely. This fixture leaves
// every member flag unset/empty -- exactly the master-delivered,
// silence-suppressed shape -- so the chips must still read 1/1 off the
// Timeline.
func TestFleetIncidentsListChipsCountFromTimelineWhenMemberFlagsUnset(t *testing.T) {
	inc := core.Incident{
		ID: "inc3", Title: "cpu high on api-01", Severity: "warning", State: "firing",
		Nodes: []string{"api-01", "api-02"}, Opened: 1000, Updated: 1010,
		Alerts: []core.IncidentAlert{
			{Node: "api-01", NodeName: "api-01", Key: "cpu_pct", FiredAt: 1000},
			{Node: "api-02", NodeName: "api-02", Key: "cpu_pct", FiredAt: 1005},
		},
		Timeline: []core.IncidentEvent{
			{TS: 1000, Kind: "fired", Leg: "fire", AlertKey: "cpu_pct", Node: "api-01"},
			{TS: 1002, Kind: "delivered", Leg: "fire", AlertKey: "cpu_pct", Node: "api-01", Policy: "default", Step: 0, Channels: []string{"telegram"}},
			{TS: 1005, Kind: "fired", Leg: "fire", AlertKey: "cpu_pct", Node: "api-02"},
			{TS: 1006, Kind: "suppressed", Leg: "fire", AlertKey: "cpu_pct", Node: "api-02", Detail: "silenced: maintenance nightly"},
		},
	}
	fleet := &fakeFleet{incidents: []core.Incident{inc}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/incidents")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet/incidents status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "delivered 1") {
		t.Errorf("GET /fleet/incidents: delivered chip did not count the master-dispatched Timeline event:\n%s", body)
	}
	if !strings.Contains(body, "suppressed 1") {
		t.Errorf("GET /fleet/incidents: suppressed chip did not count the silence Timeline event:\n%s", body)
	}
	if strings.Contains(body, "delivered 0") || strings.Contains(body, "suppressed 0") {
		t.Errorf("GET /fleet/incidents: chips still show 0/0 (B5 regression):\n%s", body)
	}
}

// TestIncidentTimeTextUsesMasterLocalZone pins round-2 review finding I1:
// incidentTimeText must render through the SAME master-local-zone-with-
// abbreviation convention silenceTimeText (handlers_fleet_silences.go)
// already established for Silences/Audit, rather than its own unlabeled-UTC
// format -- a user who just learned "times on this fleet UI carry my
// server's zone" from Silences/Audit must not then misread an Incidents
// timestamp as local when it was actually bare UTC (the same string either
// way, since neither format carried a zone marker).
func TestIncidentTimeTextUsesMasterLocalZone(t *testing.T) {
	withLocalTZ(t, "Asia/Kolkata")
	got := incidentTimeText(1893456000)
	want := silenceTimeText(1893456000)
	if got != want {
		t.Errorf("incidentTimeText(1893456000) = %q, want %q (silenceTimeText's own output)", got, want)
	}
	if !strings.Contains(got, "IST") {
		t.Errorf("incidentTimeText missing IST zone abbreviation, got %q", got)
	}
}

// TestFleetIncidentsListStateBadgeEmberOnlyForFiring pins the ember-
// discipline ruling for this task: firing is ember (.badge.crit), acked is
// amber (.badge.warn), resolved is verdigris (.badge.ok) -- never ember for
// anything but firing.
func TestFleetIncidentsListStateBadgeEmberOnlyForFiring(t *testing.T) {
	fleet := &fakeFleet{incidents: []core.Incident{
		{ID: "f1", Title: "firing one", State: "firing"},
		{ID: "a1", Title: "acked one", State: "acked"},
		{ID: "r1", Title: "resolved one", State: "resolved"},
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/incidents")
	body := rr.Body.String()
	if !strings.Contains(body, `class="badge crit">firing<`) {
		t.Errorf("firing incident missing ember badge, body:\n%s", body)
	}
	if !strings.Contains(body, `class="badge warn">acked<`) {
		t.Errorf("acked incident missing amber badge, body:\n%s", body)
	}
	if !strings.Contains(body, `class="badge ok">resolved<`) {
		t.Errorf("resolved incident missing ok badge, body:\n%s", body)
	}
	if strings.Contains(body, `class="badge crit">acked<`) || strings.Contains(body, `class="badge crit">resolved<`) {
		t.Errorf("ember used for a non-firing state, body:\n%s", body)
	}
}

// TestFleetIncidentsSoloDaemon404s pins "master only else 404" for both the
// list and the detail page.
func TestFleetIncidentsSoloDaemon404s(t *testing.T) {
	d := fleetSoloDeps(t)
	if rr := fleetGetAsViewer(t, d, "/fleet/incidents"); rr.Code != http.StatusNotFound {
		t.Errorf("GET /fleet/incidents on solo status = %d, want 404", rr.Code)
	}
	if rr := fleetGetAsViewer(t, d, "/fleet/incidents/inc1"); rr.Code != http.StatusNotFound {
		t.Errorf("GET /fleet/incidents/inc1 on solo status = %d, want 404", rr.Code)
	}
}

// TestFleetIncidentsPagination pins the 50-per-page ruling and that filters
// survive into the pager's Next link.
func TestFleetIncidentsPagination(t *testing.T) {
	incs := make([]core.Incident, 0, 60)
	for i := 0; i < 60; i++ {
		incs = append(incs, core.Incident{ID: "inc" + strconv.Itoa(i), Title: "incident " + strconv.Itoa(i), State: "firing"})
	}
	fleet := &fakeFleet{incidents: incs}
	d := fleetAdminDeps(t, fleet)

	rr := fleetGetAsViewer(t, d, "/fleet/incidents?state=firing")
	if rr.Code != http.StatusOK {
		t.Fatalf("page 1 status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Showing 50 of 60") {
		t.Errorf("page 1: missing 50-of-60 count, body:\n%s", body)
	}
	if !strings.Contains(body, "page=2") {
		t.Errorf("page 1: Next link missing page=2, body:\n%s", body)
	}
	if !strings.Contains(body, "state=firing") {
		t.Errorf("page 1: pager lost the state=firing filter, body:\n%s", body)
	}
	if strings.Contains(body, "incident 50") {
		t.Errorf("page 1 shows a row past the 50-item page size, body:\n%s", body)
	}

	rr2 := fleetGetAsViewer(t, d, "/fleet/incidents?state=firing&page=2")
	if rr2.Code != http.StatusOK {
		t.Fatalf("page 2 status = %d, want 200", rr2.Code)
	}
	body2 := rr2.Body.String()
	if !strings.Contains(body2, "Showing 10 of 60") {
		t.Errorf("page 2: missing 10-of-60 count, body:\n%s", body2)
	}
	if !strings.Contains(body2, "incident 50") || strings.Contains(body2, "incident 0<") {
		t.Errorf("page 2: wrong window of rows, body:\n%s", body2)
	}
}

// TestFleetIncidentsListPollsEvery10sWithSync is a static template-source
// scan (same convention TestAppJSFleetCompareSurvivesTablePoll uses for
// app.js) pinning task-2-brief.md's exact polling requirement: the list's
// tbody self-polls every 10s with hx-sync="this:replace".
func TestFleetIncidentsListPollsEvery10sWithSync(t *testing.T) {
	b, err := templatesFS.ReadFile("templates/fleet_incidents.html")
	if err != nil {
		t.Fatalf("read fleet_incidents.html: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, `hx-get="/fleet/incidents/table?`) {
		t.Error("fleet_incidents.html: tbody must poll /fleet/incidents/table")
	}
	if !strings.Contains(src, `hx-trigger="every 10s"`) {
		t.Error(`fleet_incidents.html: missing hx-trigger="every 10s"`)
	}
	if !strings.Contains(src, `hx-sync="this:replace"`) {
		t.Error(`fleet_incidents.html: missing hx-sync="this:replace"`)
	}
}

// ---- nav badge ------------------------------------------------------------

// TestFleetIncidentsNavBadgeSingleCall pins the ruling: the "Incidents" nav
// badge comes from ONE Incidents(State:"firing", Limit:1000) call per
// request, request-scoped cached like the fleet memo.
func TestFleetIncidentsNavBadgeSingleCall(t *testing.T) {
	fleet := &fakeFleet{incidents: []core.Incident{
		{ID: "a", State: "firing"},
		{ID: "b", State: "firing"},
		{ID: "c", State: "resolved"},
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.incidentsCalls != 1 {
		t.Fatalf("Incidents() calls while rendering /fleet = %d, want 1", fleet.incidentsCalls)
	}
	if fleet.lastIncidentsFilter.State != "firing" || fleet.lastIncidentsFilter.Limit != fleetIncidentsFiringCap {
		t.Errorf("nav badge filter = %+v, want State=firing Limit=%d", fleet.lastIncidentsFilter, fleetIncidentsFiringCap)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Incidents") {
		t.Errorf("GET /fleet: missing Incidents nav entry, body:\n%s", body)
	}
	if !strings.Contains(body, `<span class="ct">2</span>`) {
		t.Errorf("GET /fleet: missing Incidents badge count 2, body:\n%s", body)
	}
}

// TestFleetIncidentsNavHiddenOnSolo pins "master only" for the nav entry
// itself (global-constraints.md's "no fleet nav on solo/child").
func TestFleetIncidentsNavHiddenOnSolo(t *testing.T) {
	d := fleetSoloDeps(t)
	rr := fleetGetAsViewer(t, d, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), `href="/fleet/incidents"`) {
		t.Errorf("GET / on solo: Incidents nav entry should be hidden, body:\n%s", rr.Body.String())
	}
}

// ---- detail ---------------------------------------------------------------

// TestFleetIncidentDetailShowsMembersAndFullTimeline pins task-2-brief.md's
// detail test bullet verbatim: member alerts (node, key, fired, resolved,
// delivered locally, silenced by, folded reason) and the full timeline
// (fired, grouped, suppressed with reason, delivered via channels,
// escalated, acked by, receipt, resolved) -- rendered from the STRUCTURED
// fields, with Detail as fallback (the "grouped" legacy event).
func TestFleetIncidentDetailShowsMembersAndFullTimeline(t *testing.T) {
	fleet := &fakeFleet{incidents: []core.Incident{sampleFiringIncident()}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/incidents/inc1")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet/incidents/inc1 status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Members table.
	for _, want := range []string{
		"db1", "disk_pct", "web1", "cpu_pct", "db2", "mem_pct",
		"maintenance nightly", // db2's SilencedBy
		"parent db1 down",     // web1's Suppressed/folded reason
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /fleet/incidents/inc1: missing member field %q\nbody:\n%s", want, body)
		}
	}

	// Timeline: every kind, and structured-field text for the
	// delivered/escalated/receipt/resolved events (channels/policy/leg).
	for _, want := range []string{
		">fired<", ">grouped<", ">suppressed<", ">delivered<", ">escalated<", ">acked<", ">receipt<", ">resolved<",
		"grouped into inc1", // legacy Detail fallback
		"channels=telegram", "channels=email",
		"policy=default step=1",
		"leg=recover",
		"root", // acked event's Actor
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /fleet/incidents/inc1: missing timeline content %q\nbody:\n%s", want, body)
		}
	}
}

// TestFleetIncidentDetailResolvedDisablesAckAndSilence pins "resolved
// incidents have no ack button, and the silence action is disabled."
func TestFleetIncidentDetailResolvedDisablesAckAndSilence(t *testing.T) {
	inc := sampleFiringIncident()
	inc.State = "resolved"
	inc.Resolved = 2000
	fleet := &fakeFleet{incidents: []core.Incident{inc}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/incidents/inc1")
	body := rr.Body.String()
	if strings.Contains(body, `action="/fleet/incidents/inc1/ack"`) {
		t.Errorf("resolved incident must not render an ack form, body:\n%s", body)
	}
	if strings.Contains(body, `action="/fleet/incidents/inc1/silence"`) {
		t.Errorf("resolved incident must not render a silence form, body:\n%s", body)
	}
}

// TestFleetIncidentUnknownID404s pins "no such incident" -> plain 404, not a
// flash.
func TestFleetIncidentUnknownID404s(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	rr := fleetGetAsViewer(t, d, "/fleet/incidents/ghost")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /fleet/incidents/ghost status = %d, want 404", rr.Code)
	}
}

// ---- ack --------------------------------------------------------------

// TestFleetIncidentAckRecordsWebUser pins Review Focus 4: AckIncident must
// see the SIGNED-IN web user's own name as actor, and the redirect carries a
// flash back to the incident.
func TestFleetIncidentAckRecordsWebUser(t *testing.T) {
	fleet := &fakeFleet{incidents: []core.Incident{sampleFiringIncident()}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/incidents/inc1/ack", url.Values{})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("ack status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.ackedIncidentID != "inc1" {
		t.Errorf("AckIncident id = %q, want inc1", fleet.ackedIncidentID)
	}
	if fleet.ackedIncidentActor != "root" {
		t.Errorf("AckIncident actor = %q, want the signed-in admin's name (root)", fleet.ackedIncidentActor)
	}
	loc := rr.Header().Get("Location")
	if loc != "/fleet/incidents/inc1?flash=ack" {
		t.Errorf("ack redirect Location = %q, want /fleet/incidents/inc1?flash=ack (a fixed code, never the actor's name in the URL)", loc)
	}
}

// TestFleetIncidentAckRejectsResolved pins the server-side re-check backing
// "resolved incidents have no ack button" -- never trusting the
// hidden-control-implies-safe assumption.
func TestFleetIncidentAckRejectsResolved(t *testing.T) {
	inc := sampleFiringIncident()
	inc.State = "resolved"
	fleet := &fakeFleet{incidents: []core.Incident{inc}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/incidents/inc1/ack", url.Values{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("ack on resolved incident status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.ackedIncidentID != "" {
		t.Errorf("AckIncident must not be called for a resolved incident, got id=%q", fleet.ackedIncidentID)
	}
}

// TestFleetIncidentAckViewerDenied pins the RBAC floor.
func TestFleetIncidentAckViewerDenied(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{incidents: []core.Incident{sampleFiringIncident()}})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodPost, "/fleet/incidents/inc1/ack")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("POST ack as viewer = %d, want 403", rr.Code)
	}
}

// TestFleetIncidentMutationsRequireCSRF pins requireCSRF on both mutation
// routes.
func TestFleetIncidentMutationsRequireCSRF(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{incidents: []core.Incident{sampleFiringIncident()}})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)

	for _, target := range []string{"/fleet/incidents/inc1/ack", "/fleet/incidents/inc1/silence"} {
		rr := postForm(h, target, url.Values{}, cookie, "")
		if rr.Code != http.StatusForbidden {
			t.Errorf("POST %s with no CSRF token = %d, want 403", target, rr.Code)
		}
	}
}

// ---- silence ------------------------------------------------------------

// TestFleetIncidentSilencePrefillsMatchersFromOpenMembers pins task-2-
// brief.md's exact ruling: matchers are node id + rule (AlertKey), one per
// OPEN member, ORed -- recomputed server-side, excluding the incident's
// already-resolved member (web2/load1).
func TestFleetIncidentSilencePrefillsMatchersFromOpenMembers(t *testing.T) {
	fleet := &fakeFleet{incidents: []core.Incident{sampleFiringIncident()}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/incidents/inc1/silence", url.Values{"for": {"1h"}, "comment": {"noisy disk"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("silence status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.createdSilences) != 1 {
		t.Fatalf("CreateSilence calls = %d, want 1", len(fleet.createdSilences))
	}
	s := fleet.createdSilences[0]
	if s.Author != "root" {
		t.Errorf("Silence Author = %q, want the signed-in admin's name (root)", s.Author)
	}
	if s.Comment != "noisy disk" {
		t.Errorf("Silence Comment = %q, want %q", s.Comment, "noisy disk")
	}
	if s.End-s.Start != 3600 {
		t.Errorf("Silence window = %ds, want 3600s (1h)", s.End-s.Start)
	}
	want := []core.Matcher{
		{Node: "db1", Rule: "disk_pct"},
		{Node: "web1", Rule: "cpu_pct"},
		{Node: "db2", Rule: "mem_pct"},
	}
	if !reflect.DeepEqual(s.Matchers, want) {
		t.Errorf("Silence Matchers = %+v, want %+v", s.Matchers, want)
	}
	for _, m := range s.Matchers {
		if m.Node == "web2" {
			t.Errorf("resolved member web2 must not appear in the silence matchers: %+v", s.Matchers)
		}
	}

	loc := rr.Header().Get("Location")
	if loc != "/fleet/incidents/inc1?flash=silenced&for=1h" {
		t.Errorf("silence redirect Location = %q, want /fleet/incidents/inc1?flash=silenced&for=1h", loc)
	}
}

// TestFleetIncidentSilenceRejectsBadDuration pins "validation errors render
// inline, never a 500" for an out-of-range "for" value.
func TestFleetIncidentSilenceRejectsBadDuration(t *testing.T) {
	fleet := &fakeFleet{incidents: []core.Incident{sampleFiringIncident()}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/incidents/inc1/silence", url.Values{"for": {"3d"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("silence with bad duration status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.createdSilences) != 0 {
		t.Errorf("CreateSilence must not be called on a validation failure, got %d calls", len(fleet.createdSilences))
	}
	if !strings.Contains(rr.Body.String(), "choose a valid duration") {
		t.Errorf("missing inline duration validation message, body:\n%s", rr.Body.String())
	}
}

// TestFleetIncidentSilenceNoOpenMembersRejected pins "the silence action is
// disabled" for a resolved incident, at the handler level too (never
// trusting the disabled control client-side).
func TestFleetIncidentSilenceNoOpenMembersRejected(t *testing.T) {
	inc := sampleFiringIncident()
	// Resolve every member.
	for i := range inc.Alerts {
		inc.Alerts[i].ResolvedAt = 2000
	}
	fleet := &fakeFleet{incidents: []core.Incident{inc}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/incidents/inc1/silence", url.Values{"for": {"1h"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("silence with no open members status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.createdSilences) != 0 {
		t.Errorf("CreateSilence must not be called with no open members, got %d calls", len(fleet.createdSilences))
	}
	if !strings.Contains(rr.Body.String(), "no open members to silence") {
		t.Errorf("missing inline no-open-members message, body:\n%s", rr.Body.String())
	}
}

// TestFleetIncidentSilenceFleetAPIErrorRendersInline pins "FleetAPI errors
// render as a flash message ... Never return a 500" for a CreateSilence
// rejection (e.g. the daemon's own "matcher must match something" check).
func TestFleetIncidentSilenceFleetAPIErrorRendersInline(t *testing.T) {
	fleet := &fakeFleet{incidents: []core.Incident{sampleFiringIncident()}, createSilenceErr: errTestSilenceRejected}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/incidents/inc1/silence", url.Values{"for": {"1h"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("silence status = %d, want 400 (never 500), body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), errTestSilenceRejected.Error()) {
		t.Errorf("missing FleetAPI error inline, body:\n%s", rr.Body.String())
	}
}

var errTestSilenceRejected = &testError{"silence: rejected"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// ---- round 1 review fix: ?flash= must be a fixed code, never free text ---

// TestFleetIncidentFlashArbitraryTextRendersNothing pins the round-1 review
// SECURITY fix: a crafted ?flash=<arbitrary text> link must never render
// that text in the trusted flash banner (message-spoofing via a link) --
// only the two fixed codes ("ack", "silenced") ever produce a flash.
func TestFleetIncidentFlashArbitraryTextRendersNothing(t *testing.T) {
	fleet := &fakeFleet{incidents: []core.Incident{sampleFiringIncident()}}
	d := fleetAdminDeps(t, fleet)
	for _, raw := range []string{
		"your+account+has+been+compromised%2C+call+555-0100",
		"<script>alert(1)</script>",
		"acked", // close to, but not, the real code
		"silenced",
	} {
		rr := fleetGetAsViewer(t, d, "/fleet/incidents/inc1?flash="+raw)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /fleet/incidents/inc1?flash=%s status = %d, want 200", raw, rr.Code)
		}
		body := rr.Body.String()
		if strings.Contains(body, `class="flash"`) || strings.Contains(body, `class="flash err"`) {
			t.Errorf("GET /fleet/incidents/inc1?flash=%s: rendered a flash banner for a non-fixed code, body:\n%s", raw, body)
		}
	}
}

// TestFleetIncidentFlashFixedCodesRenderExpectedText pins that each of the
// two fixed codes still renders its own text: "ack" recomputes the acting
// user from the CURRENT session (never from the URL -- there is no name in
// this URL at all), and "silenced" requires a "for" from the fixed duration
// allowlist.
func TestFleetIncidentFlashFixedCodesRenderExpectedText(t *testing.T) {
	fleet := &fakeFleet{incidents: []core.Incident{sampleFiringIncident()}}
	d := fleetAdminDeps(t, fleet)

	rr := fleetGetAsViewer(t, d, "/fleet/incidents/inc1?flash=ack")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Incident acknowledged by viewer-user") {
		t.Errorf("?flash=ack: missing expected text (current session's own name), body:\n%s", rr.Body.String())
	}

	rr2 := fleetGetAsViewer(t, d, "/fleet/incidents/inc1?flash=silenced&for=4h")
	if rr2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "Silenced for 4h") {
		t.Errorf("?flash=silenced&for=4h: missing expected text, body:\n%s", rr2.Body.String())
	}

	// An unrecognized "for" alongside the real "silenced" code still renders
	// nothing -- the allowlist check applies to the parameter, not just the
	// code.
	rr3 := fleetGetAsViewer(t, d, "/fleet/incidents/inc1?flash=silenced&for=3d")
	if strings.Contains(rr3.Body.String(), "Silenced for") {
		t.Errorf("?flash=silenced&for=3d: must render nothing (3d is not an allowlisted duration), body:\n%s", rr3.Body.String())
	}
}

// ---- round 1 review fix: anonymous requests must redirect to /login ------

// TestFleetIncidentsAnonymousRedirectsToLogin pins the RBAC floor for every
// GET route on this page: no session at all redirects to /login (302), the
// same as every other requireRole(RoleViewer, ...)-gated route.
func TestFleetIncidentsAnonymousRedirectsToLogin(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{incidents: []core.Incident{sampleFiringIncident()}})
	for _, target := range []string{"/fleet/incidents", "/fleet/incidents/table", "/fleet/incidents/inc1"} {
		rr := fleetGetAnonymous(d, target)
		if rr.Code != http.StatusFound {
			t.Errorf("GET %s anonymous status = %d, want 302", target, rr.Code)
			continue
		}
		if loc := rr.Header().Get("Location"); loc != "/login" {
			t.Errorf("GET %s anonymous Location = %q, want /login", target, loc)
		}
	}
}

// ---- round 1 review fix: mutation on an unknown incident id --------------

// TestFleetIncidentAckUnknownID404sWithNoMutation pins that a POST ack for
// an id Fleet() doesn't recognize 404s and never calls AckIncident.
func TestFleetIncidentAckUnknownID404sWithNoMutation(t *testing.T) {
	fleet := &fakeFleet{incidents: []core.Incident{sampleFiringIncident()}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/incidents/ghost/ack", url.Values{})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("ack unknown id status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.ackedIncidentID != "" {
		t.Errorf("AckIncident must not be called for an unknown id, got id=%q", fleet.ackedIncidentID)
	}
}

// TestFleetIncidentSilenceUnknownID404sWithNoMutation pins that a POST
// silence for an id Fleet() doesn't recognize 404s and never calls
// CreateSilence.
func TestFleetIncidentSilenceUnknownID404sWithNoMutation(t *testing.T) {
	fleet := &fakeFleet{incidents: []core.Incident{sampleFiringIncident()}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/incidents/ghost/silence", url.Values{"for": {"1h"}})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("silence unknown id status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.createdSilences) != 0 {
		t.Errorf("CreateSilence must not be called for an unknown id, got %d calls", len(fleet.createdSilences))
	}
}
