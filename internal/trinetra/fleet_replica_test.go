package trinetra

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

const testNodeID = "0123456789abcdef0123456789abcdef"

func samplesRec(seq uint64, ts int64, m map[string]float64) fleet.Record {
	b, _ := json.Marshal(fleet.SamplesData{TS: ts, Metrics: m})
	return fleet.Record{Seq: seq, Kind: fleet.KindSamples, TS: ts, Data: b}
}

func alertRec(seq uint64, ts int64, key string) fleet.Record {
	b, _ := json.Marshal(AlertEvent{Time: ts, Key: key, Title: key + " high", Severity: "warning", Kind: "fire", Source: "anomaly"})
	return fleet.Record{Seq: seq, Kind: fleet.KindAlert, TS: ts, Data: b}
}

func eventRec(seq uint64, start int64) fleet.Record {
	b, _ := json.Marshal(fleet.DownEventData{Type: "power_down", Start: start, End: start + 60, DurationSec: 60})
	return fleet.Record{Seq: seq, Kind: fleet.KindDownEvent, TS: start, Data: b}
}

func baseRecs() []fleet.Record {
	return []fleet.Record{
		samplesRec(1, 100, map[string]float64{"cpu": 10}),
		samplesRec(2, 105, map[string]float64{"cpu": 20, "mem": 30}),
		eventRec(3, 50),
		alertRec(4, 110, "cpu"),
	}
}

func TestReplicaAppliesSamplesEventsAlerts(t *testing.T) {
	root := t.TempDir()
	r := newReplicaSink(root, StoreOptions{})
	if err := r.Apply(testNodeID, baseRecs()); err != nil {
		t.Fatal(err)
	}
	if seq, _ := r.AppliedSeq(testNodeID); seq != 4 {
		t.Fatalf("applied seq = %d", seq)
	}
	n, _ := r.node(testNodeID)
	pts, _ := n.store.Query("cpu", 0, 1000, ResRaw)
	if len(pts) != 2 || pts[1].Avg != 20 {
		t.Fatalf("cpu points = %+v", pts)
	}
	evs, _ := n.store.Events(0, 1000)
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	b, _ := os.ReadFile(filepath.Join(root, testNodeID, "alertlog.jsonl"))
	if strings.Count(string(b), "\n") != 1 {
		t.Fatalf("alertlog = %q", b)
	}
}

func TestReplicaIgnoresReplayedAndOlderPoints(t *testing.T) {
	root := t.TempDir()
	r := newReplicaSink(root, StoreOptions{})
	if err := r.Apply(testNodeID, baseRecs()); err != nil {
		t.Fatal(err)
	}
	// A crash between write and ack makes the child resend everything.
	if err := r.Backfill(testNodeID, baseRecs()); err != nil {
		t.Fatal(err)
	}
	if err := r.Backfill(testNodeID, []fleet.Record{samplesRec(0, 90, map[string]float64{"cpu": 99})}); err != nil {
		t.Fatal(err)
	}
	// Reopen from disk: the guard must survive a master restart.
	r2 := newReplicaSink(root, StoreOptions{})
	if err := r2.Backfill(testNodeID, baseRecs()); err != nil {
		t.Fatal(err)
	}
	n, _ := r2.node(testNodeID)
	pts, _ := n.store.Query("cpu", 0, 1000, ResRaw)
	evs, _ := n.store.Events(0, 1000)
	b, _ := os.ReadFile(filepath.Join(root, testNodeID, "alertlog.jsonl"))
	if len(pts) != 2 || len(evs) != 1 || strings.Count(string(b), "\n") != 1 {
		t.Fatalf("dups after replay: pts=%d evs=%d alerts=%d", len(pts), len(evs), strings.Count(string(b), "\n"))
	}
}

