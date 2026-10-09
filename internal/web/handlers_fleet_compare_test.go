package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// ---- the /fleet checkbox selection must survive the table's 5s htmx poll ----

// TestAppJSFleetCompareSurvivesTablePoll is a static source scan (same
// convention as templates_node_test.go's TestAppJSDataFetchesGoThroughNodeURL:
// read assets/app.js's embedded source, assert the expected code shapes are
// present) pinning that a persisted selection Set survives #fleet-tbody's
// every-5s outerHTML swap (which replaces every checkbox with a fresh,
// unchecked one), re-applied onto the fresh checkboxes on htmx:afterSwap
// before the counter/button are refreshed.
func TestAppJSFleetCompareSurvivesTablePoll(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(b)

	if !strings.Contains(src, "selectedNodeIds") {
		t.Fatal("app.js: missing a persisted fleet-compare selection set (selectedNodeIds)")
	}
	if !strings.Contains(src, "new Set()") {
		t.Fatal("app.js: selectedNodeIds must be a Set, not re-derived from the (about to be replaced) checkboxes")
	}
	if !strings.Contains(src, "function reapplySelection") {
		t.Fatal("app.js: missing a reapplySelection() function to re-check fresh checkboxes after a poll")
	}
	if !strings.Contains(src, "cb.checked=selectedNodeIds.has(cb.value)") {
		t.Fatal("app.js: reapplySelection must set each fresh checkbox's .checked from selectedNodeIds")
	}

	// The htmx:afterSwap handler for #fleet-tbody must call reapplySelection before refreshing
	// the counter/button -- extract that handler's body and check both calls appear in it.
	swapIdx := strings.Index(src, "'htmx:afterSwap'")
	if swapIdx < 0 {
		t.Fatal("app.js: missing an htmx:afterSwap listener")
	}
	body := src[swapIdx:]
	if end := strings.Index(body, "});"); end >= 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "fleet-tbody") {
		t.Fatal("app.js: htmx:afterSwap handler doesn't gate on #fleet-tbody")
	}
	reapplyAt := strings.Index(body, "reapplySelection()")
	refreshAt := strings.Index(body, "refresh()")
	if reapplyAt < 0 || refreshAt < 0 || reapplyAt > refreshAt {
		t.Fatalf("app.js: htmx:afterSwap must call reapplySelection() before refresh() (reapplyAt=%d refreshAt=%d)", reapplyAt, refreshAt)
	}
}

// fleetCompareMasterDeps builds a master Deps whose Fleet().FleetSeries returns pts (or
// seriesErr, if set).
func fleetCompareMasterDeps(t *testing.T, nodes []core.NodeSummary, pts []core.FleetSeriesPoint, seriesErr error) (Deps, *fakeFleet) {
	t.Helper()
	fk := &fakeFleet{status: core.FleetStatus{Role: config.RoleMaster}, nodes: nodes, series: pts, seriesErr: seriesErr}
	return fleetTestDeps(t, masterFakeAPI(fk, nil)), fk
}

// TestFleetCompareRendersSingleFleetSeriesCall pins that a compare request makes exactly
// one FleetSeries call, with the chart's data embedded in the page.
func TestFleetCompareRendersSingleFleetSeriesCall(t *testing.T) {
	pts := []core.FleetSeriesPoint{
		{Node: "web1", TS: 1000, Value: 10},
		{Node: "web2", TS: 1000, Value: 30},
	}
	d, fk := fleetCompareMasterDeps(t, fleetFiveNodeRoster(), pts, nil)
	rr := fleetGetAsViewer(t, d, "/fleet/compare?nodes=web1,web2&metric=cpu&range=6h")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet/compare status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if fk.seriesCalls != 1 {
		t.Fatalf("FleetSeries calls = %d, want 1", fk.seriesCalls)
	}
	if fk.lastSeriesMetric != "cpu" || fk.lastSeriesAgg != core.AggNone {
		t.Fatalf("FleetSeries called with metric=%q agg=%q, want cpu/none", fk.lastSeriesMetric, fk.lastSeriesAgg)
	}
	if len(fk.lastSeriesFilter.Nodes) != 2 || fk.lastSeriesFilter.Nodes[0] != "web1" || fk.lastSeriesFilter.Nodes[1] != "web2" {
		t.Fatalf("FleetSeries filter = %+v, want Nodes=[web1 web2]", fk.lastSeriesFilter)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `id="chart-fleet-compare"`) {
		t.Errorf("GET /fleet/compare: missing chart mount, body:\n%s", body)
	}
	if !strings.Contains(body, "web1") || !strings.Contains(body, "web2") {
		t.Errorf("GET /fleet/compare: missing node names in legend/data, body:\n%s", body)
	}
}

