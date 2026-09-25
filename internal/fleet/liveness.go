package fleet

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// State is a node's liveness as the master sees it.
type State string

const (
	StateOnline  State = "online"
	StateLagging State = "lagging"
	StateStale   State = "stale"
	StateDown    State = "down"
	StateRevoked State = "revoked"
)

// TrackerConfig tunes liveness.
type TrackerConfig struct {
	StaleAfter   time.Duration
	DownAfter    time.Duration
	LagAfter     time.Duration
	MassWindow   time.Duration
	MassFraction float64
	MassMin      int
	// MaxSkew: a node whose clock differs from the master's by more than
	// this is lagging (its timestamps are not trustworthy for ordering).
	MaxSkew time.Duration
}

// DefaultTrackerConfig returns the spec defaults with a configurable down threshold.
func DefaultTrackerConfig(downAfter time.Duration) TrackerConfig {
	return TrackerConfig{StaleAfter: 30 * time.Second, DownAfter: downAfter, LagAfter: 5 * time.Minute,
		MassWindow: time.Minute, MassFraction: 0.5, MassMin: 3, MaxSkew: 30 * time.Second}
}

type tracked struct {
	lastSeen   int64
	backlogAge int64
	skew       int64 // smoothed server_time - sent_at, seconds
	revoked    bool
	state      State
}

// Tracker derives node states from contact times. Safe for concurrent use.
type Tracker struct {
	cfg   TrackerConfig
	mu    sync.Mutex
	nodes map[string]*tracked
}

// Transition is a state change found by Evaluate.
type Transition struct {
	NodeID   string
	From, To State
	LastSeen int64
}

// Evaluation is the result of one Evaluate pass.
type Evaluation struct {
	Transitions []Transition
	MassDown    []string
}

// NewTracker builds an empty tracker.
func NewTracker(cfg TrackerConfig) *Tracker {
	return &Tracker{cfg: cfg, nodes: map[string]*tracked{}}
}

// Seed registers ids as just seen at now. Called at master start so the
// master's own downtime never counts against its nodes.
func (t *Tracker) Seed(ids []string, revoked map[string]bool, now int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range ids {
		st := StateOnline
		if revoked[id] {
			st = StateRevoked
		}
		t.nodes[id] = &tracked{lastSeen: now, revoked: revoked[id], state: st}
	}
}

// Seen records contact from id (adding it if new). backlogAgeSec < 0 means
// contact without outbox info (e.g. an ingest or backfill call) and leaves
// the last reported backlog age unchanged.
func (t *Tracker) Seen(id string, now int64, backlogAgeSec int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.nodes[id]
	if !ok {
		n = &tracked{state: StateOnline}
		t.nodes[id] = n
	}
	if now > n.lastSeen {
		n.lastSeen = now
	}
	if backlogAgeSec >= 0 {
		n.backlogAge = backlogAgeSec
	}
}

// SetRevoked marks id revoked or not.
func (t *Tracker) SetRevoked(id string, revoked bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n, ok := t.nodes[id]; ok {
		n.revoked = revoked
	}
}

// Forget stops tracking id (a node removed from the fleet).
func (t *Tracker) Forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.nodes, id)
}

// SetSkew records id's smoothed clock skew (server_time - sent_at, seconds).
func (t *Tracker) SetSkew(id string, skewSec int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n, ok := t.nodes[id]; ok {
		n.skew = skewSec
	}
}

// State returns id's state as of the last Evaluate ("" if unknown).
func (t *Tracker) State(id string) State {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n, ok := t.nodes[id]; ok {
		return n.state
	}
	return ""
}

func (t *Tracker) classify(n *tracked, now int64) State {
	age := time.Duration(now-n.lastSeen) * time.Second
	switch {
	case n.revoked:
		return StateRevoked
	case age > t.cfg.DownAfter:
		return StateDown
	case age > t.cfg.StaleAfter:
		return StateStale
	case time.Duration(n.backlogAge)*time.Second > t.cfg.LagAfter:
		return StateLagging
	case t.cfg.MaxSkew > 0 && time.Duration(abs64(n.skew))*time.Second > t.cfg.MaxSkew:
		return StateLagging
	}
	return StateOnline
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Evaluate recomputes every state and reports transitions and mass loss.
func (t *Tracker) Evaluate(now int64) Evaluation {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ev Evaluation
	ids := make([]string, 0, len(t.nodes))
	for id := range t.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	anyDown, total := false, 0
	var lost []string
	windowStart := now - int64((t.cfg.DownAfter+t.cfg.MassWindow)/time.Second)
	for _, id := range ids {
		n := t.nodes[id]
		st := t.classify(n, now)
		if st != n.state {
			ev.Transitions = append(ev.Transitions, Transition{NodeID: id, From: n.state, To: st, LastSeen: n.lastSeen})
			n.state = st
		}
		if st == StateRevoked {
			continue
		}
		total++
		if st == StateDown {
			anyDown = true
		}
		if (st == StateDown || st == StateStale) && n.lastSeen >= windowStart {
			lost = append(lost, id)
		}
	}
	if anyDown && len(lost) >= t.cfg.MassMin && float64(len(lost)) >= t.cfg.MassFraction*float64(total) {
		ev.MassDown = lost
	}
	return ev
}

// AlertIntent is an alert the daemon should raise or resolve.
type AlertIntent struct {
	Key      string
	Title    string
	Critical bool
	Recover  bool
}

// NodeAlerter turns evaluations into alerts, folding a mass disconnect into
// one fleet-connectivity incident instead of one page per node.
type NodeAlerter struct {
	alerted     map[string]int64 // node id -> down since (last seen)
	massActive  bool
	massMembers map[string]bool
}

// NewNodeAlerter builds an alerter with no open alerts.
func NewNodeAlerter() *NodeAlerter {
	return &NodeAlerter{alerted: map[string]int64{}, massMembers: map[string]bool{}}
}

// humanDur renders an alert duration: "35s" under a minute, rounded
// minutes ("2m") under an hour, then hours and minutes ("1h5m").
func humanDur(sec int64) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	m := (sec + 30) / 60 // rounded to the nearest minute
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	if m%60 == 0 {
		return fmt.Sprintf("%dh", m/60)
	}
	return fmt.Sprintf("%dh%dm", m/60, m%60)
}

