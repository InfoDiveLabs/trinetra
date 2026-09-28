package web

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fleetFiveNodeRoster is task-5-brief.md's exact Step 1 fixture: self
// online, web1 online tagged "web", web2 lagging (also carrying a clock
// skew and replica drops, to exercise the Link column's warning chips),
// db1 down, old1 revoked.
func fleetFiveNodeRoster() []core.NodeSummary {
	return []core.NodeSummary{
		{ID: core.SelfNodeID, Name: "self", Self: true, State: "online", CPU: 5, MemPct: 20, WorstDiskPct: 30, Load1: 0.5, Version: "1.2.3"},
		{ID: "web1", Name: "web1", State: "online", Tags: []string{"web"}, CPU: 12, MemPct: 40, WorstDiskPct: 55, Load1: 0.8, Version: "1.2.3", LastSeen: 1000},
		{ID: "web2", Name: "web2", State: "lagging", Tags: []string{"web"}, CPU: 30, MemPct: 60, WorstDiskPct: 70, Load1: 1.1, Version: "1.2.2", LastSeen: 990,
			SkewSec: 45, DroppedOutOfOrder: 3, DroppedCardinality: 1, DroppedDuplicate: 2, OutboxBytes: 2048, OutboxOldest: 900},
		{ID: "db1", Name: "db1", State: "down", CPU: 0, MemPct: 0, WorstDiskPct: 90, Load1: 0, Version: "1.2.3", LastSeen: 500},
		{ID: "old1", Name: "old1", State: "revoked", Revoked: true, Version: "1.0.0", LastSeen: 100},
	}
}

// fleetMasterDeps builds a master Deps (fleetTestDeps, node_scope_test.go)
// whose Fleet().Nodes/Status report the given roster -- config.RoleMaster,
// so fleetRole(d)/resolveMasterAndNodes both agree this daemon is a fleet
// master.
func fleetMasterDeps(t *testing.T, nodes []core.NodeSummary) Deps {
	t.Helper()
	fleet := &fakeFleet{status: core.FleetStatus{Role: config.RoleMaster}, nodes: nodes}
	return fleetTestDeps(t, masterFakeAPI(fleet, nil))
}

// fleetSoloDeps builds a non-master Deps: Status reports solo and the
// roster is just self, mirroring a genuinely solo daemon's Fleet().Nodes
// (fleet_provider.go).
func fleetSoloDeps(t *testing.T) Deps {
	t.Helper()
	fleet := &fakeFleet{status: core.FleetStatus{Role: config.RoleSolo}, nodes: []core.NodeSummary{{ID: core.SelfNodeID, Self: true, State: "online"}}}
	return fleetTestDeps(t, masterFakeAPI(fleet, nil))
}

// fleetChildDeps builds a child Deps: Status reports child, roster is just
// self (a child daemon's own Fleet().Nodes never lists siblings -- it only
// knows its master).
func fleetChildDeps(t *testing.T) Deps {
	t.Helper()
	fleet := &fakeFleet{status: core.FleetStatus{Role: config.RoleChild}, nodes: []core.NodeSummary{{ID: core.SelfNodeID, Self: true, State: "online"}}}
	return fleetTestDeps(t, masterFakeAPI(fleet, nil))
}

func fleetGetAsRole(t *testing.T, d Deps, role Role, target string) *httptest.ResponseRecorder {
	t.Helper()
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, role, http.MethodGet, target))
	return rr
}

func fleetGetAsViewer(t *testing.T, d Deps, target string) *httptest.ResponseRecorder {
	t.Helper()
	return fleetGetAsRole(t, d, RoleViewer, target)
}

// fleetGetAnonymous drives target through the full handler stack with no
// session at all (no seedSignedInRequest cookie), for the RBAC-anonymous
// negative cases.
func fleetGetAnonymous(d Deps, target string) *httptest.ResponseRecorder {
	h := newHandler(d)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
	return rr
}

// newCountingFleet builds a countingFleet (templates_node_test.go, shared
// with the Task 4 Fleet().Status()-budget tests) with BOTH counters wired,
// for this file's round-1 review test that cares about both Status() and
// Nodes() call counts.
func newCountingFleet(inner core.FleetAPI) (countingFleet, *int, *int) {
	var statusCalls, nodesCalls int
	return countingFleet{FleetAPI: inner, statusCalls: &statusCalls, nodesCalls: &nodesCalls}, &statusCalls, &nodesCalls
}

