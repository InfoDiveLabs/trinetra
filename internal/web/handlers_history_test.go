//go:build web

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSeriesStore is a minimal SeriesStore test double: data maps a metric
// name straight to the points Query should return for it (ignoring
// from/to/err plumbing unless the test needs otherwise), and err (if set) is
// returned verbatim from every Query call.
type fakeSeriesStore struct {
	data map[string][]SeriesPoint
	err  error
}

func (f *fakeSeriesStore) Query(metric string, from, to int64) ([]SeriesPoint, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.data[metric], nil
}

// historyTestDeps mirrors dashboardTestDeps/enrollTestDeps: a fresh StateDir
// plus the given SeriesStore (nil is valid — see Deps.Store's doc).
func historyTestDeps(t *testing.T, store SeriesStore) Deps {
	t.Helper()
	d := enrollTestDeps(t)
	d.Store = store
	return d
}

// TestSeriesAPIReturnsPointsForValidRange pins the core TDD obligation: a
// valid metric/range against a populated fake store returns the points as
// uPlot-shaped JSON (parallel arrays: ts, avg, min, max).
func TestSeriesAPIReturnsPointsForValidRange(t *testing.T) {
	store := &fakeSeriesStore{data: map[string][]SeriesPoint{
		"cpu": {
			{TS: 1000, Min: 1, Avg: 2, Max: 3},
			{TS: 2000, Min: 4, Avg: 5, Max: 6},
		},
	}}
	d := historyTestDeps(t, store)
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

// TestSeriesAPIUnknownMetricReturnsEmptyNot500 pins the "unknown metric ->
// empty series, 200, NOT 500" requirement: a metric the fake store has no
// data for must render as an empty series, never an error status.
func TestSeriesAPIUnknownMetricReturnsEmptyNot500(t *testing.T) {
	store := &fakeSeriesStore{data: map[string][]SeriesPoint{"cpu": {{TS: 1, Avg: 1}}}}
	d := historyTestDeps(t, store)
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
	store := &fakeSeriesStore{data: map[string][]SeriesPoint{"cpu": {{TS: 1, Avg: 1}}}}
	d := historyTestDeps(t, store)
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

// TestHistoryPageRendersAppShellAndChartHooks pins GET /history: it must
// render through the full app-shell layout (base.html, nav/topbar) — unlike
// /login or /enroll's bare layout — with the metric-picker/time-range chips
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
