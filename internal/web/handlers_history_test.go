package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"serverwatch/internal/core"
)

// fakeEventsStore is a minimal EventsStore test double: events is returned
// verbatim from Events (ignoring the from/to filter unless err is set). It's
// no longer used to build Deps.API here (fakeAPI, deps_api_test.go, covers
// that, since /api/downtime reads through Deps.API now, Task 5) but stays
// for availability_test.go, which exercises core.ComputeAvailability
// directly against a core.EventsSource.
type fakeEventsStore struct {
	events []DownEventView
	err    error
}

func (f *fakeEventsStore) Events(from, to int64) ([]DownEventView, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.events, nil
}

// historyTestDeps mirrors dashboardTestDeps/enrollTestDeps: a fresh StateDir
// plus the given core.API (nil is valid, since the history/series/downtime
// handlers all treat a nil Deps.API as "no data", mirroring the pre-Task-5
// "nil store" contract).
func historyTestDeps(t *testing.T, api core.API) Deps {
	t.Helper()
	d := enrollTestDeps(t)
	d.API = api
	return d
}

// TestSeriesAPIReturnsPointsForValidRange pins the core TDD obligation: a
// valid metric/range against a populated fake store returns the points as
// uPlot-shaped JSON (parallel arrays: ts, avg, min, max).
func TestSeriesAPIReturnsPointsForValidRange(t *testing.T) {
	api := fakeAPI{series: map[string][]SeriesPoint{
		"cpu": {
			{TS: 1000, Min: 1, Avg: 2, Max: 3},
			{TS: 2000, Min: 4, Avg: 5, Max: 6},
		},
	}}
	d := historyTestDeps(t, api)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/series?metric=cpu&from=500&to=2500")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp seriesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if resp.Metric != "cpu" {
		t.Errorf("metric = %q, want cpu", resp.Metric)
	}
	if len(resp.Series) < 2 || len(resp.Series[0]) != 2 || len(resp.Series[1]) != 2 {
		t.Fatalf("series shape = %+v, want 2 points on ts+avg", resp.Series)
	}
	if resp.Series[0][0] != 1000 || resp.Series[0][1] != 2000 {
		t.Errorf("ts values = %v, want [1000 2000]", resp.Series[0])
	}
	if resp.Series[1][0] != 2 || resp.Series[1][1] != 5 {
		t.Errorf("avg values = %v, want [2 5]", resp.Series[1])
	}
}

// TestSeriesAPIReturnsLoadAndDiskMetrics pins that the load5/load15 and
// per-mount disk:<mount> series (the mockup history.html shows a full
// 1m/5m/15m load chart and a per-filesystem disk-usage panel, both dropped
// by the first port) come back through the same /api/series endpoint just
// like cpu -- the whole restore leans on these being ordinary queryable
// metrics, nothing special-cased.
func TestSeriesAPIReturnsLoadAndDiskMetrics(t *testing.T) {
	api := fakeAPI{series: map[string][]SeriesPoint{
		"load5":      {{TS: 1000, Avg: 0.5}},
		"load15":     {{TS: 1000, Avg: 0.25}},
		"disk:/":     {{TS: 1000, Avg: 91}},
		"disk:/data": {{TS: 1000, Avg: 22}},
	}}
	d := historyTestDeps(t, api)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	for metric, wantAvg := range map[string]float64{
		"load5":      0.5,
		"load15":     0.25,
		"disk:/":     91,
		"disk:/data": 22,
	} {
		req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/series?metric="+metric+"&from=500&to=1500")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("metric %q status = %d, want 200, body: %s", metric, rr.Code, rr.Body.String())
		}
		var resp seriesResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("metric %q decode: %v", metric, err)
		}
		if len(resp.Series) < 2 || len(resp.Series[1]) != 1 || resp.Series[1][0] != wantAvg {
			t.Errorf("metric %q avg series = %+v, want [%v]", metric, resp.Series, wantAvg)
		}
	}
}

// TestSeriesAPIUnknownMetricReturnsEmptyNot500 pins the "unknown metric ->
// empty series, 200, NOT 500" requirement: a metric the fake store has no
// data for must render as an empty series, never an error status.
func TestSeriesAPIUnknownMetricReturnsEmptyNot500(t *testing.T) {
	api := fakeAPI{series: map[string][]SeriesPoint{"cpu": {{TS: 1, Avg: 1}}}}
	d := historyTestDeps(t, api)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/series?metric=does-not-exist&from=1&to=100")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp seriesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if len(resp.Series) == 0 || len(resp.Series[0]) != 0 {
		t.Errorf("series = %+v, want empty", resp.Series)
	}
}

// TestSeriesAPINilStoreReturnsEmpty pins the "nil store -> empty series, not
// a panic" requirement.
func TestSeriesAPINilStoreReturnsEmpty(t *testing.T) {
	d := historyTestDeps(t, nil)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/series?metric=cpu&from=1&to=100")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp seriesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if len(resp.Series) == 0 || len(resp.Series[0]) != 0 {
		t.Errorf("series = %+v, want empty", resp.Series)
	}
}

