package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fleetCompareMasterDeps builds a master Deps whose Fleet().FleetSeries
// returns pts (or seriesErr, if set) -- fleetMasterDeps's own fakeFleet
// doesn't let a caller control FleetSeries, so this builds its own.
func fleetCompareMasterDeps(t *testing.T, nodes []core.NodeSummary, pts []core.FleetSeriesPoint, seriesErr error) (Deps, *fakeFleet) {
	t.Helper()
	fk := &fakeFleet{status: core.FleetStatus{Role: config.RoleMaster}, nodes: nodes, series: pts, seriesErr: seriesErr}
	return fleetTestDeps(t, masterFakeAPI(fk, nil)), fk
}

// TestFleetCompareRendersSingleFleetSeriesCall pins that a compare request
// makes exactly one FleetSeries call (task-1b-brief.md: "Renders one uPlot
// overlay from a single FleetSeries call"), with the chart's data embedded
// in the page (no separate fetch needed).
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

// TestFleetCompareNoSelectionShowsPrompt pins that visiting the page with no
// ?nodes=/?tag= makes no FleetSeries call and shows a prompt instead of an
// error.
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
