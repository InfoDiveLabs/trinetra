// Package serverwatch: fleet_replica.go is the master's side of fleet
// ingest: a fleet.Sink that writes each child's records into its own tsfile
// store under <stateDir>/fleet/nodes/<id>/, plus a core.API view over that
// replica so every existing per-server read works for a remote node.
package serverwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
	"serverwatch/internal/fleet"
)

const maxReplicaSeries = 5000

var errRemoteNode = errors.New("not available for a remote fleet node yet")

var nodeIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func isNodeID(id string) bool { return nodeIDRe.MatchString(id) }

// storeOptionsFor mirrors openConfiguredStore's retention parsing.
func storeOptionsFor(cfg *config.Config) StoreOptions {
	rawRet, _ := time.ParseDuration(cfg.Storage.RawRetention)
	rollupRet, _ := time.ParseDuration(cfg.Storage.RollupRetention)
	return StoreOptions{RawRetention: rawRet, RollupRetention: rollupRet, EventRetention: rollupRet}
}

type ingestState struct {
	AppliedSeq         uint64 `json:"applied_seq"`
	LastIngestTS       int64  `json:"last_ingest_ts"`
	DroppedOld         int64  `json:"dropped_out_of_order,omitempty"`
	DroppedCardinality int64  `json:"dropped_cardinality,omitempty"`
	// SkewSec is the smoothed server_time - sent_at (see RecordSkew). It is
	// persisted with the next applied batch, not on every sample.
	SkewSec float64 `json:"skew_sec,omitempty"`
}

// skewWarnSec is the clock skew beyond which a node is flagged (spec: warn
// above 30 s; the tracker marks it lagging at the same line).
const skewWarnSec = 30

type replicaNode struct {
	mu          sync.Mutex
	dir         string
	store       *tsFileStore
	st          ingestState
	last        map[string]int64
	series      int
	lastAlertTS int64
	alertLines  map[string]bool
	// lastEventStart guards downtime events (appended in Start order); it is
	// seeded from the store on open so the guard survives a master restart.
	lastEventStart int64
	live           atomic.Pointer[fleet.LiveUpdate]
	skewInit       bool // st.SkewSec holds at least one sample
	skewWarned     bool // |skew| is currently over skewWarnSec
}

// replicaSink implements fleet.Sink over per-node tsfile stores. tsfile opens
// and closes files per operation, so keeping every node's handle in memory
// holds no file descriptors.
type replicaSink struct {
	root  string
	opts  StoreOptions
	mu    sync.Mutex
	nodes map[string]*replicaNode
}

func newReplicaSink(root string, opts StoreOptions) *replicaSink {
	return &replicaSink{root: root, opts: opts, nodes: map[string]*replicaNode{}}
}

// evict drops id's cached *replicaNode so the next node() call reopens it
// from disk, re-seeding every ordering guard (n.last, lastEventStart,
// lastAlertTS/alertLines, series count) from what is actually durable.
//
// Apply/Backfill call this after any error from n.apply: apply may have
// already written some of the batch's records (an earlier metric/record in
// the loop can succeed before a later one fails) without ever reaching
// SyncMetrics/the AppliedSeq write, so the in-memory guard state built up
// while assuming the whole batch would succeed can be ahead of what's
// actually on disk. Left alone, that poisoned guard would silently reject
// the child's identical retry as duplicates and let the caller believe
// (empty error, unchanged AppliedSeq... except the retry would then wrongly
// look like a no-op success) records were durably applied when they were
// never written -- permanent silent data loss. Evicting forces the retry to
// re-derive every guard from disk via tsFileStore.LastTS/Events/Metrics/the
// alert log's last line, so it only ever skips what is actually already
// there.
//
// Safe without extra locking: the fleet master (internal/fleet/master.go,
// Master.nodeLock) serializes Apply/Backfill/Live per node id, so at most
// one n.apply call for this id is ever in flight; this evict always runs
// after that call has already returned, never concurrently with it.
func (r *replicaSink) evict(id string) {
	r.mu.Lock()
	delete(r.nodes, id)
	r.mu.Unlock()
}

// replicaWriteFailHook, when non-nil, lets a test force one of
// replicaNode.apply's durable writes to fail as if the real I/O had failed.
// op identifies which one is about to happen ("append" raw samples,
// "rollup" 1m AppendRollup, "event" AppendEvent, "alertlog" the alert log
// append).
// This is how the ordering-guard-survives-a-failed-batch tests
// (TestReplicaApplyEvictsNodeOnWriteFailure,
// TestReplicaAlertLogFailureThenRetryWritesOnce) inject a failure partway
// through a batch. nil (a no-op) in production.
var replicaWriteFailHook func(op string) error

