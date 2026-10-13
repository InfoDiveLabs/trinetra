// Package trinetra: fleet_replica.go is the master's side of fleet ingest.
package trinetra

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

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
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
	AppliedSeq   uint64 `json:"applied_seq"`
	LastIngestTS int64  `json:"last_ingest_ts"`
	// DroppedOutOfOrder keeps the pre-split key, whose counter also held
	// duplicates, so an old ingest.state loads its count here.
	DroppedOutOfOrder  int64 `json:"dropped_out_of_order,omitempty"`
	DroppedDuplicate   int64 `json:"dropped_duplicate,omitempty"`
	DroppedCardinality int64 `json:"dropped_cardinality,omitempty"`
	// WarnedOutOfOrder / WarnedCardinality are the drop counters as of the master's last drop
	// check (see masterLoop.checkDrops).
	WarnedOutOfOrder  int64 `json:"warned_out_of_order"`
	WarnedCardinality int64 `json:"warned_cardinality"`
	// SkewSec is the filtered server_time - sent_at (see RecordSkew).
	SkewSec float64 `json:"skew_sec,omitempty"`
}

// skewWarnSec is the clock skew beyond which a node is flagged (spec: warn
// above 30 s; the tracker marks it lagging at the same line).
const skewWarnSec = 30

// skewWindow is how many recent skew samples RecordSkew filters over.
const (
	skewWindow  = 10
	skewConfirm = 3
)

type replicaNode struct {
	mu     sync.Mutex
	dir    string
	store  *tsFileStore
	st     ingestState
	last   map[string]int64
	series int
	// lastAlertTS/alertLines are the KindAlert ordering/dedupe guard, scoped PER ALERT KEY
	// (map keyed by AlertEvent.Key).
	lastAlertTS map[string]int64
	alertLines  map[string]map[string]bool
	// lastEventStart guards downtime events (appended in Start order); it is
	// seeded from the store on open so the guard survives a master restart.
	lastEventStart int64
	live           atomic.Pointer[fleet.LiveUpdate]
	// alertsWritten is the alert state last written to alerts.json successfully (guarded by
	// mu); Live compares against it, not against the last update received.
	alertsWritten []byte
	skewInit      bool    // st.SkewSec holds at least one sample
	skewWarned    bool    // the reported skew is currently over skewWarnSec
	skewSamples   []int64 // the last skewWindow raw samples, oldest first
	skewOverRun   int     // consecutive filtered estimates over skewWarnSec
}

// replicaSink implements fleet.Sink over per-node tsfile stores. tsfile opens and closes
// files per operation, so keeping every node's handle in memory holds no file descriptors.
type replicaSink struct {
	root  string
	opts  StoreOptions
	mu    sync.Mutex
	nodes map[string]*replicaNode
	// onAlert, if set, is called once for every genuinely new (not a byte-identical re-send)
	// KindAlert record applied for a node.
	onAlert func(nodeID string, ev AlertEvent)
	// onAckSync, if set, is called whenever a node's alerts.json actually changes (see Live):
	// the master alerting engine's entry point for a child-side ack/unack.
	onAckSync func(nodeID string, as json.RawMessage)

	// hub/rpc/incidents wire remote ack/unack and remote container logs into NodeAPI's
	// replicaAPI.
	hub       *fleet.Hub
	rpc       *rpcRegistry
	incidents *incidentStore
}

func newReplicaSink(root string, opts StoreOptions, onAlert func(nodeID string, ev AlertEvent)) *replicaSink {
	return &replicaSink{root: root, opts: opts, nodes: map[string]*replicaNode{}, onAlert: onAlert}
}

// reseed rebuilds every in-memory ordering guard of n from what is actually durable on
// disk: the per-series last-ts cache (cleared, so it is re-read from the store on demand).
func (n *replicaNode) reseed() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.seedLocked()
}