// TestSeriesAPIInvalidRangeRejected pins the input-validation requirement:
// from>=to, unparseable timestamps, and an absurdly large span all reject
// with 400 rather than reaching the store at all.
func TestSeriesAPIInvalidRangeRejected(t *testing.T) {
	api := fakeAPI{series: map[string][]SeriesPoint{"cpu": {{TS: 1, Avg: 1}}}}
	d := historyTestDeps(t, api)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	cases := []struct {
		name  string
		query string
	}{
		{"from equals to", "metric=cpu&from=100&to=100"},
		{"from greater than to", "metric=cpu&from=200&to=100"},
		{"from unparseable", "metric=cpu&from=not-a-number&to=100"},
		{"to unparseable", "metric=cpu&from=1&to=not-a-number"},
		{"from missing", "metric=cpu&to=100"},
		{"to missing", "metric=cpu&from=1"},
		{"absurdly large span", "metric=cpu&from=0&to=99999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/series?"+tc.query)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestHistoryAndSeriesRoutesAreViewerGated mirrors
// TestDashboardRoutesAreViewerGated (handlers_dashboard_test.go): an
// anonymous request to either route must redirect to /login, exactly like
// every other viewer+ route requireRole gates.
func TestHistoryAndSeriesRoutesAreViewerGated(t *testing.T) {
	d := historyTestDeps(t, nil)
	h := newHandler(d)
	for _, route := range []string{"/history", "/api/series?metric=cpu&from=1&to=100"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, route, nil))
		if rr.Code != http.StatusFound {
			t.Errorf("GET %s anon status = %d, want %d", route, rr.Code, http.StatusFound)
		}
		if loc := rr.Header().Get("Location"); loc != "/login" {
			t.Errorf("GET %s anon Location = %q, want /login", route, loc)
		}
	}
}

// TestDowntimeAPIReturnsEventsForValidRange pins the /api/downtime endpoint:
// a valid range against a populated fake EventsStore returns the downtime
// events as JSON for the mockup's "Downtime · 30d" timeline/rows.
func TestDowntimeAPIReturnsEventsForValidRange(t *testing.T) {
	d := historyTestDeps(t, fakeAPI{events: []DownEventView{
		{Type: "power_down", Start: 1000, End: 1600, DurationSec: 600},
		{Type: "net_down", Start: 2000, End: 2060, DurationSec: 60},
	}})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?from=500&to=2500")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp downtimeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events = %+v, want 2", resp.Events)
	}
	if resp.Events[0].Type != "power_down" || resp.Events[0].Start != 1000 || resp.Events[0].DurationSec != 600 {
		t.Errorf("events[0] = %+v, want power_down 1000..1600 600s", resp.Events[0])
	}
}

// TestDowntimeAPINilStoreReturnsEmpty pins the "nil Deps.API -> empty list,
// not a panic" requirement (mirrors TestSeriesAPINilStoreReturnsEmpty).
func TestDowntimeAPINilStoreReturnsEmpty(t *testing.T) {
	d := historyTestDeps(t, nil) // leaves d.API nil
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?from=1&to=100")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp downtimeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if len(resp.Events) != 0 {
		t.Errorf("events = %+v, want empty", resp.Events)
	}
}

// TestDowntimeAPIInvalidRangeRejected pins that /api/downtime validates its
// from/to range exactly like /api/series (parseSeriesRange).
func TestDowntimeAPIInvalidRangeRejected(t *testing.T) {
	d := historyTestDeps(t, fakeAPI{events: []DownEventView{{Type: "net_down", Start: 1, End: 2}}})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	for _, query := range []string{
		"from=100&to=100",
		"from=200&to=100",
		"from=x&to=100",
		"from=1",
		"to=100",
		"from=0&to=99999999999",
	} {
		req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?"+query)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("query %q status = %d, want 400", query, rr.Code)
		}
	}
}

// TestDowntimeRouteIsViewerGated pins that /api/downtime is viewer+ like the
// rest of the history surface: anon -> redirect to /login.
func TestDowntimeRouteIsViewerGated(t *testing.T) {
	d := historyTestDeps(t, nil)
	h := newHandler(d)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/downtime?from=1&to=100", nil))
	if rr.Code != http.StatusFound {
		t.Errorf("anon status = %d, want %d", rr.Code, http.StatusFound)
	}
	if loc := rr.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

// TestHistoryPageRendersDiskAndDowntimeSections pins that the restored
// mockup sections are present: a per-mount disk panel driven off the current
// snapshot's mounts, and a downtime panel the client fills from
// /api/downtime.
func TestHistoryPageRendersDiskAndDowntimeSections(t *testing.T) {
	d := historyTestDeps(t, nil)
	d.Snapshot = func() DashboardView {
		return DashboardView{Disks: []DiskView{
			{Mount: "/", UsagePct: 91},
			{Mount: "/data", UsagePct: 22},
		}}
	}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/history")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`data-metrics="load1,load5,load15"`, // full load chart
		`disk:/`,                            // disk series metric for mount /
		`disk:/data`,                        // disk series metric for mount /data
		`data-downtime`,                     // downtime panel hook
	} {
		if !strings.Contains(body, want) {
			t.Errorf("history page missing restored-section marker %q:\n%s", want, body)
		}
	}
}

// TestHistoryPageRendersAppShellAndChartHooks pins GET /history: it must
// render through the full app-shell layout (base.html, nav/topbar) -- unlike
// /login or /enroll's bare layout -- with the metric-picker/time-range chips
// and one chart container per metric app.js's swBootHistoryCharts targets.
func TestHistoryPageRendersAppShellAndChartHooks(t *testing.T) {
	d := historyTestDeps(t, nil)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/history")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`class="side"`,
		`class="topbar"`,
		`data-history`,
		`data-range="24h"`,
		`data-metric="cpu"`,
		`data-metric="mem"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("history page missing %q:\n%s", want, body)
		}
	}
}
