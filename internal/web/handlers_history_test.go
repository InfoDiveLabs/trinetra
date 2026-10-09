package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fakeEventsStore is a minimal EventsStore test double: events is returned verbatim from
// Events (ignoring the from/to filter unless err is set).
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

// historyTestDeps mirrors dashboardTestDeps/enrollTestDeps: a fresh StateDir plus the given
// core.API.
func historyTestDeps(t *testing.T, api core.API) Deps {
	t.Helper()
	d := enrollTestDeps(t)
	d.API = api
	return d
}

// TestSeriesAPIReturnsPointsForValidRange pins the core TDD obligation: a valid
// metric/range against a populated fake store returns the points as uPlot-shaped JSON.
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

// TestSeriesAPIReturnsLoadAndDiskMetrics pins that the load5/load15 and per-mount
// disk:<mount> series.
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

// TestSeriesAPIUnknownMetricReturnsEmptyNot500 pins the "unknown metric -> empty series,
// 200, NOT 500" requirement.
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

// TestSeriesAPIInvalidRangeRejected pins the input-validation requirement: from>=to,
// unparseable timestamps.
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

// TestHistoryAndSeriesRoutesAreViewerGated mirrors TestDashboardRoutesAreViewerGated
// (handlers_dashboard_test.go).
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

// TestDowntimeAPIReturnsEventsForValidRange pins the /api/downtime endpoint.
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
	if resp.Events[0].Type != "net_down" || resp.Events[0].Start != 2000 || resp.Events[0].DurationSec != 60 {
		t.Errorf("events[0] = %+v, want net_down 2000..2060 60s (newest first)", resp.Events[0])
	}
	if resp.Events[1].Type != "power_down" || resp.Events[1].Start != 1000 {
		t.Errorf("events[1] = %+v, want power_down 1000..1600 (oldest last)", resp.Events[1])
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
	if resp.TotalSeconds != 660 {
		t.Errorf("total_seconds = %d, want 660", resp.TotalSeconds)
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

// downtimeEventsFixture builds n synthetic downtime events spaced 100s apart starting at
// base, each lasting durSec, for the pagination tests below.
func downtimeEventsFixture(n int, base int64, durSec int64) []DownEventView {
	evs := make([]DownEventView, n)
	for i := 0; i < n; i++ {
		start := base + int64(i)*100
		evs[i] = DownEventView{Type: "net_down", Start: start, End: start + durSec, DurationSec: durSec}
	}
	return evs
}

// TestDowntimeAPIDefaultsLimitAndOffset pins the paging defaults: no limit/offset given ->
// limit=50, offset=0, and (since 5 < 50) every event comes back on the one page.
func TestDowntimeAPIDefaultsLimitAndOffset(t *testing.T) {
	evs := downtimeEventsFixture(5, 1000, 60)
	d := historyTestDeps(t, fakeAPI{events: evs})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?from=0&to=100000")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp downtimeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if resp.Limit != 50 {
		t.Errorf("limit = %d, want default 50", resp.Limit)
	}
	if resp.Offset != 0 {
		t.Errorf("offset = %d, want default 0", resp.Offset)
	}
	if resp.Total != 5 {
		t.Errorf("total = %d, want 5", resp.Total)
	}
	if len(resp.Events) != 5 {
		t.Errorf("events = %d, want all 5 on the one page", len(resp.Events))
	}
}

// TestDowntimeAPILimitOffsetSlicesNewestFirst pins the core paging contract: events are
// sorted newest-first (by Start descending) and then sliced by offset/limit.
func TestDowntimeAPILimitOffsetSlicesNewestFirst(t *testing.T) {
	evs := downtimeEventsFixture(10, 1000, 60) // starts 1000,1100,...,1900
	// Feed the fake store in reverse (oldest-last-in-store) to prove the
	// handler sorts rather than trusting store order.
	reversed := make([]DownEventView, len(evs))
	for i, e := range evs {
		reversed[len(evs)-1-i] = e
	}
	d := historyTestDeps(t, fakeAPI{events: reversed})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?from=0&to=100000&limit=3&offset=2")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp downtimeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if resp.Limit != 3 || resp.Offset != 2 {
		t.Errorf("limit/offset = %d/%d, want 3/2", resp.Limit, resp.Offset)
	}
	if resp.Total != 10 {
		t.Errorf("total = %d, want 10 (full range count, not page size)", resp.Total)
	}
	// Newest-first full order is starts 1900,1800,...,1000; offset=2,limit=3
	// -> starts 1700,1600,1500.
	wantStarts := []int64{1700, 1600, 1500}
	if len(resp.Events) != len(wantStarts) {
		t.Fatalf("events = %+v, want %d events", resp.Events, len(wantStarts))
	}
	for i, want := range wantStarts {
		if resp.Events[i].Start != want {
			t.Errorf("events[%d].Start = %d, want %d", i, resp.Events[i].Start, want)
		}
	}
}