// TestFleetOverviewHealthStripAndTable pins the full Step 1 page shape: the
// health strip's four counts, the table's columns/row content, self vs.
// remote row links, and the Link column's CLI-worded warning chips.
func TestFleetOverviewHealthStripAndTable(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsViewer(t, d, "/fleet")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Health strip: online 2 (self, web1), behind 1 (web2, lagging), down 1
	// (db1), revoked 1 (old1).
	for _, want := range []string{
		`href="/fleet?state=online"><div class="n">2</div>`,
		`href="/fleet?state=lagging"><div class="n">1</div>`,
		`href="/fleet?state=down"><div class="n">1</div>`,
		`href="/fleet?state=revoked"><div class="n">1</div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /fleet: missing health strip fragment %q\nbody:\n%s", want, body)
		}
	}

	// Rows: self links to "/", a remote node links to "/n/<id>/".
	if !strings.Contains(body, `<a href="/">self</a>`) {
		t.Error(`GET /fleet: self row should link to "/"`)
	}
	if !strings.Contains(body, `<a href="/n/web1/">web1</a>`) {
		t.Error(`GET /fleet: web1 row should link to "/n/web1/"`)
	}
	if !strings.Contains(body, `<a href="/n/db1/">db1</a>`) {
		t.Error(`GET /fleet: db1 row should link to "/n/db1/"`)
	}

	// Link column: CLI-worded warning chips for web2's skew and drops --
	// asserted as the FULL verbatim sentence (round-1 review: a prefix
	// match let a dropped "(fix NTP on that host)" suffix slip through
	// undetected), against internal/trinetra/fleet_cmd.go:448's
	// printNodeWarnings wording. html/template escapes both "'" and "+"
	// (its default text escaper widens the replacement table beyond the
	// bare minimum), so the literal wording is checked against the
	// UNescaped body.
	unescaped := html.UnescapeString(body)
	if !strings.Contains(unescaped, "clock differs from this master's by +45s (fix NTP on that host)") {
		t.Errorf("GET /fleet: missing web2's full clock-skew warning sentence\nbody:\n%s", unescaped)
	}
	if !strings.Contains(unescaped, "replica drops: 3 out of order, 1 over the series limit, 2 duplicates (harmless re-sends)") {
		t.Errorf("GET /fleet: missing web2's replica-drops warning chip\nbody:\n%s", unescaped)
	}

	// State text is never colour-only: every row's literal state string is
	// present regardless of badge class.
	for _, want := range []string{">online<", ">lagging<", ">down<", ">revoked<"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /fleet: missing state text %q", want)
		}
	}
}

// TestFleetSkewWarnTextVerbatim is a direct, exact-string unit test for
// fleetSkewWarnText (round-1 review: the page-level test above only ever
// asserted a substring/prefix, which didn't catch the missing "(fix NTP on
// that host)" suffix -- this pins the FULL sentence against
// internal/trinetra/fleet_cmd.go:448's printNodeWarnings wording with
// strict equality, not Contains).
func TestFleetSkewWarnTextVerbatim(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    core.NodeSummary
		want string
	}{
		{"ahead", core.NodeSummary{SkewSec: 45}, "clock differs from this master's by +45s (fix NTP on that host)"},
		{"behind", core.NodeSummary{SkewSec: -60}, "clock differs from this master's by -60s (fix NTP on that host)"},
		{"within threshold", core.NodeSummary{SkewSec: 30}, ""},
		{"self always exempt", core.NodeSummary{Self: true, SkewSec: 999}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fleetSkewWarnText(tc.n); got != tc.want {
				t.Errorf("fleetSkewWarnText(%+v) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}

// TestFleetDropsWarnTextIgnoresDuplicatesAlone pins U3 (2026-09-25 UI
// audit): duplicates are explicitly "harmless re-sends" per this very
// sentence's own wording, so they alone must never earn the amber
// .fleet-link-warn styling on /fleet or /fleet/admin -- only an actual
// out-of-order or over-the-series-limit drop does.
func TestFleetDropsWarnTextIgnoresDuplicatesAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    core.NodeSummary
		want string
	}{
		{"duplicates only", core.NodeSummary{DroppedDuplicate: 5}, ""},
		{"none at all", core.NodeSummary{}, ""},
		{"self always exempt", core.NodeSummary{Self: true, DroppedOutOfOrder: 9}, ""},
		{"out of order", core.NodeSummary{DroppedOutOfOrder: 1},
			"replica drops: 1 out of order, 0 over the series limit, 0 duplicates (harmless re-sends)"},
		{"over the series limit", core.NodeSummary{DroppedCardinality: 1},
			"replica drops: 0 out of order, 1 over the series limit, 0 duplicates (harmless re-sends)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fleetDropsWarnText(tc.n); got != tc.want {
				t.Errorf("fleetDropsWarnText(%+v) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}

// TestFleetOverviewFilters pins core.NodeFilter semantics applied through
// the query string: ?tag=web&state=online&q=we narrows the 5-node roster
// to exactly web1 (web2 is tagged "web" too but is state=lagging, not
// online; "we" as a query substring matches "web1"/"web2" by name but the
// state filter alone already excludes web2).
func TestFleetOverviewFilters(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsViewer(t, d, "/fleet?tag=web&state=online&q=we")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `<a href="/n/web1/">web1</a>`) {
		t.Error("filtered /fleet: expected web1's row")
	}
	for _, absent := range []string{`<a href="/n/web2/">web2</a>`, `<a href="/n/db1/">db1</a>`, `<a href="/n/old1/">old1</a>`, `<a href="/">self</a>`} {
		if strings.Contains(body, absent) {
			t.Errorf("filtered /fleet: unexpected row present: %s\nbody:\n%s", absent, body)
		}
	}
	// The health strip stays fleet-wide (unaffected by the filter).
	if !strings.Contains(body, `href="/fleet?state=online"><div class="n">2</div>`) {
		t.Error("filtered /fleet: health strip should still show the full-roster online count (2)")
	}
}

// TestFleetOverviewBehindStateIncludesStale pins the ruling's documented
// deviation: the health strip's "Behind" link (?state=lagging) must also
// surface a "stale" node, not just "lagging" ones.
func TestFleetOverviewBehindStateIncludesStale(t *testing.T) {
	nodes := append(fleetFiveNodeRoster(), core.NodeSummary{ID: "cache1", Name: "cache1", State: "stale", LastSeen: 800})
	d := fleetMasterDeps(t, nodes)
	rr := fleetGetAsViewer(t, d, "/fleet?state=lagging")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `<a href="/n/web2/">web2</a>`) {
		t.Error("?state=lagging: expected web2 (state=lagging)")
	}
	if !strings.Contains(body, `<a href="/n/cache1/">cache1</a>`) {
		t.Error("?state=lagging: expected cache1 (state=stale) to also match, per the ruling")
	}
	if strings.Contains(body, `<a href="/n/db1/">db1</a>`) {
		t.Error("?state=lagging: db1 (state=down) must not match")
	}
}

// TestFleetOverviewSort pins explicit sort/dir query handling: ?sort=cpu
// &dir=desc orders rows by descending CPU (web2 30 > web1 12 > self 5 >
// db1 0, old1 has CPU 0 too but sorts stably after db1).
func TestFleetOverviewSort(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsViewer(t, d, "/fleet?sort=cpu&dir=desc")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	idx := func(id string) int {
		i := strings.Index(body, `data-node-id="`+id+`"`)
		if i < 0 {
			t.Fatalf("row for %q not found in body", id)
		}
		return i
	}
	iWeb2, iWeb1, iSelf := idx("web2"), idx("web1"), idx(core.SelfNodeID)
	if !(iWeb2 < iWeb1 && iWeb1 < iSelf) {
		t.Errorf("sort=cpu&dir=desc: expected web2 < web1 < self by position, got web2=%d web1=%d self=%d", iWeb2, iWeb1, iSelf)
	}
}

// TestFleetOverviewDefaultSort pins the documented default: down nodes
// first, then name ascending.
func TestFleetOverviewDefaultSort(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsViewer(t, d, "/fleet")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	idx := func(id string) int {
		i := strings.Index(body, `data-node-id="`+id+`"`)
		if i < 0 {
			t.Fatalf("row for %q not found in body", id)
		}
		return i
	}
	iDb1 := idx("db1")
	for _, other := range []string{core.SelfNodeID, "web1", "web2", "old1"} {
		if idx(other) < iDb1 {
			t.Errorf("default sort: expected db1 (down) before %q, got db1=%d %s=%d", other, iDb1, other, idx(other))
		}
	}
	// Among the non-down nodes, name-ascending: old1 < self < web1 < web2.
	iOld1, iSelf, iWeb1, iWeb2 := idx("old1"), idx(core.SelfNodeID), idx("web1"), idx("web2")
	if !(iOld1 < iSelf && iSelf < iWeb1 && iWeb1 < iWeb2) {
		t.Errorf("default sort: expected name-ascending among non-down nodes, got old1=%d self=%d web1=%d web2=%d", iOld1, iSelf, iWeb1, iWeb2)
	}
}

// TestFleetTableFragmentReturnsOnlyTbody pins GET /fleet/table's exact
// output shape: a single <tbody id="fleet-tbody">...</tbody> fragment, no
// surrounding page/base.html markup, and no loss of the same filter/sort
// query semantics /fleet itself applies.
func TestFleetTableFragmentReturnsOnlyTbody(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsViewer(t, d, "/fleet/table?tag=web&state=online")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := strings.TrimSpace(rr.Body.String())
	if !strings.HasPrefix(body, `<tbody id="fleet-tbody"`) {
		t.Fatalf("GET /fleet/table: body must start with the tbody fragment, got:\n%s", body)
	}
	if !strings.HasSuffix(body, "</tbody>") {
		t.Fatalf("GET /fleet/table: body must end with </tbody>, got:\n%s", body)
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "<body") || strings.Contains(body, `class="app"`) {
		t.Errorf("GET /fleet/table: fragment must not include the page shell:\n%s", body)
	}
	if !strings.Contains(body, `<a href="/n/web1/">web1</a>`) {
		t.Error("GET /fleet/table: filter (tag=web&state=online) should still apply, expected web1's row")
	}
	if strings.Contains(body, `<a href="/n/web2/">web2</a>`) {
		t.Error("GET /fleet/table: web2 (state=lagging) must not match state=online")
	}
	// The fragment's own self-poll re-targets the SAME query string it was
	// rendered with (url.Values.Encode() sorts keys alphabetically: state
	// before tag), so a subsequent htmx swap-in keeps polling with the
	// filter still applied.
	if !strings.Contains(html.UnescapeString(body), `hx-get="/fleet/table?state=online&tag=web"`) {
		t.Errorf("GET /fleet/table: fragment's self-poll hx-get should preserve the query string, got:\n%s", body)
	}
	if !strings.Contains(body, `hx-trigger="every 5s"`) {
		t.Error("GET /fleet/table: expected hx-trigger=\"every 5s\" on the self-polling tbody")
	}
}

// TestFleetPageEmbedsPollingQueryString pins the full page's initial tbody
// carrying the SAME query string the page itself was requested with -- the
// htmx poll fragment must keep the current query string (global-
// constraints.md/task-5-brief.md).
func TestFleetPageEmbedsPollingQueryString(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsViewer(t, d, "/fleet?state=down&sort=name&dir=asc")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := html.UnescapeString(rr.Body.String())
	if !strings.Contains(body, `hx-get="/fleet/table?dir=asc&sort=name&state=down"`) {
		t.Errorf("GET /fleet?state=down&sort=name&dir=asc: expected the tbody's hx-get to carry the same query string, body:\n%s", body)
	}
}

// TestFleetNodesAPI pins GET /api/fleet/nodes' JSON contract: []core.NodeSummary,
// filtered by tag/state/q using plain core.NodeFilter.Match semantics (NOT
// the HTML page's "?state=lagging also matches stale" convenience -- see
// fleetStateMatches' doc).
func TestFleetNodesAPI(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsViewer(t, d, "/api/fleet/nodes?tag=web")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got []core.NodeSummary
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rr.Body.String())
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (web1, web2 -- both tagged \"web\"), body: %s", len(got), rr.Body.String())
	}
	ids := map[string]bool{}
	for _, n := range got {
		ids[n.ID] = true
	}
	if !ids["web1"] || !ids["web2"] {
		t.Errorf("expected web1 and web2 in result, got %+v", got)
	}

	// state=lagging on the JSON API is an EXACT match -- a "stale" node
	// must NOT be included (unlike the HTML page's health-strip link).
	nodes := append(fleetFiveNodeRoster(), core.NodeSummary{ID: "cache1", Name: "cache1", State: "stale"})
	d2 := fleetMasterDeps(t, nodes)
	rr2 := fleetGetAsViewer(t, d2, "/api/fleet/nodes?state=lagging")
	var got2 []core.NodeSummary
	if err := json.Unmarshal(rr2.Body.Bytes(), &got2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got2) != 1 || got2[0].ID != "web2" {
		t.Errorf("state=lagging (JSON API, exact match): got %+v, want only web2", got2)
	}
}

// TestFleetRoutesNotFoundOnSoloAndChild pins the "masters only" gate: on a
// solo or child daemon, all three routes 404 and the nav carries no "Fleet"
// entry (task-5-brief.md: "All 404 unless fleetRole(d)=='master'";
// global-constraints.md: "no fleet nav" on solo/child).
func TestFleetRoutesNotFoundOnSoloAndChild(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps func(t *testing.T) Deps
	}{
		{"solo", fleetSoloDeps},
		{"child", fleetChildDeps},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.deps(t)
			for _, target := range []string{"/fleet", "/fleet/table", "/api/fleet/nodes", "/fleet/compare?tag=web", "/fleet/incidents", "/fleet/incidents/table", "/fleet/incidents/inc1"} {
				rr := fleetGetAsViewer(t, d, target)
				if rr.Code != http.StatusNotFound {
					t.Errorf("GET %s on %s = %d, want 404", target, tc.name, rr.Code)
				}
			}

			// The dashboard's nav must carry no "Fleet" entry/link at all.
			rr := fleetGetAsViewer(t, d, "/")
			if rr.Code != http.StatusOK {
				t.Fatalf("GET / on %s = %d, want 200", tc.name, rr.Code)
			}
			body := rr.Body.String()
			if strings.Contains(body, `href="/fleet"`) {
				t.Errorf("GET / on %s: nav must not link to /fleet, body:\n%s", tc.name, body)
			}
		})
	}
}

// TestFleetOverviewNavItemOnMaster is TestFleetRoutesNotFoundOnSoloAndChild's
// positive counterpart: a master's nav DOES carry the "Fleet" entry, and a
// down node's count shows as its badge.
func TestFleetOverviewNavItemOnMaster(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsViewer(t, d, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/fleet"`) {
		t.Errorf("GET / on master: expected nav to link to /fleet, body:\n%s", body)
	}
	if !strings.Contains(body, `<span class="ic">⛶</span><span class="lb">Fleet</span><span class="ct">1</span>`) {
		t.Errorf("GET / on master: expected the Fleet nav badge to show 1 (one down node), body:\n%s", body)
	}
}