func checkReplicaWriteFail(op string) error {
	if replicaWriteFailHook == nil {
		return nil
	}
	return replicaWriteFailHook(op)
}

func (r *replicaSink) node(id string) (*replicaNode, error) {
	if !isNodeID(id) {
		return nil, fmt.Errorf("fleet: invalid node id %q", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[id]; ok {
		return n, nil
	}
	dir := filepath.Join(r.root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st, err := newTSFileStore(dir, r.opts)
	if err != nil {
		return nil, err
	}
	n := &replicaNode{dir: dir, store: st, last: map[string]int64{}, alertLines: map[string]bool{}, lastEventStart: math.MinInt64}
	if evs, err := st.Events(0, math.MaxInt64); err == nil {
		for _, e := range evs {
			if e.Start > n.lastEventStart {
				n.lastEventStart = e.Start
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ingest.state")); err == nil {
		_ = json.Unmarshal(b, &n.st)
		n.skewInit = n.st.SkewSec != 0
	}
	if ms, err := st.Metrics(ResRaw); err == nil {
		n.series = len(ms)
	}
	if line := lastLine(filepath.Join(dir, "alertlog.jsonl")); line != nil {
		var h struct {
			Time int64 `json:"time"`
		}
		if json.Unmarshal(line, &h) == nil {
			n.lastAlertTS = h.Time
			n.alertLines[string(line)] = true
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "live.json")); err == nil {
		var u fleet.LiveUpdate
		if json.Unmarshal(b, &u) == nil {
			n.live.Store(&u)
		}
	}
	r.nodes[id] = n
	return n, nil
}

// lastLine returns the last non-empty line of path (reading at most 64 KiB).
func lastLine(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	off := fi.Size() - 64<<10
	if off < 0 {
		off = 0
	}
	b := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
		return nil
	}
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	if len(lines) == 0 || len(lines[len(lines)-1]) == 0 {
		return nil
	}
	return lines[len(lines)-1]
}

// AppliedSeq implements fleet.Sink.
func (r *replicaSink) AppliedSeq(id string) (uint64, error) {
	n, err := r.node(id)
	if err != nil {
		return 0, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.st.AppliedSeq, nil
}

// Apply implements fleet.Sink. On any error from n.apply, id's cached
// replicaNode is evicted (see evict's doc) so the child's retry re-seeds
// every ordering guard from disk instead of risking a poisoned in-memory
// guard silently dropping records that were never actually written.
func (r *replicaSink) Apply(id string, recs []fleet.Record) error {
	n, err := r.node(id)
	if err != nil {
		return err
	}
	if err := n.apply(recs, true); err != nil {
		r.evict(id)
		return err
	}
	return nil
}

// Backfill implements fleet.Sink. Same evict-on-error contract as Apply.
func (r *replicaSink) Backfill(id string, recs []fleet.Record) error {
	n, err := r.node(id)
	if err != nil {
		return err
	}
	if err := n.apply(recs, false); err != nil {
		r.evict(id)
		return err
	}
	return nil
}

// admit reports whether a point at ts may be appended to (res, metric),
// updating the cached last ts when it may. A non-nil error means LastTS
// itself failed (disk read error) -- the caller must abort apply with that
// error rather than silently treating the metric as rejected, which would
// otherwise black-hole it (a transient read error is not the same thing as
// "already have this point").
func (n *replicaNode) admit(metric string, res Resolution, ts int64) (bool, error) {
	key := string(res) + "|" + metric
	last, ok := n.last[key]
	if !ok {
		var err error
		if last, err = n.store.LastTS(metric, res); err != nil {
			return false, err
		}
		if last == math.MinInt64 {
			if n.series >= maxReplicaSeries {
				n.st.DroppedCardinality++
				return false, nil
			}
			n.series++
		}
	}
	if ts <= last {
		n.last[key] = last
		n.st.DroppedOld++
		return false, nil
	}
	n.last[key] = ts
	return true, nil
}

func (n *replicaNode) apply(recs []fleet.Record, sequenced bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	touched := map[string]bool{}
	events := false
	var alerts bytes.Buffer
	for _, rec := range recs {
		switch rec.Kind {
		case fleet.KindSamples:
			var d fleet.SamplesData
			if json.Unmarshal(rec.Data, &d) != nil {
				continue
			}
			if d.Res == "1m" {
				for m, p := range d.Rollups {
					ok, err := n.admit(m, Res1m, d.TS)
					if err != nil {
						return err
					}
					if !ok {
						continue
					}
					if err := checkReplicaWriteFail("rollup"); err != nil {
						return err
					}
					if err := n.store.AppendRollup(m, Point{TS: d.TS, Min: p.Min, Avg: p.Avg, Max: p.Max}); err != nil {
						return err
					}
					touched[m] = true
				}
				continue
			}
			ms := MetricSet{}
			for m, v := range d.Metrics {
				ok, err := n.admit(m, ResRaw, d.TS)
				if err != nil {
					return err
				}
				if ok {
					ms[m] = v
					touched[m] = true
				}
			}
			if len(ms) > 0 {
				if err := checkReplicaWriteFail("append"); err != nil {
					return err
				}
				if err := n.store.Append(d.TS, ms); err != nil {
					return err
				}
			}
			if d.TS > n.st.LastIngestTS {
				n.st.LastIngestTS = d.TS
			}
		case fleet.KindDownEvent:
			var e fleet.DownEventData
			if json.Unmarshal(rec.Data, &e) != nil {
				continue
			}
			if e.Start <= n.lastEventStart {
				n.st.DroppedOld++
				continue
			}
			n.lastEventStart = e.Start
			if err := checkReplicaWriteFail("event"); err != nil {
				return err
			}
			if err := n.store.AppendEvent(DownEvent{Type: e.Type, Start: e.Start, End: e.End, DurationSec: e.DurationSec}); err != nil {
				return err
			}
			events = true
		case fleet.KindAlert:
			var h struct {
				Time int64 `json:"time"`
			}
			if json.Unmarshal(rec.Data, &h) != nil {
				continue
			}
			var line bytes.Buffer
			if json.Compact(&line, rec.Data) != nil {
				continue
			}
			if h.Time < n.lastAlertTS || (h.Time == n.lastAlertTS && n.alertLines[line.String()]) {
				continue
			}
			if h.Time > n.lastAlertTS {
				n.lastAlertTS = h.Time
				n.alertLines = map[string]bool{}
			}
			n.alertLines[line.String()] = true
			alerts.Write(line.Bytes())
			alerts.WriteByte('\n')
		}
	}
	if alerts.Len() > 0 {
		if err := checkReplicaWriteFail("alertlog"); err != nil {
			return err
		}
		if err := appendSynced(filepath.Join(n.dir, "alertlog.jsonl"), alerts.Bytes()); err != nil {
			return err
		}
	}
	metrics := make([]string, 0, len(touched))
	for m := range touched {
		metrics = append(metrics, m)
	}
	sort.Strings(metrics)
	if err := n.store.SyncMetrics(metrics, events); err != nil {
		return err
	}
	if sequenced && len(recs) > 0 {
		n.st.AppliedSeq = recs[len(recs)-1].Seq
	}
	b, _ := json.Marshal(n.st)
	return writeFileSynced(filepath.Join(n.dir, "ingest.state"), b)
}

func appendSynced(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeFileSynced(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Live implements fleet.Sink. Every node posts one of these every few
// seconds, so this is the master's hottest write path and it is kept cheap:
// the latest view lives in memory (the node API reads the snapshot from
// there), live.json is rewritten for continuity across a master restart
// with a plain temp-file + rename and NO fsync (it is regenerated every few
// seconds, so losing the last one to a crash costs nothing), and alerts.json
// (read by the node API's ActiveAlerts) is rewritten only when the child's
// alert state actually changed.
func (r *replicaSink) Live(id string, u fleet.LiveUpdate) error {
	n, err := r.node(id)
	if err != nil {
		return err
	}
	prev := n.live.Load()
	if prev != nil && len(u.HostInfo) == 0 {
		u.HostInfo = prev.HostInfo // hostinfo is only sent every few minutes
	}
	n.live.Store(&u)
	if len(u.AlertState) > 0 && (prev == nil || !bytes.Equal(prev.AlertState, u.AlertState)) {
		if err := writeFileAtomic(filepath.Join(n.dir, "alerts.json"), u.AlertState, 0o600); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(u)
	return writeFileAtomic(filepath.Join(n.dir, "live.json"), b, 0o600)
}

// RecordSkew folds one clock-skew sample (server_time - sent_at, seconds)
// into id's smoothed skew (exponential moving average, weight 1/4 per
// sample) and returns it rounded, plus whether this sample took |skew| over
// skewWarnSec (true once per excursion, so the caller warns once).
func (r *replicaSink) RecordSkew(id string, sampleSec int64) (int64, bool) {
	n, err := r.node(id)
	if err != nil {
		return 0, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.skewInit {
		n.st.SkewSec, n.skewInit = float64(sampleSec), true
	} else {
		n.st.SkewSec = 0.75*n.st.SkewSec + 0.25*float64(sampleSec)
	}
	s := int64(math.Round(n.st.SkewSec))
	over := s > skewWarnSec || s < -skewWarnSec
	crossed := over && !n.skewWarned
	n.skewWarned = over
	return s, crossed
}

// Stats returns a copy of id's ingest counters (zero for an unknown id).
func (r *replicaSink) Stats(id string) ingestState {
	n, err := r.node(id)
	if err != nil {
		return ingestState{}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.st
}

// LiveOf returns the last live update for id, or nil.
func (r *replicaSink) LiveOf(id string) *fleet.LiveUpdate {
	n, err := r.node(id)
	if err != nil {
		return nil
	}
	return n.live.Load()
}

// maintSliceSize is how many of total replicas one master tick maintains so
// that each is maintained about once per interval: replica maintenance
// (Downsample + Prune, which rewrite and fsync series files) runs on the same
// cadence as the local store's (storeMaintenanceInterval), staggered across
// ticks instead of every node at once.
func maintSliceSize(total int, tick, interval time.Duration) int {
	if total <= 0 {
		return 0
	}
	ticks := int(interval / tick)
	if ticks < 1 {
		ticks = 1
	}
	return (total + ticks - 1) / ticks
}

// maintScheduler hands out the next slice of replicas to maintain, rotating
// through them so every node gets its turn.
type maintScheduler struct{ cursor int }

func (m *maintScheduler) next(ids []string, tick, interval time.Duration) []string {
	n := maintSliceSize(len(ids), tick, interval)
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ids[(m.cursor+i)%len(ids)])
	}
	if len(ids) > 0 {
		m.cursor = (m.cursor + n) % len(ids)
	}
	return out
}

// nodeIDs lists every replica directory on disk, sorted.
func (r *replicaSink) nodeIDs() []string {
	ents, err := os.ReadDir(r.root)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() && isNodeID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids
}

// maintainNode downsamples and prunes one replica. Downsampling uses the
// node's newest ingested sample time as "now", so a child whose backlog is
// still arriving never has a half-received minute rolled up early.
func (r *replicaSink) maintainNode(id string, now int64) {
	n, err := r.node(id)
	if err != nil {
		return
	}
	n.mu.Lock()
	lastTS := n.st.LastIngestTS
	n.mu.Unlock()
	if lastTS > 0 {
		_ = n.store.Downsample(lastTS)
	}
	_ = n.store.Prune(now)
}

// NodeAPI returns a core.API over id's replica.
func (r *replicaSink) NodeAPI(id string, getCfg func() *config.Config) (core.API, error) {
	n, err := r.node(id)
	if err != nil {
		return nil, err
	}
	getSnap := func() Snapshot {
		var s Snapshot
		if u := n.live.Load(); u != nil && len(u.Snapshot) > 0 {
			_ = json.Unmarshal(u.Snapshot, &s)
		}
		return s
	}
	reload := func(*config.Config) error { return errRemoteNode }
	return &replicaAPI{API: newInprocAPI(getSnap, getCfg, n.store, n.dir, reload, nil, nil), n: n}, nil
}

// replicaAPI serves reads from a replica and refuses what needs the live
// child (phase 1: container logs, config writes, acks, channel tests).
type replicaAPI struct {
	core.API
	n *replicaNode
}

func (a *replicaAPI) HostInfo() (core.HostInfoView, error) {
	var h HostInfo
	if u := a.n.live.Load(); u != nil && len(u.HostInfo) > 0 {
		_ = json.Unmarshal(u.HostInfo, &h)
	}
	return buildHostInfoView(h, time.Now().Unix()), nil
}

func (a *replicaAPI) Version() (string, error) {
	if u := a.n.live.Load(); u != nil {
		return u.Version, nil
	}
	return "", nil
}

func (a *replicaAPI) Doctor() (core.DoctorReport, error)         { return core.DoctorReport{}, errRemoteNode }
func (a *replicaAPI) ContainerLogs(string, int) (string, error)  { return "", errRemoteNode }
func (a *replicaAPI) ApplyConfig(*config.Config) error           { return errRemoteNode }
func (a *replicaAPI) AckAlert(string) error                      { return errRemoteNode }
func (a *replicaAPI) UnackAlert(string) error                    { return errRemoteNode }
func (a *replicaAPI) TestChannel(string) error                   { return errRemoteNode }
func (a *replicaAPI) ValidateChannel(config.ChannelConfig) error { return errRemoteNode }
func (a *replicaAPI) EnrollmentPIN(context.Context) (string, bool, error) {
	return "", false, errRemoteNode
}
func (a *replicaAPI) MonitorTargets(context.Context) ([]core.TargetView, error) {
	return nil, errRemoteNode
}
func (a *replicaAPI) Subscribe(context.Context) (<-chan core.Event, error) {
	return nil, errRemoteNode
}