// TestFleetCompareTagSelection pins the ?tag= form building a Tag filter.
func TestFleetCompareTagSelection(t *testing.T) {
	d, fk := fleetCompareMasterDeps(t, fleetFiveNodeRoster(), nil, nil)
	rr := fleetGetAsViewer(t, d, "/fleet/compare?tag=web&metric=mem&range=1h")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if fk.seriesCalls != 1 || fk.lastSeriesFilter.Tag != "web" || fk.lastSeriesMetric != "mem" {
		t.Fatalf("FleetSeries call = calls=%d filter=%+v metric=%q", fk.seriesCalls, fk.lastSeriesFilter, fk.lastSeriesMetric)
	}
}

// TestFleetCompareCapErrorShownInline pins that FleetSeries' cap error
// renders inline (200, with the message in the body), never a 500.
func TestFleetCompareCapErrorShownInline(t *testing.T) {
	d, _ := fleetCompareMasterDeps(t, fleetFiveNodeRoster(), nil, errors.New("compare at most 10 nodes"))
	rr := fleetGetAsViewer(t, d, "/fleet/compare?tag=web&metric=cpu&range=6h")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error shown inline, not a 500)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "compare at most 10 nodes") {
		t.Errorf("GET /fleet/compare: cap error not rendered inline, body:\n%s", rr.Body.String())
	}
}

// TestFleetCompareUnknownNodeRejectedBeforeFleetSeriesCall pins that an unknown ?nodes= id
// is validated against the roster BEFORE FleetSeries is ever called.
func TestFleetCompareUnknownNodeRejectedBeforeFleetSeriesCall(t *testing.T) {
	d, fk := fleetCompareMasterDeps(t, fleetFiveNodeRoster(), []core.FleetSeriesPoint{{Node: "web1", TS: 1000, Value: 1}}, nil)
	rr := fleetGetAsViewer(t, d, "/fleet/compare?nodes=web1,ghost1,ghost2&metric=cpu&range=6h")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (validation error shown inline, not a 500)", rr.Code)
	}
	if fk.seriesCalls != 0 {
		t.Fatalf("FleetSeries calls = %d, want 0 -- unknown nodes must be rejected before ever calling FleetSeries", fk.seriesCalls)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "no such node: ghost1, ghost2") {
		t.Errorf("GET /fleet/compare: missing/wrong unknown-node error, body:\n%s", body)
	}
}

// TestFleetCompareUnknownNodeAcceptsDisplayName pins that validation, like
// core.NodeFilter.Nodes itself, accepts either a node's id or its exact display name.
func TestFleetCompareUnknownNodeAcceptsDisplayName(t *testing.T) {
	d, fk := fleetCompareMasterDeps(t, fleetFiveNodeRoster(), []core.FleetSeriesPoint{{Node: "self", TS: 1000, Value: 1}}, nil)
	rr := fleetGetAsViewer(t, d, "/fleet/compare?nodes=self&metric=cpu&range=6h")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if fk.seriesCalls != 1 {
		t.Fatalf("FleetSeries calls = %d, want 1 -- self's display name/id must validate", fk.seriesCalls)
	}
	if strings.Contains(rr.Body.String(), "no such node") {
		t.Errorf("GET /fleet/compare: self wrongly rejected as unknown, body:\n%s", rr.Body.String())
	}
}

// TestFleetCompareZeroPointsShowsEmptyMessage pins that a successful FleetSeries call
// returning no points shows a specific message, not a blank/empty panel.
func TestFleetCompareZeroPointsShowsEmptyMessage(t *testing.T) {
	d, _ := fleetCompareMasterDeps(t, fleetFiveNodeRoster(), nil, nil)
	rr := fleetGetAsViewer(t, d, "/fleet/compare?nodes=web1&metric=cpu&range=6h")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "no data for the selected nodes in this range") {
		t.Errorf("GET /fleet/compare: missing empty-result message, body:\n%s", rr.Body.String())
	}
}

// TestFleetCompareNoSelectionShowsPrompt pins that visiting the page with no ?nodes=/?tag=
// makes no FleetSeries call and shows a prompt instead of an error.
func TestFleetCompareNoSelectionShowsPrompt(t *testing.T) {
	d, fk := fleetCompareMasterDeps(t, fleetFiveNodeRoster(), nil, nil)
	rr := fleetGetAsViewer(t, d, "/fleet/compare")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if fk.seriesCalls != 0 {
		t.Fatalf("FleetSeries calls = %d, want 0 with no selection", fk.seriesCalls)
	}
	if !strings.Contains(rr.Body.String(), "Select nodes") {
		t.Errorf("GET /fleet/compare with no selection: missing prompt, body:\n%s", rr.Body.String())
	}
}