// seedLocked loads n's guards and counters from disk (see reseed).
func (n *replicaNode) seedLocked() {
	n.last = map[string]int64{}
	n.lastEventStart = math.MinInt64
	if evs, err := n.store.Events(0, math.MaxInt64); err == nil {
		for _, e := range evs {
			if e.Start > n.lastEventStart {
				n.lastEventStart = e.Start
			}
		}
	}
	skew := n.st.SkewSec
	n.st = ingestState{}
	if b, err := os.ReadFile(filepath.Join(n.dir, "ingest.state")); err == nil {
		_ = json.Unmarshal(b, &n.st)
		var w struct {
			OutOfOrder *int64 `json:"warned_out_of_order"`
		}
		if json.Unmarshal(b, &w) == nil && w.OutOfOrder == nil {
			// Written before the drop-warning baseline existed: its counts.
			n.st.WarnedOutOfOrder, n.st.WarnedCardinality = n.st.DroppedOutOfOrder, n.st.DroppedCardinality
		}
	}
	if n.skewInit {
		n.st.SkewSec = skew
	} else {
		n.skewInit = n.st.SkewSec != 0
	}
	if ms, err := n.store.Metrics(ResRaw); err == nil {
		n.series = len(ms)
	}
	// The dedupe set is scoped PER ALERT KEY (see the struct field doc comment): several
	// alerts, of possibly different keys.
	n.lastAlertTS, n.alertLines = map[string]int64{}, map[string]map[string]bool{}
	lines := tailLines(filepath.Join(n.dir, "alertlog.jsonl"))
	type parsed struct {
		key  string
		time int64
		line []byte
	}
	var evs []parsed
	for _, l := range lines {
		var h struct {
			Key  string `json:"key"`
			Time int64  `json:"time"`
		}
		if json.Unmarshal(l, &h) != nil {
			continue
		}
		evs = append(evs, parsed{key: h.Key, time: h.Time, line: l})
		if cur, ok := n.lastAlertTS[h.Key]; !ok || h.Time > cur {
			n.lastAlertTS[h.Key] = h.Time
		}
	}
	for _, ev := range evs {
		if ev.time != n.lastAlertTS[ev.key] {
			continue
		}
		if n.alertLines[ev.key] == nil {
			n.alertLines[ev.key] = map[string]bool{}
		}
		n.alertLines[ev.key][string(ev.line)] = true
	}
}

// replicaWriteFailHook, when non-nil, lets a test force one of replicaNode.apply's durable
// writes to fail as if the real I/O had failed. op identifies which one is about to happen.
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
	n := &replicaNode{dir: dir, store: st}
	n.seedLocked()
	if b, err := os.ReadFile(filepath.Join(dir, "live.json")); err == nil {
		var u fleet.LiveUpdate
		if json.Unmarshal(b, &u) == nil {
			n.live.Store(&u)
		}
	}
	r.nodes[id] = n
	return n, nil
}

// tailLines returns the complete non-empty lines in the last 64 KiB of path, oldest first.
func tailLines(path string) [][]byte {
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
	lines := bytes.Split(b, []byte("\n"))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // starts mid-line
	}
	var out [][]byte
	for _, l := range lines {
		if len(l) > 0 {
			out = append(out, l)
		}
	}
	return out
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

// Apply implements fleet.Sink.
func (r *replicaSink) Apply(id string, recs []fleet.Record) error {
	n, err := r.node(id)
	if err != nil {
		return err
	}
	if err := n.apply(id, recs, true, r.onAlert); err != nil {
		n.reseed()
		return err
	}
	return nil
}

// Backfill implements fleet.Sink. Same reseed-on-error contract as Apply.
func (r *replicaSink) Backfill(id string, recs []fleet.Record) error {
	n, err := r.node(id)
	if err != nil {
		return err
	}
	if err := n.apply(id, recs, false, r.onAlert); err != nil {
		n.reseed()
		return err
	}
	return nil
}

// admit reports whether a point at ts may be appended to (res, metric), updating the cached
// last ts when it may.
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
		if ts == last {
			n.st.DroppedDuplicate++ // a re-sent copy (refill, retried batch)
		} else {
			n.st.DroppedOutOfOrder++
		}
		return false, nil
	}
	n.last[key] = ts
	return true, nil
}