// TestDowntimeAPITotalSecondsCoversFullRangeNotJustPage pins that total_seconds (the "total
// Xs · YY.YY%" summary's basis) is computed over every event in [from,to].
func TestDowntimeAPITotalSecondsCoversFullRangeNotJustPage(t *testing.T) {
	evs := downtimeEventsFixture(10, 1000, 60) // 10 events * 60s = 600s total
	d := historyTestDeps(t, fakeAPI{events: evs})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?from=0&to=100000&limit=2&offset=0")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp downtimeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if resp.TotalSeconds != 600 {
		t.Errorf("total_seconds = %d, want 600 (10 events * 60s, independent of limit=2)", resp.TotalSeconds)
	}
}

// TestDowntimeAPIOffsetBeyondTotalReturnsEmptyPage pins that an offset past
// the end of the range is a normal empty page (200), not an error.
func TestDowntimeAPIOffsetBeyondTotalReturnsEmptyPage(t *testing.T) {
	evs := downtimeEventsFixture(3, 1000, 60)
	d := historyTestDeps(t, fakeAPI{events: evs})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?from=0&to=100000&limit=10&offset=50")
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
		t.Errorf("events = %+v, want empty page", resp.Events)
	}
	if resp.Total != 3 {
		t.Errorf("total = %d, want 3", resp.Total)
	}
}

