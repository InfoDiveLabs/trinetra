package serverwatch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
	"serverwatch/internal/fleet"
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