// TestFleetOverviewViewerCanSee pins RBAC: /fleet is viewer-reachable, not
// admin-only (task-5-brief.md: "GET /fleet (viewer)").
func TestFleetOverviewViewerCanSee(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsViewer(t, d, "/fleet")
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer GET /fleet status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
}

// TestFleetRequestScopedMemoLimitsRoundTrips is round-1 review item 2: GET
// /fleet was making 2x Status() + 2x Nodes() (fleetGateHTML's fleetRole
// call plus newPageData's resolveFleetPageInfo; fetchFleetNodes plus
// navCountsFor's FleetDown count). With the request-scoped fleetMemo
// (fleet_memo.go) wired through every one of those call sites, both GET
// /fleet (the master-local page) and GET /n/child1/monitoring (a
// node-scoped page, exercising withNodeRouter's own resolveMasterAndNodes
// call alongside newPageData's) must make at most one real Status() call
// and one real Nodes() call each, for the whole request.
func TestFleetRequestScopedMemoLimitsRoundTrips(t *testing.T) {
	fakeF := &fakeFleet{
		status: core.FleetStatus{Role: config.RoleMaster},
		nodes: []core.NodeSummary{
			{ID: core.SelfNodeID, Self: true, State: "online"},
			{ID: "child1", Name: "child-one", State: "online"},
		},
	}
	cf, statusCalls, nodesCalls := newCountingFleet(fakeF)
	d := fleetTestDeps(t, masterFakeAPI(fakeF, map[string]core.API{"child1": childFakeAPI("child1-svc")}))
	d.Fleet = func() core.FleetAPI { return cf }

	for _, target := range []string{"/fleet", "/n/child1/monitoring"} {
		t.Run(target, func(t *testing.T) {
			*statusCalls, *nodesCalls = 0, 0
			rr := fleetGetAsViewer(t, d, target)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200, body: %s", target, rr.Code, rr.Body.String())
			}
			if *statusCalls > 1 {
				t.Errorf("GET %s: Status() called %d times, want <=1", target, *statusCalls)
			}
			if *nodesCalls > 1 {
				t.Errorf("GET %s: Nodes() called %d times, want <=1", target, *nodesCalls)
			}
		})
	}
}