// TestDowntimeAPIInvalidLimitOffsetRejected pins the limit/offset validation contract:
// non-numeric, out-of-bounds.
func TestDowntimeAPIInvalidLimitOffsetRejected(t *testing.T) {
	d := historyTestDeps(t, fakeAPI{events: downtimeEventsFixture(3, 1000, 60)})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	cases := []struct {
		name  string
		query string
	}{
		{"limit not a number", "from=0&to=100000&limit=abc"},
		{"limit zero", "from=0&to=100000&limit=0"},
		{"limit negative", "from=0&to=100000&limit=-1"},
		{"limit over max", "from=0&to=100000&limit=501"},
		{"offset not a number", "from=0&to=100000&offset=abc"},
		{"offset negative", "from=0&to=100000&offset=-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?"+tc.query)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("query %q status = %d, want 400, body: %s", tc.query, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestDowntimeAPIMaxLimitAccepted pins that limit=500 (the documented max)
// is accepted, not rejected as "over max".
func TestDowntimeAPIMaxLimitAccepted(t *testing.T) {
	d := historyTestDeps(t, fakeAPI{events: downtimeEventsFixture(1, 1000, 60)})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?from=0&to=100000&limit=500")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
}

// TestNodeScopedDowntimeAPIPagination pins that the node-scoped route
// (/n/<id>/api/downtime, fleet routing) honors limit/offset exactly like the master route.
func TestNodeScopedDowntimeAPIPagination(t *testing.T) {
	child := fakeAPI{events: downtimeEventsFixture(5, 1000, 60)}
	master := fakeAPI{events: downtimeEventsFixture(1, 1, 1)}
	d := nodeScopedDeps(t, master, child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/api/downtime?from=0&to=100000&limit=2&offset=1")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp downtimeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if resp.Total != 5 {
		t.Errorf("total = %d, want 5 (child1's count)", resp.Total)
	}
	if len(resp.Events) != 2 {
		t.Errorf("events = %+v, want 2 (limit=2)", resp.Events)
	}
}

// TestDowntimeAPITimelineHasFixedBucketCount pins that the response's Timeline always has
// exactly downtimeTimelineBuckets entries covering the full requested range.
func TestDowntimeAPITimelineHasFixedBucketCount(t *testing.T) {
	cases := []struct {
		name string
		api  core.API
	}{
		{"nil API", nil},
		{"no events", fakeAPI{}},
		{"many events", fakeAPI{events: downtimeEventsFixture(40, 1000, 60)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := historyTestDeps(t, tc.api)
			h := newHandler(d)
			users := newUserStore(d.StateDir)
			sessions := newSessionStore(d.StateDir)

			req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?from=0&to=100000")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
			}
			var resp downtimeResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
			}
			if len(resp.Timeline) != downtimeTimelineBuckets {
				t.Fatalf("len(timeline) = %d, want %d", len(resp.Timeline), downtimeTimelineBuckets)
			}
			if resp.Timeline[0].Start != 0 {
				t.Errorf("timeline[0].Start = %d, want 0 (=from)", resp.Timeline[0].Start)
			}
			if last := resp.Timeline[len(resp.Timeline)-1].End; last != 100000 {
				t.Errorf("timeline[last].End = %d, want 100000 (=to)", last)
			}
		})
	}
}

// TestDowntimeAPITimelineClipsAtRangeAndBucketEdges pins the clipping contract: an event
// that starts before `from`, an event that's still open (End 0, clipped through `to`).
func TestDowntimeAPITimelineClipsAtRangeAndBucketEdges(t *testing.T) {
	if 1200%downtimeTimelineBuckets != 0 {
		t.Fatalf("test assumes 1200 divides evenly by downtimeTimelineBuckets(=%d)", downtimeTimelineBuckets)
	}
	bucketWidth := int64(1200 / downtimeTimelineBuckets)

	events := []DownEventView{
		// Starts before `from`(0): only [0,50) should count (clipped from
		// [-100,50)), spread across the first 5 buckets (10s each).
		{Type: "net_down", Start: -100, End: 50, DurationSec: 150},
		// Crosses a bucket boundary at t=10: [5,25) contributes 5s to bucket 0 ([0,10)), 10s to
		// bucket 1 ([10,20)), 5s to bucket 2 ([20,30)).
		{Type: "power_down", Start: 5, End: 25, DurationSec: 20},
		// Still open (End 0): clipped through `to`(1200), landing entirely
		// in the last bucket ([1190,1200)).
		{Type: "net_down", Start: 1190, End: 0, DurationSec: 999999},
	}
	d := historyTestDeps(t, fakeAPI{events: events})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?from=0&to=1200")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp downtimeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if len(resp.Timeline) != downtimeTimelineBuckets {
		t.Fatalf("len(timeline) = %d, want %d", len(resp.Timeline), downtimeTimelineBuckets)
	}

	// bucket 0 ([0,10)): 10s from the range-clipped first event, + 5s from
	// the boundary-crossing second event = 15s.
	if got := resp.Timeline[0].DownSeconds; got != bucketWidth+5 {
		t.Errorf("timeline[0].DownSeconds = %d, want %d", got, bucketWidth+5)
	}
	// bucket 1 ([10,20)): 10s from the first event + 10s from the second event = 20s.
	if got := resp.Timeline[1].DownSeconds; got != 2*bucketWidth {
		t.Errorf("timeline[1].DownSeconds = %d, want %d", got, 2*bucketWidth)
	}
	// bucket 2 ([20,30)): 10s from the first event + 5s from the second
	// event (which ends at 25) = 15s.
	if got := resp.Timeline[2].DownSeconds; got != bucketWidth+5 {
		t.Errorf("timeline[2].DownSeconds = %d, want %d", got, bucketWidth+5)
	}
	// buckets 3,4 ([30,50)): 10s each from the range-clipped first event only.
	for _, i := range []int{3, 4} {
		if got := resp.Timeline[i].DownSeconds; got != bucketWidth {
			t.Errorf("timeline[%d].DownSeconds = %d, want %d", i, got, bucketWidth)
		}
	}
	// bucket 5 ([50,60)): nothing -- the first event ended exactly at 50.
	if got := resp.Timeline[5].DownSeconds; got != 0 {
		t.Errorf("timeline[5].DownSeconds = %d, want 0", got)
	}
	// last bucket ([1190,1200)): the open event, clipped through `to`.
	last := downtimeTimelineBuckets - 1
	if got := resp.Timeline[last].DownSeconds; got != bucketWidth {
		t.Errorf("timeline[%d].DownSeconds = %d, want %d", last, got, bucketWidth)
	}
	if resp.Timeline[last].Type != "net_down" {
		t.Errorf("timeline[%d].Type = %q, want net_down", last, resp.Timeline[last].Type)
	}
	// bucket 1 overlaps both a net_down (bucket 0's first event, spilling
	// in) and the power_down boundary-crosser -- power_down must win.
	if resp.Timeline[1].Type != "power_down" {
		t.Errorf("timeline[1].Type = %q, want power_down (crit wins over warn)", resp.Timeline[1].Type)
	}

	// The event(s) fully or partly outside [from,to] must still be clipped the same way in
	// TotalSeconds, not counted at raw duration_sec: total clipped seconds = 50.
	if resp.TotalSeconds != 80 {
		t.Errorf("total_seconds = %d, want 80 (clipped, not raw duration_sec)", resp.TotalSeconds)
	}
}