func TestReplicaRollupBackfill(t *testing.T) {
	r := newReplicaSink(t.TempDir(), StoreOptions{})
	b, _ := json.Marshal(fleet.SamplesData{TS: 60, Res: "1m", Rollups: map[string]fleet.RollupPoint{"cpu": {Min: 1, Avg: 2, Max: 3}}})
	if err := r.Backfill(testNodeID, []fleet.Record{{Kind: fleet.KindSamples, TS: 60, Data: b}}); err != nil {
		t.Fatal(err)
	}
	n, _ := r.node(testNodeID)
	pts, _ := n.store.Query("cpu", 0, 1000, Res1m)
	if len(pts) != 1 || pts[0].Min != 1 || pts[0].Max != 3 {
		t.Fatalf("1m points = %+v", pts)
	}
}

func TestReplicaRejectsBadNodeID(t *testing.T) {
	r := newReplicaSink(t.TempDir(), StoreOptions{})
	for _, id := range []string{"../etc", "", "ABCDEF0123456789ABCDEF0123456789", "short"} {
		if _, err := r.AppliedSeq(id); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
}

func TestReplicaNodeAPIServesReplica(t *testing.T) {
	r := newReplicaSink(t.TempDir(), StoreOptions{})
	if err := r.Apply(testNodeID, baseRecs()); err != nil {
		t.Fatal(err)
	}
	snap, _ := json.Marshal(Snapshot{TS: 105, CPU: 42, MemPct: 30, Online: true})
	host, _ := json.Marshal(HostInfo{Hostname: "db-1"})
	if err := r.Live(testNodeID, fleet.LiveUpdate{Version: "v9", Snapshot: snap, HostInfo: host}); err != nil {
		t.Fatal(err)
	}
	api, err := r.NodeAPI(testNodeID, config.Default)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := api.Snapshot()
	if v.CPU != 42 {
		t.Fatalf("snapshot cpu = %v", v.CPU)
	}
	pts, _ := api.Series("cpu", 0, 1000, core.ResRaw)
	if len(pts) != 2 {
		t.Fatalf("series = %+v", pts)
	}
	hist, _ := api.AlertHistory(0, 10)
	if len(hist) != 1 {
		t.Fatalf("alert history = %+v", hist)
	}
	hi, _ := api.HostInfo()
	if hi.Hostname != "db-1" {
		t.Fatalf("hostinfo = %+v", hi)
	}
	if ver, _ := api.Version(); ver != "v9" {
		t.Fatalf("version = %q", ver)
	}
	if _, err := api.ContainerLogs("x", 10); err == nil {
		t.Fatal("container logs on remote node should error in phase 1")
	}
}

func TestTSFileMetricsDecodeEncodedIDs(t *testing.T) {
	s, err := newTSFileStore(t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(1, MetricSet{"disk:/mnt/my disk": 1, "cpu": 2}); err != nil {
		t.Fatal(err)
	}
	ms, err := s.Metrics(ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ms, "|")
	if got != "cpu|disk:/mnt/my disk" {
		t.Fatalf("metrics = %q", got)
	}
}

// TestReplicaApplyEvictsNodeOnWriteFailure proves a write failure partway
// through a batch (record 2 of 4's raw-sample Append fails; the event and
// alert records after it are never even attempted) does not poison the
// in-memory ordering guard for the child's retry: after the failure is
// cleared, resubmitting the EXACT SAME batch must apply every record
// exactly once (no loss of what only the retry can write, no duplication of
// what the first, partial attempt already got onto disk), and AppliedSeq
// must advance only once the retry actually succeeds.
func TestReplicaApplyEvictsNodeOnWriteFailure(t *testing.T) {
	root := t.TempDir()
	r := newReplicaSink(root, StoreOptions{})

	calls := 0
	replicaWriteFailHook = func(op string) error {
		if op == "append" {
			calls++
			if calls == 2 {
				return errors.New("injected append failure")
			}
		}
		return nil
	}
	defer func() { replicaWriteFailHook = nil }()

	recs := baseRecs() // samples(100,cpu), samples(105,cpu+mem), event(50), alert(110)
	if err := r.Apply(testNodeID, recs); err == nil {
		t.Fatal("expected the injected failure to surface")
	}
	if seq, _ := r.AppliedSeq(testNodeID); seq != 0 {
		t.Fatalf("applied seq after failed apply = %d, want 0 (nothing acked)", seq)
	}

	// Clear the injected failure and resubmit the identical batch, exactly
	// as a real child would after an ingest call errors.
	replicaWriteFailHook = nil
	if err := r.Apply(testNodeID, recs); err != nil {
		t.Fatalf("retry after clearing the failure: %v", err)
	}
	if seq, _ := r.AppliedSeq(testNodeID); seq != 4 {
		t.Fatalf("applied seq after successful retry = %d, want 4", seq)
	}

	n, _ := r.node(testNodeID)
	cpuPts, _ := n.store.Query("cpu", 0, 1000, ResRaw)
	if len(cpuPts) != 2 {
		t.Fatalf("cpu points = %+v, want exactly 2 (first attempt's point kept, no dup)", cpuPts)
	}
	memPts, _ := n.store.Query("mem", 0, 1000, ResRaw)
	if len(memPts) != 1 {
		t.Fatalf("mem points = %+v, want exactly 1 (only the retry could ever write it)", memPts)
	}
	evs, _ := n.store.Events(0, 1000)
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want exactly 1", evs)
	}
	b, _ := os.ReadFile(filepath.Join(root, testNodeID, "alertlog.jsonl"))
	if strings.Count(string(b), "\n") != 1 {
		t.Fatalf("alertlog = %q, want exactly 1 line", b)
	}
}

// TestReplicaAlertLogFailureThenRetryWritesOnce covers the alert-log write
// specifically (a separate append-only file from the tsfile series): a
// failed alert-log append must not leave behind a poisoned dedup guard that
// makes the identical retry silently drop the alert as "already seen".
func TestReplicaAlertLogFailureThenRetryWritesOnce(t *testing.T) {
	root := t.TempDir()
	r := newReplicaSink(root, StoreOptions{})

	replicaWriteFailHook = func(op string) error {
		if op == "alertlog" {
			return errors.New("injected alertlog failure")
		}
		return nil
	}
	recs := []fleet.Record{alertRec(1, 110, "cpu")}
	if err := r.Apply(testNodeID, recs); err == nil {
		t.Fatal("expected the injected failure to surface")
	}
	if seq, _ := r.AppliedSeq(testNodeID); seq != 0 {
		t.Fatalf("applied seq after failed apply = %d, want 0", seq)
	}

	replicaWriteFailHook = nil
	if err := r.Apply(testNodeID, recs); err != nil {
		t.Fatalf("retry after clearing the failure: %v", err)
	}
	if seq, _ := r.AppliedSeq(testNodeID); seq != 1 {
		t.Fatalf("applied seq after successful retry = %d, want 1", seq)
	}
	b, err := os.ReadFile(filepath.Join(root, testNodeID, "alertlog.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "\n") != 1 {
		t.Fatalf("alertlog = %q, want exactly 1 line (not zero, not duplicated)", b)
	}
}

// Replica maintenance (Downsample+Prune, fsync-heavy) runs on the same
// cadence as the local store's (storeMaintenanceInterval), staggered so each
// 5s master tick handles only a slice of the nodes.
func TestReplicaMaintenanceSliceSize(t *testing.T) {
	cases := []struct {
		total          int
		tick, interval time.Duration
		want           int
	}{
		{0, 5 * time.Second, 15 * time.Minute, 0},
		{1, 5 * time.Second, 15 * time.Minute, 1},
		{180, 5 * time.Second, 15 * time.Minute, 1},
		{181, 5 * time.Second, 15 * time.Minute, 2},
		{1000, 5 * time.Second, 15 * time.Minute, 6},
		{5, 5 * time.Second, 10 * time.Second, 3},
		{5, time.Minute, time.Second, 5}, // interval shorter than a tick: everything each tick
	}
	for _, c := range cases {
		if got := maintSliceSize(c.total, c.tick, c.interval); got != c.want {
			t.Errorf("maintSliceSize(%d, %v, %v) = %d, want %d", c.total, c.tick, c.interval, got, c.want)
		}
	}
}

func TestReplicaMaintenanceVisitsEveryNodeOncePerInterval(t *testing.T) {
	var s maintScheduler
	ids := []string{"a", "b", "c", "d", "e"}
	seen := map[string]int{}
	for i := 0; i < 2; i++ { // 2 ticks per interval
		for _, id := range s.next(ids, 5*time.Second, 10*time.Second) {
			seen[id]++
		}
	}
	for _, id := range ids {
		if seen[id] == 0 {
			t.Fatalf("node %s not maintained within one interval: %v", id, seen)
		}
	}
	if len(s.next(nil, 5*time.Second, 10*time.Second)) != 0 {
		t.Fatal("no nodes should mean an empty slice")
	}
}

// Live updates arrive every few seconds per node: they must not rewrite the
// snapshot and alert state files each time. The node API still serves the
// latest snapshot (from memory / live.json).
func TestReplicaLiveWritesOnlyWhatChanged(t *testing.T) {
	root := t.TempDir()
	r := newReplicaSink(root, StoreOptions{})
	snap, _ := json.Marshal(Snapshot{TS: 1, CPU: 7})
	as := json.RawMessage(`{"active":{}}`)
	if err := r.Live(testNodeID, fleet.LiveUpdate{Snapshot: snap, AlertState: as}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, testNodeID, "snapshot.json")); !os.IsNotExist(err) {
		t.Fatalf("snapshot.json written on live update (err=%v)", err)
	}
	alertsPath := filepath.Join(root, testNodeID, "alerts.json")
	old := time.Unix(1000, 0)
	if err := os.Chtimes(alertsPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := r.Live(testNodeID, fleet.LiveUpdate{Snapshot: snap, AlertState: as}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(alertsPath); !fi.ModTime().Equal(old) {
		t.Fatal("unchanged alert state rewritten")
	}
	api, _ := r.NodeAPI(testNodeID, config.Default)
	if v, _ := api.Snapshot(); v.CPU != 7 {
		t.Fatalf("snapshot cpu = %v", v.CPU)
	}
	// And after a master restart (fresh sink over the same dir).
	api2, _ := newReplicaSink(root, StoreOptions{}).NodeAPI(testNodeID, config.Default)
	if v, _ := api2.Snapshot(); v.CPU != 7 {
		t.Fatalf("snapshot cpu after restart = %v", v.CPU)
	}
}

// Clock skew (server_time - sent_at) is estimated per node from the sample
// closest to zero over a short window, persisted in ingest.state, and only
// reported over the 30s line after three consecutive over-line estimates.
func TestReplicaSkewFilteredAndPersisted(t *testing.T) {
	root := t.TempDir()
	r := newReplicaSink(root, StoreOptions{})
	for i := 1; i <= 2; i++ {
		if s, crossed := r.RecordSkew(testNodeID, 100); s != 0 || crossed {
			t.Fatalf("sample %d: skew %d crossed %v, want 0 false (not yet confirmed)", i, s, crossed)
		}
	}
	if s, crossed := r.RecordSkew(testNodeID, 100); s != 100 || !crossed {
		t.Fatalf("third sample: skew %d crossed %v, want 100 true", s, crossed)
	}
	if s, crossed := r.RecordSkew(testNodeID, 100); s != 100 || crossed {
		t.Fatalf("fourth sample: skew %d crossed %v, want 100 false (warned once)", s, crossed)
	}
	// Clock fixed: one good sample is enough (closest to zero wins).
	if s, _ := r.RecordSkew(testNodeID, 1); s != 1 {
		t.Fatalf("after fix: skew %d, want 1", s)
	}
	if err := r.Apply(testNodeID, baseRecs()); err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(testNodeID, []fleet.Record{samplesRec(5, 100, map[string]float64{"cpu": 1})}); err != nil {
		t.Fatal(err)
	}
	want := r.Stats(testNodeID)
	if want.SkewSec != 1 || want.DroppedOutOfOrder != 1 {
		t.Fatalf("stats = %+v, want skew 1 and one out-of-order drop", want)
	}
	got := newReplicaSink(root, StoreOptions{}).Stats(testNodeID)
	if got.SkewSec != want.SkewSec || got.DroppedOutOfOrder != 1 {
		t.Fatalf("after restart stats = %+v, want %+v", got, want)
	}
}

// Network delay only ever inflates server_time - sent_at: a burst of
// requests held in a partition and then delivered (sent_at 60s old) must
// not read as a clock 60s behind.
func TestReplicaSkewIgnoresDelayedBurst(t *testing.T) {
	r := newReplicaSink(t.TempDir(), StoreOptions{})
	for i := 0; i < 6; i++ {
		r.RecordSkew(testNodeID, int64(i%2))
	}
	for i := 0; i < 6; i++ {
		if s, crossed := r.RecordSkew(testNodeID, 60); s > 30 || crossed {
			t.Fatalf("delayed sample %d: skew %d crossed %v", i, s, crossed)
		}
	}
}

// A genuinely skewed clock (every sample 45s off, either way) is reported
// and warned about once it has held for three samples.
func TestReplicaSkewGenuineBothDirections(t *testing.T) {
	for _, off := range []int64{45, -45} {
		r := newReplicaSink(t.TempDir(), StoreOptions{})
		warned := 0
		var s int64
		for i := 0; i < 10; i++ {
			var crossed bool
			s, crossed = r.RecordSkew(testNodeID, off)
			if crossed {
				warned++
				if i != 2 {
					t.Fatalf("offset %d: warned at sample %d, want the third", off, i+1)
				}
			}
		}
		if s != off || warned != 1 {
			t.Fatalf("offset %d: skew %d warned %d, want %d once", off, s, warned, off)
		}
	}
}

// A failed apply re-seeds the ordering guards on the SAME replicaNode and
// store rather than dropping it from the cache: replacing it would put a
// second tsFileStore on the same directory while maintenance may still hold
// the first.
func TestReplicaFailedApplyReseedsSameNode(t *testing.T) {
	r := newReplicaSink(t.TempDir(), StoreOptions{})
	before, err := r.node(testNodeID)
	if err != nil {
		t.Fatal(err)
	}
	replicaWriteFailHook = func(op string) error {
		if op == "append" {
			return errors.New("injected append failure")
		}
		return nil
	}
	err = r.Apply(testNodeID, baseRecs())
	replicaWriteFailHook = nil
	if err == nil {
		t.Fatal("expected the injected failure")
	}
	after, _ := r.node(testNodeID)
	if after != before || after.store != before.store {
		t.Fatal("failed apply replaced the cached replica node/store")
	}
	if err := r.Apply(testNodeID, baseRecs()); err != nil {
		t.Fatal(err)
	}
	if pts, _ := after.store.Query("cpu", 0, 1000, ResRaw); len(pts) != 2 {
		t.Fatalf("cpu points after retry = %+v", pts)
	}
}

// Several alerts can share the newest timestamp. After a master restart the
// dedupe set must hold all of them, not just the last line, or a re-sent
// batch (e.g. gap repair) duplicates the others.
func TestReplicaAlertDedupeSeedsAllLinesAtLastTimestamp(t *testing.T) {
	root := t.TempDir()
	recs := []fleet.Record{alertRec(1, 110, "cpu"), alertRec(2, 110, "mem"), alertRec(3, 110, "disk")}
	if err := newReplicaSink(root, StoreOptions{}).Apply(testNodeID, recs); err != nil {
		t.Fatal(err)
	}
	r2 := newReplicaSink(root, StoreOptions{}) // master restart
	if err := r2.Backfill(testNodeID, []fleet.Record{alertRec(0, 110, "cpu"), alertRec(0, 110, "mem"), alertRec(0, 110, "disk")}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, testNodeID, "alertlog.jsonl"))
	if n := strings.Count(string(b), "\n"); n != 3 {
		t.Fatalf("alertlog has %d lines, want 3 (no duplicates):\n%s", n, b)
	}
}

// Re-sent copies of what the replica already holds (a refill, a retried
// batch) count as duplicates; only points older than the series' last one
// count as out of order. Both persist across a restart.
func TestReplicaSplitsDuplicateFromOutOfOrder(t *testing.T) {
	root := t.TempDir()
	r := newReplicaSink(root, StoreOptions{})
	if err := r.Apply(testNodeID, baseRecs()); err != nil {
		t.Fatal(err)
	}
	if err := r.Backfill(testNodeID, []fleet.Record{
		samplesRec(0, 105, map[string]float64{"cpu": 20, "mem": 30}), // 2 duplicates
		samplesRec(0, 90, map[string]float64{"cpu": 1}),              // out of order
		eventRec(0, 50),         // duplicate
		eventRec(0, 40),         // out of order
		alertRec(0, 110, "cpu"), // duplicate
	}); err != nil {
		t.Fatal(err)
	}
	st := r.Stats(testNodeID)
	if st.DroppedDuplicate != 4 || st.DroppedOutOfOrder != 2 {
		t.Fatalf("stats = %+v, want 4 duplicates and 2 out of order", st)
	}
	got := newReplicaSink(root, StoreOptions{}).Stats(testNodeID)
	if got.DroppedDuplicate != 4 || got.DroppedOutOfOrder != 2 {
		t.Fatalf("after restart stats = %+v", got)
	}
}

// An ingest.state written before the split (one dropped_out_of_order counter
// that also held duplicates) loads as out of order.
func TestReplicaLoadsOldDropCounter(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, testNodeID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ingest.state"), []byte(`{"applied_seq":4,"last_ingest_ts":105,"dropped_out_of_order":7,"dropped_cardinality":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st := newReplicaSink(root, StoreOptions{}).Stats(testNodeID)
	if st.AppliedSeq != 4 || st.DroppedOutOfOrder != 7 || st.DroppedCardinality != 1 || st.DroppedDuplicate != 0 {
		t.Fatalf("stats = %+v", st)
	}
	// The old counter (which also held duplicates) is taken as already
	// warned about, so upgrading does not raise a warning for it.
	if st.WarnedOutOfOrder != 7 || st.WarnedCardinality != 1 {
		t.Fatalf("drop-warning baseline = %d/%d, want 7/1", st.WarnedOutOfOrder, st.WarnedCardinality)
	}
}

// A failed alerts.json write is retried by the next live update even when
// the alert state has not changed since: the comparison is against what was
// last written successfully, not what was last received.
func TestReplicaLiveRetriesFailedAlertsWrite(t *testing.T) {
	root := t.TempDir()
	r := newReplicaSink(root, StoreOptions{})
	replicaWriteFailHook = func(op string) error {
		if op == "alerts" {
			return errors.New("injected alerts.json failure")
		}
		return nil
	}
	t.Cleanup(func() { replicaWriteFailHook = nil })
	as := json.RawMessage(`{"active":{"cpu":{}}}`)
	if err := r.Live(testNodeID, fleet.LiveUpdate{AlertState: as}); err == nil {
		t.Fatal("failed alerts.json write not reported")
	}
	replicaWriteFailHook = nil
	if err := r.Live(testNodeID, fleet.LiveUpdate{AlertState: as}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, testNodeID, "alerts.json"))
	if err != nil || !bytes.Equal(b, as) {
		t.Fatalf("alerts.json = %q, %v; want %q", b, err, as)
	}
}