// TestFleetTablePollingSyncsThis pins round-1 review item 3: the polling
// tbody must carry hx-sync="this:replace" so an in-flight poll is aborted/
// replaced by the next one rather than letting two responses race and
// apply out of order.
func TestFleetTablePollingSyncsThis(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsViewer(t, d, "/fleet")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `hx-sync="this:replace"`) {
		t.Errorf("GET /fleet: expected hx-sync=\"this:replace\" on the polling tbody, body:\n%s", rr.Body.String())
	}

	rr2 := fleetGetAsViewer(t, d, "/fleet/table")
	if rr2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), `hx-sync="this:replace"`) {
		t.Errorf("GET /fleet/table: expected hx-sync=\"this:replace\" on the fragment's tbody, body:\n%s", rr2.Body.String())
	}
}

// TestFleetNodesAPIRedactsRemoteAddrForNonAdmin is round-1 review item 4:
// GET /api/fleet/nodes blanks RemoteAddr for a viewer, but an admin session
// still sees the real value.
func TestFleetNodesAPIRedactsRemoteAddrForNonAdmin(t *testing.T) {
	nodes := fleetFiveNodeRoster()
	nodes[1].RemoteAddr = "10.0.0.5:9443" // web1
	d := fleetMasterDeps(t, nodes)

	rrViewer := fleetGetAsRole(t, d, RoleViewer, "/api/fleet/nodes")
	if rrViewer.Code != http.StatusOK {
		t.Fatalf("viewer GET /api/fleet/nodes status = %d, want 200, body: %s", rrViewer.Code, rrViewer.Body.String())
	}
	var viewerGot []core.NodeSummary
	if err := json.Unmarshal(rrViewer.Body.Bytes(), &viewerGot); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, n := range viewerGot {
		if n.RemoteAddr != "" {
			t.Errorf("viewer: node %s RemoteAddr = %q, want blank", n.ID, n.RemoteAddr)
		}
	}

	rrAdmin := fleetGetAsRole(t, d, RoleAdmin, "/api/fleet/nodes")
	if rrAdmin.Code != http.StatusOK {
		t.Fatalf("admin GET /api/fleet/nodes status = %d, want 200, body: %s", rrAdmin.Code, rrAdmin.Body.String())
	}
	var adminGot []core.NodeSummary
	if err := json.Unmarshal(rrAdmin.Body.Bytes(), &adminGot); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, n := range adminGot {
		if n.ID == "web1" {
			found = true
			if n.RemoteAddr != "10.0.0.5:9443" {
				t.Errorf("admin: web1 RemoteAddr = %q, want the real address", n.RemoteAddr)
			}
		}
	}
	if !found {
		t.Fatal("admin response missing web1")
	}
}