// TestDowntimeAPITimelineSumMatchesTotalSeconds pins the invariant the controller asked for
// explicitly: summing every bucket's DownSeconds must equal TotalSeconds, for an arbitrary.
func TestDowntimeAPITimelineSumMatchesTotalSeconds(t *testing.T) {
	events := downtimeEventsFixture(37, 500, 42) // arbitrary, not aligned to bucket width
	d := historyTestDeps(t, fakeAPI{events: events})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?from=0&to=100000")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp downtimeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	var sum int64
	for _, b := range resp.Timeline {
		sum += b.DownSeconds
	}
	if sum != resp.TotalSeconds {
		t.Errorf("sum(timeline.down_seconds) = %d, total_seconds = %d, want equal", sum, resp.TotalSeconds)
	}
	if resp.TotalSeconds == 0 {
		t.Fatal("total_seconds = 0, test fixture should produce nonzero downtime")
	}
}

// TestDowntimeAPITimelineIndependentOfLimitOffset pins that Timeline (and
// TotalSeconds/Total) never changes with limit/offset.
func TestDowntimeAPITimelineIndependentOfLimitOffset(t *testing.T) {
	events := downtimeEventsFixture(40, 1000, 60)
	d := historyTestDeps(t, fakeAPI{events: events})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	fetch := func(query string) downtimeResponse {
		t.Helper()
		req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/downtime?from=0&to=100000&"+query)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("query %q status = %d, want 200, body: %s", query, rr.Code, rr.Body.String())
		}
		var resp downtimeResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("query %q decode: %v", query, err)
		}
		return resp
	}

	small := fetch("limit=2&offset=0")
	big := fetch("limit=500&offset=0")
	offsetted := fetch("limit=2&offset=30")

	for _, pair := range []struct {
		name string
		got  downtimeResponse
	}{{"limit=500", big}, {"offset=30", offsetted}} {
		if len(pair.got.Timeline) != len(small.Timeline) {
			t.Fatalf("%s: len(timeline) = %d, want %d (same as limit=2)", pair.name, len(pair.got.Timeline), len(small.Timeline))
		}
		for i := range small.Timeline {
			if pair.got.Timeline[i] != small.Timeline[i] {
				t.Errorf("%s: timeline[%d] = %+v, want %+v (identical to limit=2's)", pair.name, i, pair.got.Timeline[i], small.Timeline[i])
			}
		}
		if pair.got.TotalSeconds != small.TotalSeconds {
			t.Errorf("%s: total_seconds = %d, want %d", pair.name, pair.got.TotalSeconds, small.TotalSeconds)
		}
		if pair.got.Total != small.Total {
			t.Errorf("%s: total = %d, want %d", pair.name, pair.got.Total, small.Total)
		}
	}
}

// TestHistoryPageRendersDiskAndDowntimeSections pins that the page carries a per-mount disk
// panel driven off the current snapshot's mounts.
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

// TestHistoryPageRendersAppShellAndChartHooks pins GET /history: it must render through the
// full app-shell layout (base.html, nav/topbar) -- unlike /login or /enroll's bare layout.
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