// Forget drops id from alerting (the node was removed from the fleet): an
// open node-down page is resolved, and if id was the last member of an open
// fleet-connectivity incident that is resolved too.
func (a *NodeAlerter) Forget(id, name string) []AlertIntent {
	var out []AlertIntent
	if _, ok := a.alerted[id]; ok {
		delete(a.alerted, id)
		out = append(out, AlertIntent{Key: "fleet:node:" + id + ":down", Title: fmt.Sprintf("🟢 %s was removed from the fleet", name), Recover: true})
	}
	if a.massMembers[id] {
		delete(a.massMembers, id)
		if a.massActive && len(a.massMembers) == 0 {
			a.massActive = false
			out = append(out, AlertIntent{Key: "fleet:connectivity", Title: "🟢 Fleet connectivity restored", Recover: true})
		}
	}
	return out
}

// Plan returns the alerts to raise/resolve for ev. A mass disconnect opens
// one fleet:connectivity incident; nodes that are part of it (or go stale or
// down while it is open, even if they join after the incident opened) are
// not paged individually. Membership is added, never inferred solely from
// the opening ev.MassDown set: every tick the incident is open, any node
// newly reported in ev.MassDown or newly transitioning to stale or down
// (and not already individually alerted) joins the incident. A member
// leaves only when it transitions to online, lagging or revoked, and the
// incident resolves once every member has left — so a node that goes silent
// after the incident opened, and is still lost when the original members
// recover, correctly keeps the incident open instead of triggering an early
// "restored" alert followed by a late, separate individual page.
func (a *NodeAlerter) Plan(ev Evaluation, now int64, name func(id string) string) []AlertIntent {
	var out []AlertIntent
	if !a.massActive && len(ev.MassDown) > 0 {
		a.massActive = true
		out = append(out, AlertIntent{
			Key:      "fleet:connectivity",
			Title:    fmt.Sprintf("🔴 Fleet connectivity: %d nodes lost contact at once (check the master's network)", len(ev.MassDown)),
			Critical: true,
		})
	}
	if a.massActive {
		for _, id := range ev.MassDown {
			if _, ok := a.alerted[id]; !ok {
				a.massMembers[id] = true
			}
		}
	}
	for _, tr := range ev.Transitions {
		switch tr.To {
		case StateDown, StateStale:
			if a.massActive {
				if _, ok := a.alerted[tr.NodeID]; !ok {
					a.massMembers[tr.NodeID] = true
				}
				continue
			}
			if tr.To == StateStale {
				continue // still lost; nothing to do
			}
			a.alerted[tr.NodeID] = tr.LastSeen
			out = append(out, AlertIntent{
				Key:      "fleet:node:" + tr.NodeID + ":down",
				Title:    fmt.Sprintf("🔴 %s is down: no contact for %s", name(tr.NodeID), humanDur(now-tr.LastSeen)),
				Critical: true,
			})
		default: // online, lagging, revoked: the node is reachable again (or retired)
			delete(a.massMembers, tr.NodeID)
			if since, ok := a.alerted[tr.NodeID]; ok {
				delete(a.alerted, tr.NodeID)
				if tr.To != StateRevoked {
					out = append(out, AlertIntent{
						Key:     "fleet:node:" + tr.NodeID + ":down",
						Title:   fmt.Sprintf("🟢 %s is back after %s", name(tr.NodeID), humanDur(now-since)),
						Recover: true,
					})
				}
			}
		}
	}
	if a.massActive && len(a.massMembers) == 0 {
		a.massActive = false
		out = append(out, AlertIntent{Key: "fleet:connectivity", Title: "🟢 Fleet connectivity restored", Recover: true})
	}
	return out
}
