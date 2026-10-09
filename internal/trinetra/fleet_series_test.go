package trinetra

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// fleetSeriesFixture wires a masterState with a real registry/sink, plus a
// self store/name via engine.SetRules (fleet_rules.go's ruleSelfSource,
// exactly the plumbing the aggregate-rule engine already uses -- see
// fleet_provider.go's FleetSeries doc), so FleetSeries's self-inclusion path
// is exercised the same way it runs in production.
type fleetSeriesFixture struct {
	m         *masterState
	api       fleetAPIImpl
	selfStore *tsFileStore
}

func newFleetSeriesFixture(t *testing.T) *fleetSeriesFixture {
	t.Helper()
	m := newTestMasterState(t)
	selfStore, err := newTSFileStore(filepath.Join(t.TempDir(), "self"), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m.engine.SetRules(m.reg, m.sink, m.tracker, time.Now(), ruleSelfSource{
		Name:  func() string { return "master1" },
		Snap:  func() (Snapshot, bool) { return Snapshot{}, false },
		Store: selfStore,
	})
	p := &fleetProvider{role: "master", master: m, selfName: func() string { return "master1" }}
	return &fleetSeriesFixture{m: m, api: fleetAPIImpl{p}, selfStore: selfStore}
}

// addNode registers a fleet node and returns it.
func (f *fleetSeriesFixture) addNode(t *testing.T, name string, tags []string) fleet.Node {
	t.Helper()
	id, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	n := fleet.Node{ID: id, Name: name, Tags: tags, Joined: time.Now().Unix()}
	if err := f.m.reg.Add(n); err != nil {
		t.Fatal(err)
	}
	got, _ := f.m.reg.Get(id)
	return got
}

// appendPoint writes one 1m-resolution rollup point directly to id's
// replicated store (the same call fleet_replica.go's gap filler and
// fleet_rules_test.go's ruleFixture.appendSeriesPoint use).
func (f *fleetSeriesFixture) appendPoint(t *testing.T, id, metric string, ts int64, v float64) {
	t.Helper()
	n, err := f.m.sink.node(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.AppendRollup(metric, Point{TS: ts, Min: v, Avg: v, Max: v}); err != nil {
		t.Fatal(err)
	}
}

// appendRawPoint writes one raw (unrolled-up) sample directly to id's
// replicated store, the same call the real ingest path (fleet_replica.go)
// uses for a fast-tier metric.
func (f *fleetSeriesFixture) appendRawPoint(t *testing.T, id, metric string, ts int64, v float64) {
	t.Helper()
	n, err := f.m.sink.node(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.Append(ts, MetricSet{metric: v}); err != nil {
		t.Fatal(err)
	}
}

func fleetSeriesValues(pts []core.FleetSeriesPoint, node string) map[int64]float64 {
	out := map[int64]float64{}
	for _, p := range pts {
		if p.Node == node {
			out[p.TS] = p.Value
		}
	}
	return out
}

// TestFleetSeriesNoneReturnsOneSeriesPerNode pins agg="none"'s per-node
// shape: both fake replica nodes' own points come back,
// each tagged with that node's display name.
func TestFleetSeriesNoneReturnsOneSeriesPerNode(t *testing.T) {
	f := newFleetSeriesFixture(t)
	web1 := f.addNode(t, "web1", []string{"web"})
	web2 := f.addNode(t, "web2", []string{"web"})
	f.appendPoint(t, web1.ID, "cpu", 1000, 10)
	f.appendPoint(t, web1.ID, "cpu", 1060, 20)
	f.appendPoint(t, web2.ID, "cpu", 1000, 30)
	f.appendPoint(t, web2.ID, "cpu", 1060, 40)

	pts, err := f.api.FleetSeries("cpu", core.NodeFilter{Tag: "web"}, core.AggNone, 900, 1200, core.Res1m)
	if err != nil {
		t.Fatalf("FleetSeries: %v", err)
	}
	if len(pts) != 4 {
		t.Fatalf("len(pts) = %d, want 4: %+v", len(pts), pts)
	}
	w1 := fleetSeriesValues(pts, "web1")
	w2 := fleetSeriesValues(pts, "web2")
	if w1[1000] != 10 || w1[1060] != 20 {
		t.Fatalf("web1 series = %+v", w1)
	}
	if w2[1000] != 30 || w2[1060] != 40 {
		t.Fatalf("web2 series = %+v", w2)
	}
}

// TestFleetSeriesAggAvgAndMax pins avg/max's per-bucket aggregation across
// every matching node (task-1b-brief.md: "avg/max/min return one aggregated
// series ... computed per timestamp bucket").
func TestFleetSeriesAggAvgAndMax(t *testing.T) {
	f := newFleetSeriesFixture(t)
	web1 := f.addNode(t, "web1", []string{"web"})
	web2 := f.addNode(t, "web2", []string{"web"})
	// 960/1020 are 60s-bucket-aligned (real 1m rollups always are); using
	// aligned timestamps here keeps this test about avg/max/min itself, not
	// about the bucketing floor (see TestFleetSeriesAggBucketsMisalignedRawTimestamps
	// for that).
	f.appendPoint(t, web1.ID, "cpu", 960, 10)
	f.appendPoint(t, web1.ID, "cpu", 1020, 20)
	f.appendPoint(t, web2.ID, "cpu", 960, 30)
	f.appendPoint(t, web2.ID, "cpu", 1020, 40)

	avg, err := f.api.FleetSeries("cpu", core.NodeFilter{Tag: "web"}, core.AggAvg, 900, 1200, core.Res1m)
	if err != nil {
		t.Fatalf("FleetSeries avg: %v", err)
	}
	if len(avg) != 2 || avg[0].Node != "" || avg[0].TS != 960 || avg[0].Value != 20 || avg[1].Value != 30 {
		t.Fatalf("avg series = %+v", avg)
	}

	max, err := f.api.FleetSeries("cpu", core.NodeFilter{Tag: "web"}, core.AggMax, 900, 1200, core.Res1m)
	if err != nil {
		t.Fatalf("FleetSeries max: %v", err)
	}
	if len(max) != 2 || max[0].Value != 30 || max[1].Value != 40 {
		t.Fatalf("max series = %+v", max)
	}

	min, err := f.api.FleetSeries("cpu", core.NodeFilter{Tag: "web"}, core.AggMin, 900, 1200, core.Res1m)
	if err != nil {
		t.Fatalf("FleetSeries min: %v", err)
	}
	if len(min) != 2 || min[0].Value != 10 || min[1].Value != 20 {
		t.Fatalf("min series = %+v", min)
	}
}

// TestFleetSeriesTagFilterExcludesNonMatching pins that FleetSeries's
// NodeFilter narrows the node set exactly like Nodes() does.
func TestFleetSeriesTagFilterExcludesNonMatching(t *testing.T) {
	f := newFleetSeriesFixture(t)
	web1 := f.addNode(t, "web1", []string{"web"})
	db1 := f.addNode(t, "db1", []string{"db"})
	f.appendPoint(t, web1.ID, "cpu", 1000, 10)
	f.appendPoint(t, db1.ID, "cpu", 1000, 99)

	pts, err := f.api.FleetSeries("cpu", core.NodeFilter{Tag: "web"}, core.AggNone, 900, 1200, core.Res1m)
	if err != nil {
		t.Fatalf("FleetSeries: %v", err)
	}
	if len(pts) != 1 || pts[0].Node != "web1" || pts[0].Value != 10 {
		t.Fatalf("tag-filtered series = %+v, want just web1's point", pts)
	}
}

// TestFleetSeriesNoneCapError pins agg="none"'s 10-node cap
// (task-1b-brief.md: "capped at 10 nodes. Above the cap the error is
// 'compare at most 10 nodes'").
func TestFleetSeriesNoneCapError(t *testing.T) {
	f := newFleetSeriesFixture(t)
	for i := 0; i < 11; i++ {
		f.addNode(t, "farm"+string(rune('a'+i)), []string{"farm"})
	}
	_, err := f.api.FleetSeries("cpu", core.NodeFilter{Tag: "farm"}, core.AggNone, 900, 1200, core.Res1m)
	if err == nil || err.Error() != "compare at most 10 nodes" {
		t.Fatalf("FleetSeries over the cap: err = %v, want %q", err, "compare at most 10 nodes")
	}
}

// TestFleetSeriesIncludesSelf pins that FleetSeries includes the master's
// own node via its local store, exactly like the aggregate-rule engine's
// ruleSelfSource (task-1b-brief.md: "Includes the master's own node (self)
// the same way B7's rules do, via its local store").
func TestFleetSeriesIncludesSelf(t *testing.T) {
	f := newFleetSeriesFixture(t)
	if err := f.selfStore.AppendRollup("cpu", Point{TS: 1000, Min: 55, Avg: 55, Max: 55}); err != nil {
		t.Fatal(err)
	}

	pts, err := f.api.FleetSeries("cpu", core.NodeFilter{}, core.AggNone, 900, 1200, core.Res1m)
	if err != nil {
		t.Fatalf("FleetSeries: %v", err)
	}
	self := fleetSeriesValues(pts, "master1")
	if self[1000] != 55 {
		t.Fatalf("self series = %+v, want {1000:55}", self)
	}
}

// TestFleetSeriesRequiresMaster pins that FleetSeries, like every other
// FleetAPI method, is master-only.
func TestFleetSeriesRequiresMaster(t *testing.T) {
	p := &fleetProvider{role: "solo"}
	api := fleetAPIImpl{p}
	if _, err := api.FleetSeries("cpu", core.NodeFilter{}, core.AggNone, 0, 100, core.Res1m); err != core.ErrNotMaster {
		t.Fatalf("FleetSeries on a non-master = %v, want core.ErrNotMaster", err)
	}
}

// TestFleetSeriesAggBucketsMisalignedRawTimestamps pins task-1b review round
// 1's bucketing ruling at raw resolution: two nodes whose raw samples are
// NOT taken at the same instants (a realistic scenario -- nothing
// synchronizes two independent daemons' sample clocks) still land in the
// same shared bucket and aggregate together, each node contributing only
// its LAST value within that bucket. newTestMasterState's getCfg is
// config.Default (FastInterval 5), so the raw bucket width here is 5s.
func TestFleetSeriesAggBucketsMisalignedRawTimestamps(t *testing.T) {
	f := newFleetSeriesFixture(t)
	web1 := f.addNode(t, "web1", []string{"web"})
	web2 := f.addNode(t, "web2", []string{"web"})

	// Bucket [1000,1005): web1 has two samples in it (1000, 1002) -- only
	// the later one (1002 -> 14) should count; web2 has one (1001 -> 20).
	f.appendRawPoint(t, web1.ID, "cpu", 1000, 10)
	f.appendRawPoint(t, web1.ID, "cpu", 1002, 14)
	f.appendRawPoint(t, web2.ID, "cpu", 1001, 20)
	// Bucket [1005,1010): web1 at 1008 -> 30, web2 at 1009 -> 40.
	f.appendRawPoint(t, web1.ID, "cpu", 1008, 30)
	f.appendRawPoint(t, web2.ID, "cpu", 1009, 40)

	avg, err := f.api.FleetSeries("cpu", core.NodeFilter{Tag: "web"}, core.AggAvg, 990, 1020, core.ResRaw)
	if err != nil {
		t.Fatalf("FleetSeries avg: %v", err)
	}
	if len(avg) != 2 || avg[0].TS != 1000 || avg[0].Value != 17 || avg[1].TS != 1005 || avg[1].Value != 35 {
		t.Fatalf("avg series = %+v, want [{TS:1000 Value:17} {TS:1005 Value:35}]", avg)
	}

	max, err := f.api.FleetSeries("cpu", core.NodeFilter{Tag: "web"}, core.AggMax, 990, 1020, core.ResRaw)
	if err != nil {
		t.Fatalf("FleetSeries max: %v", err)
	}
	if len(max) != 2 || max[0].Value != 20 || max[1].Value != 40 {
		t.Fatalf("max series = %+v, want values [20 40]", max)
	}

	min, err := f.api.FleetSeries("cpu", core.NodeFilter{Tag: "web"}, core.AggMin, 990, 1020, core.ResRaw)
	if err != nil {
		t.Fatalf("FleetSeries min: %v", err)
	}
	if len(min) != 2 || min[0].Value != 14 || min[1].Value != 30 {
		t.Fatalf("min series = %+v, want values [14 30]", min)
	}
}

// TestFleetSeriesDiskMetricUsesWorstMountPerBucket pins that FleetSeries'
// "disk" metric reports, at each bucket, the WORST of a node's several
// mounts -- not just one mount's own series -- exactly like
// NodeSummary.WorstDiskPct/the aggregate rules' own "disk (worst)" framing.
func TestFleetSeriesDiskMetricUsesWorstMountPerBucket(t *testing.T) {
	f := newFleetSeriesFixture(t)
	web1 := f.addNode(t, "web1", []string{"web"})
	// At ts=960, "/data" (70) is worse than "/" (40); at ts=1020, "/" (55)
	// is worse than "/data" (50) -- the worst mount flips between buckets,
	// so a correct implementation can't just pick one mount's series.
	f.appendPoint(t, web1.ID, "disk:/", 960, 40)
	f.appendPoint(t, web1.ID, "disk:/data", 960, 70)
	f.appendPoint(t, web1.ID, "disk:/", 1020, 55)
	f.appendPoint(t, web1.ID, "disk:/data", 1020, 50)

	pts, err := f.api.FleetSeries("disk", core.NodeFilter{Nodes: []string{web1.ID}}, core.AggNone, 900, 1200, core.Res1m)
	if err != nil {
		t.Fatalf("FleetSeries: %v", err)
	}
	got := fleetSeriesValues(pts, "web1")
	if got[960] != 70 || got[1020] != 55 {
		t.Fatalf("disk series = %+v, want {960:70, 1020:55}", got)
	}
}

// TestFleetSeriesRejectsUnknownAgg pins the minor validation ruling: an agg
// value other than none/avg/max/min is rejected, naming it verbatim.
func TestFleetSeriesRejectsUnknownAgg(t *testing.T) {
	f := newFleetSeriesFixture(t)
	_, err := f.api.FleetSeries("cpu", core.NodeFilter{}, core.Agg("bogus"), 900, 1200, core.Res1m)
	if err == nil || err.Error() != "unknown aggregation" {
		t.Fatalf("FleetSeries with agg=bogus: err = %v, want %q", err, "unknown aggregation")
	}
}

// TestFleetSeriesRejectsToNotAfterFrom pins the minor validation ruling:
// to<=from is rejected rather than silently returning nothing/garbage.
func TestFleetSeriesRejectsToNotAfterFrom(t *testing.T) {
	f := newFleetSeriesFixture(t)
	if _, err := f.api.FleetSeries("cpu", core.NodeFilter{}, core.AggNone, 1000, 1000, core.Res1m); err == nil {
		t.Fatal("FleetSeries with to==from: want an error")
	}
	if _, err := f.api.FleetSeries("cpu", core.NodeFilter{}, core.AggNone, 1000, 500, core.Res1m); err == nil {
		t.Fatal("FleetSeries with to<from: want an error")
	}
}

// TestFleetSeriesRejectsRawWindowOverCap pins the minor validation ruling:
// a raw-resolution window wider than 24h is rejected.
func TestFleetSeriesRejectsRawWindowOverCap(t *testing.T) {
	f := newFleetSeriesFixture(t)
	_, err := f.api.FleetSeries("cpu", core.NodeFilter{}, core.AggNone, 0, 25*3600, core.ResRaw)
	if err == nil || err.Error() != "raw resolution is limited to a 24h window" {
		t.Fatalf("FleetSeries over the raw window cap: err = %v, want %q", err, "raw resolution is limited to a 24h window")
	}
	// Exactly at the cap is still allowed.
	if _, err := f.api.FleetSeries("cpu", core.NodeFilter{}, core.AggNone, 0, 24*3600, core.ResRaw); err != nil {
		t.Fatalf("FleetSeries at exactly the raw window cap: %v, want no error", err)
	}
}
