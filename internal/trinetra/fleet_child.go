// Package serverwatch: fleet_child.go adapts a child daemon to the fleet
// link: it tees local writes into the outbox, rebuilds dropped outbox ranges
// from local history, builds the live update, and raises local alerts when
// the link to the master is lost or the node is revoked.
package trinetra

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/fleet"
	"github.com/InfoDiveLabs/trinetra/internal/version"
)

// outboxTee appends local writes to the fleet outbox. Failures are counted
// and logged once per minute, never propagated: the local store is the
// source of truth. A record the outbox cannot write is not silently lost:
// Outbox.Append truncates any torn frame, moves to a fresh segment, and
// records the unsent range (including the failed record) as a gap that the
// shipper rebuilds from the local store (localGapFiller) before shipping
// anything newer. Only if the outbox cannot even persist that gap (for
// example the disk is completely unwritable) is the record left to the
// local store alone, and that failure is what the log line reports. A
// marshal failure is also only counted and logged.
type outboxTee struct {
	ob       *fleet.Outbox
	logf     func(string, ...any)
	failures atomic.Int64
	lastLog  atomic.Int64
}

func newOutboxTee(ob *fleet.Outbox, logf func(string, ...any)) *outboxTee {
	return &outboxTee{ob: ob, logf: logf}
}

func (t *outboxTee) put(kind string, ts int64, v any) {
	b, err := json.Marshal(v)
	if err == nil {
		_, err = t.ob.Append(kind, ts, b)
	}
	if err != nil {
		t.failures.Add(1)
		now := time.Now().Unix()
		if now-t.lastLog.Load() >= 60 {
			t.lastLog.Store(now)
			t.logf("fleet: outbox append failed (%d so far): %v", t.failures.Load(), err)
		}
	}
}

func (t *outboxTee) Samples(ts int64, ms MetricSet) {
	t.put(fleet.KindSamples, ts, fleet.SamplesData{TS: ts, Metrics: map[string]float64(ms)})
}

func (t *outboxTee) Event(e DownEvent) {
	t.put(fleet.KindDownEvent, e.Start, fleet.DownEventData{Type: e.Type, Start: e.Start, End: e.End, DurationSec: e.DurationSec})
}

func (t *outboxTee) Alert(ev AlertEvent) { t.put(fleet.KindAlert, ev.Time, ev) }

func (t *outboxTee) Failures() int64 { return t.failures.Load() }

type metricLister interface {
	Metrics(res Resolution) ([]string, error)
}

// localGapFiller answers fleet.GapFiller from the child's own tsfile store:
// raw points where raw retention still holds them, 1m rollups before that.
type localGapFiller struct {
	store        SampleStore
	alog         *AlertLog
	rawRetention time.Duration
	now          func() time.Time
}

func (g *localGapFiller) Fill(gap fleet.Gap) ([]fleet.Record, error) {
	if g.store == nil {
		return nil, errors.New("no local store")
	}
	ml, ok := g.store.(metricLister)
	if !ok {
		return nil, errors.New("local store cannot enumerate series")
	}
	cutoff := g.now().Unix() - int64(g.rawRetention/time.Second)
	var recs []fleet.Record
	if gap.MinTS < cutoff {
		to := min(gap.MaxTS, cutoff-1)
		metrics, err := ml.Metrics(Res1m)
		if err != nil {
			return nil, err
		}
		byTS := map[int64]map[string]fleet.RollupPoint{}
		for _, m := range metrics {
			pts, err := g.store.Query(m, gap.MinTS, to, Res1m)
			if err != nil {
				return nil, err
			}
			for _, p := range pts {
				if byTS[p.TS] == nil {
					byTS[p.TS] = map[string]fleet.RollupPoint{}
				}
				byTS[p.TS][m] = fleet.RollupPoint{Min: p.Min, Avg: p.Avg, Max: p.Max}
			}
		}
		for ts, r := range byTS {
			b, _ := json.Marshal(fleet.SamplesData{TS: ts, Res: "1m", Rollups: r})
			recs = append(recs, fleet.Record{Kind: fleet.KindSamples, TS: ts, Data: b})
		}
	}
	if gap.MaxTS >= cutoff {
		from := max(gap.MinTS, cutoff)
		metrics, err := ml.Metrics(ResRaw)
		if err != nil {
			return nil, err
		}
		byTS := map[int64]map[string]float64{}
		for _, m := range metrics {
			pts, err := g.store.Query(m, from, gap.MaxTS, ResRaw)
			if err != nil {
				return nil, err
			}
			for _, p := range pts {
				if byTS[p.TS] == nil {
					byTS[p.TS] = map[string]float64{}
				}
				byTS[p.TS][m] = p.Avg
			}
		}
		for ts, ms := range byTS {
			b, _ := json.Marshal(fleet.SamplesData{TS: ts, Metrics: ms})
			recs = append(recs, fleet.Record{Kind: fleet.KindSamples, TS: ts, Data: b})
		}
	}
	evs, err := g.store.Events(gap.MinTS, gap.MaxTS)
	if err != nil {
		return nil, err
	}
	for _, e := range evs {
		if e.Start < gap.MinTS || e.Start > gap.MaxTS {
			continue
		}
		b, _ := json.Marshal(fleet.DownEventData{Type: e.Type, Start: e.Start, End: e.End, DurationSec: e.DurationSec})
		recs = append(recs, fleet.Record{Kind: fleet.KindDownEvent, TS: e.Start, Data: b})
	}
	if g.alog != nil {
		aes, err := g.alog.AlertEventsSince(gap.MinTS)
		if err != nil {
			return nil, err
		}
		for _, ae := range aes {
			if ae.Time > gap.MaxTS {
				continue
			}
			b, _ := json.Marshal(ae)
			recs = append(recs, fleet.Record{Kind: fleet.KindAlert, TS: ae.Time, Data: b})
		}
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].TS < recs[j].TS })
	return recs, nil
}