// TestFleetQueryNonAdminNeverMatchesRemoteAddr is the Task 6 review
// carry-over from Task 5: a non-admin's q= must never match RemoteAddr --
// on the HTML page (/fleet?q=) as well as the JSON API (already covered by
// TestFleetNodesAPIQueryNonAdminNeverMatchesRemoteAddr below) -- while an
// admin session's q= still does (core.NodeFilter.Match's own semantics,
// unchanged for admin).
func TestFleetQueryNonAdminNeverMatchesRemoteAddr(t *testing.T) {
	nodes := fleetFiveNodeRoster()
	nodes[1].RemoteAddr = "10.0.0.5:9443" // web1
	d := fleetMasterDeps(t, nodes)

	rrViewer := fleetGetAsRole(t, d, RoleViewer, "/fleet?q=10.0.0.5")
	if rrViewer.Code != http.StatusOK {
		t.Fatalf("viewer GET /fleet?q=10.0.0.5 status = %d, want 200, body: %s", rrViewer.Code, rrViewer.Body.String())
	}
	if strings.Contains(rrViewer.Body.String(), `<a href="/n/web1/">web1</a>`) {
		t.Errorf("viewer /fleet?q=10.0.0.5 matched web1 via RemoteAddr, want no match:\n%s", rrViewer.Body.String())
	}

	rrAdmin := fleetGetAsRole(t, d, RoleAdmin, "/fleet?q=10.0.0.5")
	if rrAdmin.Code != http.StatusOK {
		t.Fatalf("admin GET /fleet?q=10.0.0.5 status = %d, want 200, body: %s", rrAdmin.Code, rrAdmin.Body.String())
	}
	if !strings.Contains(rrAdmin.Body.String(), `<a href="/n/web1/">web1</a>`) {
		t.Errorf("admin /fleet?q=10.0.0.5 should still match web1 via RemoteAddr:\n%s", rrAdmin.Body.String())
	}
}

// TestFleetNodesAPIQueryNonAdminNeverMatchesRemoteAddr is the JSON API's
// counterpart to TestFleetQueryNonAdminNeverMatchesRemoteAddr: a viewer's
// q= matching only a node's RemoteAddr returns zero nodes; an admin's same
// query still finds it.
func TestFleetNodesAPIQueryNonAdminNeverMatchesRemoteAddr(t *testing.T) {
	nodes := fleetFiveNodeRoster()
	nodes[1].RemoteAddr = "10.0.0.5:9443" // web1
	d := fleetMasterDeps(t, nodes)

	rrViewer := fleetGetAsRole(t, d, RoleViewer, "/api/fleet/nodes?q=10.0.0.5")
	if rrViewer.Code != http.StatusOK {
		t.Fatalf("viewer GET /api/fleet/nodes?q=10.0.0.5 status = %d, want 200, body: %s", rrViewer.Code, rrViewer.Body.String())
	}
	var viewerGot []core.NodeSummary
	if err := json.Unmarshal(rrViewer.Body.Bytes(), &viewerGot); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(viewerGot) != 0 {
		t.Errorf("viewer q=10.0.0.5 matched %d node(s) via RemoteAddr, want 0: %+v", len(viewerGot), viewerGot)
	}

	rrAdmin := fleetGetAsRole(t, d, RoleAdmin, "/api/fleet/nodes?q=10.0.0.5")
	if rrAdmin.Code != http.StatusOK {
		t.Fatalf("admin GET /api/fleet/nodes?q=10.0.0.5 status = %d, want 200, body: %s", rrAdmin.Code, rrAdmin.Body.String())
	}
	var adminGot []core.NodeSummary
	if err := json.Unmarshal(rrAdmin.Body.Bytes(), &adminGot); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, n := range adminGot {
		if n.ID == "web1" {
			found = true
		}
	}
	if !found {
		t.Errorf("admin q=10.0.0.5 should still match web1 via RemoteAddr, got %+v", adminGot)
	}
}

// TestFleetQueryNonAdminStillMatchesTags pins the ruling's positive half:
// a non-admin's q= still matches name/id/tags -- the RemoteAddr fix above
// must not break ordinary search.
func TestFleetQueryNonAdminStillMatchesTags(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsRole(t, d, RoleViewer, "/api/fleet/nodes?q=web")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var got []core.NodeSummary
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("viewer q=web: got %d node(s), want 2 (web1, web2 -- name and tag both contain \"web\"): %+v", len(got), got)
	}
}

// TestFleetTableFragmentQueryNonAdminNeverMatchesRemoteAddr is round-1
// review item (c): the /fleet/table htmx fragment applies the SAME
// non-admin RemoteAddr query-match rule as /fleet and /api/fleet/nodes
// (TestFleetQueryNonAdminNeverMatchesRemoteAddr/
// TestFleetNodesAPIQueryNonAdminNeverMatchesRemoteAddr above) -- a direct
// test on /fleet/table itself, not just its sibling endpoints.
func TestFleetTableFragmentQueryNonAdminNeverMatchesRemoteAddr(t *testing.T) {
	nodes := fleetFiveNodeRoster()
	nodes[1].RemoteAddr = "10.0.0.5:9443" // web1
	d := fleetMasterDeps(t, nodes)

	rrViewer := fleetGetAsRole(t, d, RoleViewer, "/fleet/table?q=10.0.0.5")
	if rrViewer.Code != http.StatusOK {
		t.Fatalf("viewer GET /fleet/table?q=10.0.0.5 status = %d, want 200, body: %s", rrViewer.Code, rrViewer.Body.String())
	}
	if strings.Contains(rrViewer.Body.String(), `<a href="/n/web1/">web1</a>`) {
		t.Errorf("viewer /fleet/table?q=10.0.0.5 matched web1 via RemoteAddr, want no match:\n%s", rrViewer.Body.String())
	}

	rrAdmin := fleetGetAsRole(t, d, RoleAdmin, "/fleet/table?q=10.0.0.5")
	if rrAdmin.Code != http.StatusOK {
		t.Fatalf("admin GET /fleet/table?q=10.0.0.5 status = %d, want 200, body: %s", rrAdmin.Code, rrAdmin.Body.String())
	}
	if !strings.Contains(rrAdmin.Body.String(), `<a href="/n/web1/">web1</a>`) {
		t.Errorf("admin /fleet/table?q=10.0.0.5 should still match web1 via RemoteAddr:\n%s", rrAdmin.Body.String())
	}
}

