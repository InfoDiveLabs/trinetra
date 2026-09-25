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
}

// DefaultTrackerConfig returns the spec defaults with a configurable down threshold.
func DefaultTrackerConfig(downAfter time.Duration) TrackerConfig {
	return TrackerConfig{StaleAfter: 30 * time.Second, DownAfter: downAfter, LagAfter: 5 * time.Minute,
		MassWindow: time.Minute, MassFraction: 0.5, MassMin: 3}
}

type tracked struct {
	lastSeen   int64
	backlogAge int64
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
	}
	return StateOnline
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

func humanDur(sec int64) string {
	d := (time.Duration(sec) * time.Second).Round(time.Minute)
	if d < time.Minute {
		return fmt.Sprintf("%ds", sec)
	}
	return d.String()
}

// Plan returns the alerts to raise/resolve for ev. A mass disconnect opens
// one fleet:connectivity incident; nodes that are part of it (or go down
// while it is open) are not paged individually, and the incident resolves
// once every member is back.
func (a *NodeAlerter) Plan(ev Evaluation, now int64, name func(id string) string) []AlertIntent {
	var out []AlertIntent
	if !a.massActive && len(ev.MassDown) > 0 {
		a.massActive = true
		for _, id := range ev.MassDown {
			if _, ok := a.alerted[id]; !ok {
				a.massMembers[id] = true
			}
		}
		out = append(out, AlertIntent{
			Key:      "fleet:connectivity",
			Title:    fmt.Sprintf("🔴 Fleet connectivity: %d nodes lost contact at once (check the master's network)", len(ev.MassDown)),
			Critical: true,
		})
	}
	for _, tr := range ev.Transitions {
		switch tr.To {
		case StateDown:
			if a.massActive {
				a.massMembers[tr.NodeID] = true
				continue
			}
			a.alerted[tr.NodeID] = tr.LastSeen
			out = append(out, AlertIntent{
				Key:      "fleet:node:" + tr.NodeID + ":down",
				Title:    fmt.Sprintf("🔴 %s is down: no contact for %s", name(tr.NodeID), humanDur(now-tr.LastSeen)),
				Critical: true,
			})
		case StateStale:
			// still lost; nothing to do
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
