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
}

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

// Apply implements fleet.Sink.
func (r *replicaSink) Apply(id string, recs []fleet.Record) error {
	n, err := r.node(id)
	if err != nil {
		return err
	}
	return n.apply(recs, true)
}

// Backfill implements fleet.Sink.
func (r *replicaSink) Backfill(id string, recs []fleet.Record) error {
	n, err := r.node(id)
	if err != nil {
		return err
	}
	return n.apply(recs, false)
}

// admit reports whether a point at ts may be appended to (res, metric),
// updating the cached last ts when it may.
func (n *replicaNode) admit(metric string, res Resolution, ts int64) bool {
	key := string(res) + "|" + metric
	last, ok := n.last[key]
	if !ok {
		var err error
		if last, err = n.store.LastTS(metric, res); err != nil {
			return false
		}
		if last == math.MinInt64 {
			if n.series >= maxReplicaSeries {
				n.st.DroppedCardinality++
				return false
			}
			n.series++
		}
	}
	if ts <= last {
		n.last[key] = last
		n.st.DroppedOld++
		return false
	}
	n.last[key] = ts
	return true
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
					if !n.admit(m, Res1m, d.TS) {
						continue
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
				if n.admit(m, ResRaw, d.TS) {
					ms[m] = v
					touched[m] = true
				}
			}
			if len(ms) > 0 {
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

// Live implements fleet.Sink: the latest view is kept in memory and written
// (unsynced; it is regenerated every few seconds) for the node API and for
// continuity across a master restart.
func (r *replicaSink) Live(id string, u fleet.LiveUpdate) error {
	n, err := r.node(id)
	if err != nil {
		return err
	}
	if prev := n.live.Load(); prev != nil && len(u.HostInfo) == 0 {
		u.HostInfo = prev.HostInfo // hostinfo is only sent every few minutes
	}
	n.live.Store(&u)
	if len(u.Snapshot) > 0 {
		if err := writeFileAtomic(filepath.Join(n.dir, "snapshot.json"), u.Snapshot, 0o600); err != nil {
			return err
		}
	}
	if len(u.AlertState) > 0 {
		if err := writeFileAtomic(filepath.Join(n.dir, "alerts.json"), u.AlertState, 0o600); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(u)
	return writeFileAtomic(filepath.Join(n.dir, "live.json"), b, 0o600)
}

// LiveOf returns the last live update for id, or nil.
func (r *replicaSink) LiveOf(id string) *fleet.LiveUpdate {
	n, err := r.node(id)
	if err != nil {
		return nil
	}
	return n.live.Load()
}

// Maintain downsamples and prunes every known replica. Downsampling uses the
// node's newest ingested sample time as "now", so a child whose backlog is
// still arriving never has a half-received minute rolled up early.
func (r *replicaSink) Maintain(now int64) {
	ents, err := os.ReadDir(r.root)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || !isNodeID(e.Name()) {
			continue
		}
		n, err := r.node(e.Name())
		if err != nil {
			continue
		}
		n.mu.Lock()
		lastTS := n.st.LastIngestTS
		n.mu.Unlock()
		if lastTS > 0 {
			_ = n.store.Downsample(lastTS)
		}
		_ = n.store.Prune(now)
	}
}

// NodeAPI returns a core.API over id's replica.
func (r *replicaSink) NodeAPI(id string, getCfg func() *config.Config) (core.API, error) {
	n, err := r.node(id)
	if err != nil {
		return nil, err
	}
	getSnap := func() Snapshot {
		var s Snapshot
		if b, err := os.ReadFile(filepath.Join(n.dir, "snapshot.json")); err == nil {
			_ = json.Unmarshal(b, &s)
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