// TestFleetRoutesAnonymousRedirectToLogin is the brief's "minor" ask:
// /fleet/table and /api/fleet/nodes are viewer-gated exactly like /fleet
// itself -- an anonymous caller is redirected to /login (302), not 404 or
// 401.
func TestFleetRoutesAnonymousRedirectToLogin(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	for _, target := range []string{"/fleet/table", "/api/fleet/nodes"} {
		t.Run(target, func(t *testing.T) {
			rr := fleetGetAnonymous(d, target)
			if rr.Code != http.StatusFound {
				t.Errorf("GET %s anon status = %d, want %d", target, rr.Code, http.StatusFound)
			}
			if loc := rr.Header().Get("Location"); loc != "/login" {
				t.Errorf("GET %s anon Location = %q, want /login", target, loc)
			}
		})
	}
}

// ===========================================================================
// Task C1a: heatmap and top-N panels (task-1a-brief.md)
// ===========================================================================

// fleetHeatBandRoster is task-1a-brief.md's fixture for the heatmap's
// discrete colour ramp: a neutral CPU (50, below 70), an amber-low CPU (75,
// 70<=x<85), an amber-full CPU (92, >=85), a down node (state overrides the
// metric entirely, ember OUTLINE + "down" text) and a revoked node (neutral,
// "revoked" text) -- the down/revoked nodes carry an extreme CPU (99/10) to
// prove state always wins over the metric ramp.
func fleetHeatBandRoster() []core.NodeSummary {
	return []core.NodeSummary{
		{ID: "n1", Name: "n1", State: "online", CPU: 50},
		{ID: "n2", Name: "n2", State: "online", CPU: 75},
		{ID: "n3", Name: "n3", State: "online", CPU: 92},
		{ID: "n4", Name: "n4", State: "down", CPU: 99, LastSeen: 500},
		{ID: "n5", Name: "n5", State: "revoked", CPU: 10},
	}
}

// TestFleetHeatmapColorBandsAndOverrides pins the default (cpu) metric's
// discrete ramp -- below 70% neutral, 70-85% amber (low intensity), 85%+
// amber (full intensity) -- plus the down/revoked overrides, and that ember
// never appears as a metric colour (only .heat-down's outline, which is the
// ruling's one sanctioned "down" use).
func TestFleetHeatmapColorBandsAndOverrides(t *testing.T) {
	d := fleetMasterDeps(t, fleetHeatBandRoster())
	rr := fleetGetAsViewer(t, d, "/fleet")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	heatTile := func(id, class, href, text string) string {
		return fmt.Sprintf(`<a class="heat-tile heat-%s" href="%s" data-heat-id="%s"><span class="heat-name">%s</span><span class="heat-val">%s</span></a>`,
			class, href, id, id, text)
	}
	for _, tc := range []struct{ id, href, class, text string }{
		{"n1", "/n/n1/", "neutral", "50%"},
		{"n2", "/n/n2/", "amber-low", "75%"},
		{"n3", "/n/n3/", "amber-full", "92%"},
		{"n4", "/n/n4/", "down", "down"},
		{"n5", "/n/n5/", "revoked", "revoked"},
	} {
		want := heatTile(tc.id, tc.class, tc.href, tc.text)
		if !strings.Contains(body, want) {
			t.Errorf("heatmap: expected tile %q, body:\n%s", want, body)
		}
	}
	// Ember (--crit's own class, .heat-down) is reserved for "down" alone --
	// the amber-full tile must never carry it, no matter how hot the value.
	if strings.Contains(body, `heat-tile heat-crit`) {
		t.Error("heatmap: no tile should ever carry an ember/crit band class as a metric colour")
	}
}

// fleetLoadBandRoster exercises the load metric's absolute thresholds (1, 4
// and 8 -- task-1a-brief.md: NodeSummary carries no core count, so the
// heatmap can't compute a per-core ratio and falls back to these).
func fleetLoadBandRoster() []core.NodeSummary {
	return []core.NodeSummary{
		{ID: "lo", Name: "lo", State: "online", Load1: 0.5},
		{ID: "mid", Name: "mid", State: "online", Load1: 2},
		{ID: "hi", Name: "hi", State: "online", Load1: 5},
	}
}

func TestFleetHeatmapLoadMetricAbsoluteThresholds(t *testing.T) {
	d := fleetMasterDeps(t, fleetLoadBandRoster())
	rr := fleetGetAsViewer(t, d, "/fleet?metric=load")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, tc := range []struct{ id, href, class, text string }{
		{"lo", "/n/lo/", "neutral", "0.50"},
		{"mid", "/n/mid/", "amber-low", "2.00"},
		{"hi", "/n/hi/", "amber-full", "5.00"},
	} {
		want := fmt.Sprintf(`<a class="heat-tile heat-%s" href="%s" data-heat-id="%s"><span class="heat-name">%s</span><span class="heat-val">%s</span></a>`,
			tc.class, tc.href, tc.id, tc.id, tc.text)
		if !strings.Contains(body, want) {
			t.Errorf("heatmap ?metric=load: expected tile %q, body:\n%s", want, body)
		}
	}
}