func (n *replicaNode) apply(id string, recs []fleet.Record, sequenced bool, onAlert func(nodeID string, ev AlertEvent)) error {
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
			if e.Start == n.lastEventStart {
				n.st.DroppedDuplicate++
				continue
			}
			if e.Start < n.lastEventStart {
				n.st.DroppedOutOfOrder++
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
			var ev AlertEvent
			if json.Unmarshal(rec.Data, &ev) != nil {
				continue
			}
			var line bytes.Buffer
			if json.Compact(&line, rec.Data) != nil {
				continue
			}
			// Scoped per ALERT KEY (never a single guard shared across every key on the node): a
			// delayed fallback record for one key.
			lastTS := n.lastAlertTS[ev.Key]
			if ev.Time == lastTS && n.alertLines[ev.Key][line.String()] {
				n.st.DroppedDuplicate++
				continue
			}
			// An older alert is skipped uncounted: the dedupe set only holds the newest timestamp's
			// lines, so it cannot tell a re-sent copy from a genuinely late alert.
			if ev.Time < lastTS {
				continue
			}
			if ev.Time > lastTS {
				n.lastAlertTS[ev.Key] = ev.Time
				n.alertLines[ev.Key] = map[string]bool{}
			}
			n.alertLines[ev.Key][line.String()] = true
			alerts.Write(line.Bytes())
			alerts.WriteByte('\n')
			// The master alerting engine's entry point for a genuinely new (not a byte-identical
			// re-send) alert record: see fleet_engine.go's HandleChildAlert.
			if onAlert != nil {
				onAlert(id, ev)
			}
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
	return writeFileAtomicSynced(filepath.Join(n.dir, "ingest.state"), b, 0o600)
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

// Live implements fleet.Sink.
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
	changed, err := n.writeAlertState(u.AlertState)
	if err != nil {
		return fmt.Errorf("write alerts.json: %w", err)
	}
	// onAckSync: a child's own AlertState.Ack -- via a manual `trinetra alerts ack` on that
	// node, or the master's own AckIncident push applied there.
	if changed && r.onAckSync != nil {
		r.onAckSync(id, u.AlertState)
	}
	b, _ := json.Marshal(u)
	return writeFileAtomic(filepath.Join(n.dir, "live.json"), b, 0o600)
}

// writeAlertState rewrites alerts.json when as differs from the last
// successfully written state, reporting whether it actually wrote.
func (n *replicaNode) writeAlertState(as json.RawMessage) (bool, error) {
	if len(as) == 0 {
		return false, nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.alertsWritten != nil && bytes.Equal(n.alertsWritten, as) {
		return false, nil
	}
	if err := checkReplicaWriteFail("alerts"); err != nil {
		return false, err
	}
	if err := writeFileAtomic(filepath.Join(n.dir, "alerts.json"), as, 0o600); err != nil {
		return false, err
	}
	n.alertsWritten = bytes.Clone(as)
	return true, nil
}

// RecordSkew adds one clock-skew sample (server_time - sent_at, seconds, server_time taken
// at request arrival) for id and returns the node's reported skew.
func (r *replicaSink) RecordSkew(id string, sampleSec int64) (int64, bool) {
	n, err := r.node(id)
	if err != nil {
		return 0, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.skewSamples = append(n.skewSamples, sampleSec)
	if len(n.skewSamples) > skewWindow {
		n.skewSamples = n.skewSamples[len(n.skewSamples)-skewWindow:]
	}
	est := n.skewSamples[0]
	for _, v := range n.skewSamples[1:] {
		if abs64(v) < abs64(est) {
			est = v
		}
	}
	over := est > skewWarnSec || est < -skewWarnSec
	if !over {
		n.skewOverRun = 0
	} else if n.skewOverRun++; n.skewOverRun < skewConfirm {
		return int64(math.Round(n.st.SkewSec)), false // not yet confirmed
	}
	n.st.SkewSec, n.skewInit = float64(est), true
	crossed := over && !n.skewWarned
	n.skewWarned = over
	return est, crossed
}

// MarkDropsChecked records outOfOrder/cardinality as id's drop-warning baseline and
// persists it in ingest.state (only when it changed), so the next check.
func (r *replicaSink) MarkDropsChecked(id string, outOfOrder, cardinality int64) error {
	n, err := r.node(id)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.st.WarnedOutOfOrder == outOfOrder && n.st.WarnedCardinality == cardinality {
		return nil
	}
	n.st.WarnedOutOfOrder, n.st.WarnedCardinality = outOfOrder, cardinality
	b, _ := json.Marshal(n.st)
	return writeFileAtomicSynced(filepath.Join(n.dir, "ingest.state"), b, 0o600)
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

// maintSliceSize is how many of total replicas one master tick maintains so that each is
// maintained about once per interval: replica maintenance.
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

// maintainNode downsamples and prunes one replica.
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
	return &replicaAPI{
		API: newInprocAPI(getSnap, getCfg, n.store, n.dir, reload, nil, nil),
		n:   n, nodeID: id, hub: r.hub, rpc: r.rpc, incidents: r.incidents,
	}, nil
}

// replicaAPI serves reads from a replica and refuses what still needs the live child
// directly (config writes, channel tests).
type replicaAPI struct {
	core.API
	n         *replicaNode
	nodeID    string
	hub       *fleet.Hub
	rpc       *rpcRegistry
	incidents *incidentStore
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

func (a *replicaAPI) Doctor() (core.DoctorReport, error) { return core.DoctorReport{}, errRemoteNode }
func (a *replicaAPI) ApplyConfig(*config.Config) error   { return errRemoteNode }
func (a *replicaAPI) TestChannel(string) error           { return errRemoteNode }

// AckAlert/UnackAlert: push an ack/unack frame down this node's stream connection -- the
// child applies it locally via AlertState.Ack/ Unack (fleet_lease.go's applyAckFrame).
func (a *replicaAPI) AckAlert(key string) error   { return a.remoteAck(key, false) }
func (a *replicaAPI) UnackAlert(key string) error { return a.remoteAck(key, true) }

func (a *replicaAPI) remoteAck(key string, unack bool) error {
	if a.hub == nil || !a.hub.Connected(a.nodeID) {
		return errNodeNotConnected
	}
	data, err := json.Marshal(ackFrameData{Key: key})
	if err != nil {
		return err
	}
	frameType := "ack"
	if unack {
		frameType = "unack"
	}
	a.hub.Push(a.nodeID, fleet.Frame{Type: frameType, Data: data})
	if !unack && a.incidents != nil {
		// Also record the ack on the master's own incident view immediately, so the UI need not
		// wait for anything to come back over the stream.
		if inc, ok := a.incidents.OpenAlertIncident(a.nodeID, key); ok {
			_, _ = a.incidents.Ack(inc.ID, "web", time.Now().Unix())
		}
	}
	return nil
}

// ContainerLogs runs `docker logs` on the remote node via an RPC over the master-to-child
// stream (fleet_rpc.go).
func (a *replicaAPI) ContainerLogs(name string, lines int) (string, error) {
	if a.hub == nil || a.rpc == nil {
		return "", errRemoteNode
	}
	args, err := json.Marshal(rpcContainerLogsArgs{Name: name, Lines: lines})
	if err != nil {
		return "", err
	}
	res, err := a.rpc.Call(a.hub, a.nodeID, "container_logs", args)
	if err != nil {
		return "", err
	}
	if !res.OK {
		// res.Error is authored end-to-end by the remote child (Hub.handleRPC only authenticates
		// the node), so a buggy or compromised child can send any text.
		return "", fmt.Errorf("container_logs: child reported %q", res.Error)
	}
	return res.Output, nil
}
func (a *replicaAPI) ValidateChannel(config.ChannelConfig) error { return errRemoteNode }

// UpdateStatus/UpdateCheck/UpdateApply/UpdateRollback are not available for a remote fleet
// node yet -- self-update is a per-node operation with no RPC plumbing.
func (a *replicaAPI) UpdateStatus() (core.UpdateStatusView, error) {
	return core.UpdateStatusView{}, errRemoteNode
}
func (a *replicaAPI) UpdateCheck(context.Context) (core.UpdateStatusView, error) {
	return core.UpdateStatusView{}, errRemoteNode
}
func (a *replicaAPI) UpdateApply(context.Context, string) error { return errRemoteNode }
func (a *replicaAPI) UpdateRollback() error                     { return errRemoteNode }
func (a *replicaAPI) EnrollmentPIN(context.Context) (string, bool, error) {
	return "", false, errRemoteNode
}
func (a *replicaAPI) MonitorTargets(context.Context) ([]core.TargetView, error) {
	return nil, errRemoteNode
}
func (a *replicaAPI) Subscribe(context.Context) (<-chan core.Event, error) {
	return nil, errRemoteNode
}