// liveBuilder assembles the latest-wins LiveUpdate. Host inventory is static
// for a boot and relatively expensive, so it rides along only every 10 min.
type liveBuilder struct {
	snap           func() Snapshot
	alertStatePath string
	host           func() HostInfo
	mu             sync.Mutex
	lastHost       time.Time
}

func newLiveBuilder(snap func() Snapshot, alertStatePath string, host func() HostInfo) *liveBuilder {
	return &liveBuilder{snap: snap, alertStatePath: alertStatePath, host: host}
}

func (l *liveBuilder) Build() (fleet.LiveUpdate, error) {
	sb, err := json.Marshal(l.snap())
	if err != nil {
		return fleet.LiveUpdate{}, err
	}
	u := fleet.LiveUpdate{Version: version.String(), Snapshot: sb}
	if b, err := os.ReadFile(l.alertStatePath); err == nil && json.Valid(b) {
		u.AlertState = b
	}
	l.mu.Lock()
	due := time.Since(l.lastHost) >= 10*time.Minute
	if due {
		l.lastHost = time.Now()
	}
	l.mu.Unlock()
	if due {
		if hb, err := json.Marshal(l.host()); err == nil {
			u.HostInfo = hb
		}
	}
	return u, nil
}

const linkDownWarnAfter = 10 * 60 // seconds

// childLinkAlerts decides the child's local alerts about its own link.
type childLinkAlerts struct {
	linkDownRaised bool
	revokedRaised  bool
}

func (c *childLinkAlerts) Plan(st fleet.LinkStatus, masterURL string, startedAt, now int64) []Alert {
	var out []Alert
	if st.State == "revoked" {
		if !c.revokedRaised {
			c.revokedRaised = true
			out = append(out, Alert{Key: "fleet:link:revoked", Severity: SevCritical, Kind: "fire", Source: "fleet", Time: now,
				Title: "⛔ This node was revoked by the fleet master. Telemetry stays on this host and is no longer shipped; alerts are sent locally."})
		}
		return out
	}
	lastOK := st.LastAck
	if lastOK == 0 {
		lastOK = startedAt
	}
	// "catching up" is reachable too: live updates get through while the
	// spooled backlog drains.
	reachable := st.State == fleet.LinkLinked || st.State == fleet.LinkCatchingUp
	switch {
	case reachable && c.linkDownRaised:
		c.linkDownRaised = false
		out = append(out, Alert{Key: "fleet:link:down", Severity: SevWarning, Kind: "recover", Source: "fleet", Time: now,
			Title: "🟢 Fleet link restored; spooled telemetry is being sent to the master."})
	case !reachable && !c.linkDownRaised && now-lastOK >= linkDownWarnAfter:
		c.linkDownRaised = true
		out = append(out, Alert{Key: "fleet:link:down", Severity: SevWarning, Kind: "fire", Source: "fleet", Time: now,
			Title: fmt.Sprintf("⚠ Fleet master %s unreachable for %d min. Alerts continue locally; telemetry is spooled (%.1f MB) and will be sent when it's back.",
				masterURL, (now-lastOK)/60, float64(st.Outbox.Bytes)/(1<<20))})
	}
	return out
}