// TestFleetHeatmapMetricSelectorPreservesFilters pins "the metric selector is
// links, not a form; it preserves the other query params" -- ?tag=web&
// metric=mem must render the OTHER metric links (cpu/disk/load) still
// carrying tag=web, and the active metric's chip carries the "on" class.
func TestFleetHeatmapMetricSelectorPreservesFilters(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsViewer(t, d, "/fleet?tag=web&metric=mem")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := html.UnescapeString(rr.Body.String())
	for _, want := range []string{
		`href="/fleet?metric=cpu&tag=web"`,
		`href="/fleet?metric=disk&tag=web"`,
		`href="/fleet?metric=load&tag=web"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metric selector: expected %q to preserve tag=web, body:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `class="chip on" href="/fleet?metric=mem&tag=web"`) {
		t.Errorf("metric selector: expected the active mem chip to carry the \"on\" class, body:\n%s", body)
	}
}

// TestFleetHeatmapUnknownMetricFallsBackToCPU pins parseFleetMetric's
// documented default: an unrecognized ?metric= value degrades to cpu rather
// than 500ing or rendering an all-zero/blank ramp.
func TestFleetHeatmapUnknownMetricFallsBackToCPU(t *testing.T) {
	d := fleetMasterDeps(t, fleetHeatBandRoster())
	rr := fleetGetAsViewer(t, d, "/fleet?metric=bogus")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// n2's CPU is 75 (amber-low); n2 has no MemPct/WorstDiskPct/Load1 set,
	// so this tile would render "0%"/neutral instead if ?metric=bogus fell
	// through to anything other than cpu.
	want := `<a class="heat-tile heat-amber-low" href="/n/n2/" data-heat-id="n2"><span class="heat-name">n2</span><span class="heat-val">75%</span></a>`
	if !strings.Contains(body, want) {
		t.Errorf("?metric=bogus: expected fallback to cpu, missing %q\nbody:\n%s", want, body)
	}
	if !strings.Contains(html.UnescapeString(body), `class="chip on" href="/fleet?metric=cpu"`) {
		t.Errorf("?metric=bogus: expected the CPU chip to be the active one, body:\n%s", html.UnescapeString(body))
	}
}

// TestFleetHeatmapMetricSelectorEscapesQueryValue pins that the metric
// selector's links correctly re-encode a q= value containing characters
// that are meaningful in a URL (a space and a literal "&") -- fq.encode()
// followed by fleetPageQueryString's re-parse/re-encode round trip must not
// corrupt or truncate the value.
func TestFleetHeatmapMetricSelectorEscapesQueryValue(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	target := "/fleet?metric=mem&q=" + url.QueryEscape("a b&c")
	rr := fleetGetAsViewer(t, d, target)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := html.UnescapeString(rr.Body.String())
	want := `href="/fleet?metric=cpu&q=a+b%26c"`
	if !strings.Contains(body, want) {
		t.Errorf("metric selector: expected %q (q=%q correctly re-encoded), body:\n%s", want, "a b&c", body)
	}
}

// fleetTopNRoster gives every online node a distinct, non-overlapping value
// per metric (cpu/mem/disk) so a test can assert exactly which five nodes
// rank into each top-N panel without cross-panel ambiguity. down/revoked
// carry the highest raw values of all, to prove they're excluded from every
// ranking despite that.
func fleetTopNRoster() []core.NodeSummary {
	return []core.NodeSummary{
		{ID: "a", Name: "a", State: "online", CPU: 10, MemPct: 95, WorstDiskPct: 5},
		{ID: "b", Name: "b", State: "online", CPU: 95, MemPct: 10, WorstDiskPct: 50},
		{ID: "c", Name: "c", State: "online", CPU: 85, MemPct: 20, WorstDiskPct: 97},
		{ID: "d", Name: "d", State: "online", CPU: 75, MemPct: 30, WorstDiskPct: 15},
		{ID: "e", Name: "e", State: "online", CPU: 65, MemPct: 40, WorstDiskPct: 20},
		{ID: "f", Name: "f", State: "online", CPU: 55, MemPct: 50, WorstDiskPct: 25},
		{ID: "g", Name: "g", State: "online", CPU: 45, MemPct: 60, WorstDiskPct: 8},
		{ID: "downnode", Name: "downnode", State: "down", CPU: 100, MemPct: 100, WorstDiskPct: 100, LastSeen: 12345},
		{ID: "revoked1", Name: "revoked1", State: "revoked", CPU: 99, MemPct: 99, WorstDiskPct: 99},
	}
}

// TestFleetTopNPanelsRankAndExcludeDownRevoked pins the top-N panels' exact
// contract: top 5 by CPU/mem/worst-disk, descending, down/revoked excluded
// from every ranking regardless of their raw value.
func TestFleetTopNPanelsRankAndExcludeDownRevoked(t *testing.T) {
	d := fleetMasterDeps(t, fleetTopNRoster())
	rr := fleetGetAsViewer(t, d, "/fleet")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	hbar := func(class, id, name string, pct int) string {
		return fmt.Sprintf(`<a class="%s" href="/n/%s/"><span class="lbl">%s</span><span class="track"><i style="width:%d%%"></i></span><span class="v">%d%%</span></a>`,
			class, id, name, pct, pct)
	}

	// Top 5 CPU: b95 c85 d75 e65 f55 -- excludes a(10), g(45), downnode(100), revoked1(99).
	for _, want := range []string{
		hbar("hbar", "b", "b", 95), hbar("hbar", "c", "c", 85), hbar("hbar", "d", "d", 75),
		hbar("hbar", "e", "e", 65), hbar("hbar", "f", "f", 55),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("top-5 CPU: missing %q\nbody:\n%s", want, body)
		}
	}
	for _, absent := range []string{hbar("hbar", "a", "a", 10), hbar("hbar", "g", "g", 45)} {
		if strings.Contains(body, absent) {
			t.Errorf("top-5 CPU: unexpected %q present (should be excluded from top 5)\nbody:\n%s", absent, body)
		}
	}

	// Top 5 mem: a95 g60 f50 e40 d30 -- excludes b(10), c(20).
	for _, want := range []string{
		hbar("hbar mem", "a", "a", 95), hbar("hbar mem", "g", "g", 60), hbar("hbar mem", "f", "f", 50),
		hbar("hbar mem", "e", "e", 40), hbar("hbar mem", "d", "d", 30),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("top-5 mem: missing %q\nbody:\n%s", want, body)
		}
	}

	// Top 5 disk: c97 b50 f25 e20 d15 -- excludes a(5), g(8).
	for _, want := range []string{
		hbar("hbar", "c", "c", 97), hbar("hbar", "b", "b", 50), hbar("hbar", "f", "f", 25),
		hbar("hbar", "e", "e", 20), hbar("hbar", "d", "d", 15),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("top-5 disk: missing %q\nbody:\n%s", want, body)
		}
	}

	// down/revoked never appear in ANY of the three metric ranking panels,
	// despite the highest raw values of the whole roster -- scoped to the
	// panels' own hbar markup (class="hbar"/"hbar mem") so this doesn't
	// false-negative against downnode's legitimate appearance in the "Down
	// now" list or either node's appearance in the table/heatmap.
	for _, id := range []string{"downnode", "revoked1"} {
		for _, class := range []string{"hbar", "hbar mem"} {
			bad := `<a class="` + class + `" href="/n/` + id + `/">`
			if strings.Contains(body, bad) {
				t.Errorf("top-N panels: %s must not appear in any metric ranking (found %q)\nbody:\n%s", id, bad, body)
			}
		}
	}
}

// TestFleetDownNowPanelListsDownNodesWithLastSeen pins the "Down now" panel:
// every down node listed with its last-seen text, online nodes excluded.
func TestFleetDownNowPanelListsDownNodesWithLastSeen(t *testing.T) {
	nodes := []core.NodeSummary{
		{ID: core.SelfNodeID, Self: true, State: "online"},
		{ID: "d1", Name: "d1", State: "down", LastSeen: 500},
		{ID: "d2", Name: "d2", State: "down", LastSeen: 900},
		{ID: "up1", Name: "up1", State: "online"},
	}
	d := fleetMasterDeps(t, nodes)
	rr := fleetGetAsViewer(t, d, "/fleet")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, id := range []string{"d1", "d2"} {
		want := `<a class="row" href="/n/` + id + `/">`
		if !strings.Contains(body, want) {
			t.Errorf("down-now panel: missing row for %s, body:\n%s", id, body)
		}
	}
	if !strings.Contains(body, nodeAgoText(500)) {
		t.Errorf("down-now panel: missing last-seen text %q for d1, body:\n%s", nodeAgoText(500), body)
	}
	if strings.Contains(body, `<a class="row" href="/n/up1/">`) {
		t.Errorf("down-now panel: up1 (online) must not be listed, body:\n%s", body)
	}
}

// TestFleetHeatmapAndPanelsRespectFilters pins "the heatmap and the panels
// respect the page's existing filters" -- ?tag=web narrows both the table
// AND the heatmap to web1/web2 alone.
func TestFleetHeatmapAndPanelsRespectFilters(t *testing.T) {
	d := fleetMasterDeps(t, fleetFiveNodeRoster())
	rr := fleetGetAsViewer(t, d, "/fleet?tag=web")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, id := range []string{"web1", "web2"} {
		if !strings.Contains(body, `data-node-id="`+id+`"`) {
			t.Errorf("?tag=web: expected table row for %s, body:\n%s", id, body)
		}
		if !strings.Contains(body, `data-heat-id="`+id+`"`) {
			t.Errorf("?tag=web: expected heatmap tile for %s, body:\n%s", id, body)
		}
	}
	for _, id := range []string{"db1", "old1", core.SelfNodeID} {
		if strings.Contains(body, `data-node-id="`+id+`"`) {
			t.Errorf("?tag=web: unexpected table row for %s, body:\n%s", id, body)
		}
		if strings.Contains(body, `data-heat-id="`+id+`"`) {
			t.Errorf("?tag=web: unexpected heatmap tile for %s, body:\n%s", id, body)
		}
	}
}

// TestFleetHeatmapPanelsUseSingleNodesCall is task-1a-brief.md's own
// requirement: "Both render from the request-scoped memo's single Nodes()
// call. No extra Fleet() round trips; assert this with the existing
// counting fake."
func TestFleetHeatmapPanelsUseSingleNodesCall(t *testing.T) {
	fakeF := &fakeFleet{status: core.FleetStatus{Role: config.RoleMaster}, nodes: fleetFiveNodeRoster()}
	cf, statusCalls, nodesCalls := newCountingFleet(fakeF)
	d := fleetTestDeps(t, masterFakeAPI(fakeF, nil))
	d.Fleet = func() core.FleetAPI { return cf }

	rr := fleetGetAsViewer(t, d, "/fleet")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if *nodesCalls > 1 {
		t.Errorf("GET /fleet (heatmap+top-N panels): Nodes() called %d times, want <=1", *nodesCalls)
	}
	if *statusCalls > 1 {
		t.Errorf("GET /fleet: Status() called %d times, want <=1", *statusCalls)
	}
}

// fleetBigRoster builds an n-node roster with varied state/metric values for
// the performance test below.
func fleetBigRoster(n int) []core.NodeSummary {
	nodes := make([]core.NodeSummary, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("node%03d", i)
		state := "online"
		switch {
		case i%37 == 0:
			state = "down"
		case i%53 == 0:
			state = "revoked"
		case i%11 == 0:
			state = "lagging"
		}
		nodes = append(nodes, core.NodeSummary{
			ID: id, Name: id, State: state,
			CPU: float64(i % 100), MemPct: float64((i * 3) % 100), WorstDiskPct: float64((i * 7) % 100),
			Load1: float64(i%10) / 2, LastSeen: int64(1000 + i),
		})
	}
	return nodes
}

// TestFleetOverviewPerformance200Nodes pins task-1a-brief.md's performance
// bound: a 200-node fake renders /fleet in under 300ms server-side, measured
// as the median over 3 runs (a generous, CI-safe bound).
func TestFleetOverviewPerformance200Nodes(t *testing.T) {
	d := fleetMasterDeps(t, fleetBigRoster(200))
	const runs = 3
	durs := make([]time.Duration, runs)
	for i := 0; i < runs; i++ {
		start := time.Now()
		rr := fleetGetAsViewer(t, d, "/fleet")
		durs[i] = time.Since(start)
		if rr.Code != http.StatusOK {
			t.Fatalf("run %d: status = %d, want 200, body: %s", i, rr.Code, rr.Body.String())
		}
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	median := durs[runs/2]
	if median > 300*time.Millisecond {
		t.Errorf("median render time for 200 nodes = %v, want <= 300ms (all runs: %v)", median, durs)
	}
}
