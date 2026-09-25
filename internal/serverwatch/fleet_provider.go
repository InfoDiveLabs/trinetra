// Package serverwatch: fleet_provider.go implements core.FleetProvider for
// the daemon. On solo and child it reports just this host ("self"); on a
// master it adds every enrolled node from the registry, with state from the
// liveness tracker and metrics from each node's latest live update.
package serverwatch

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
	"serverwatch/internal/fleet"
	"serverwatch/internal/version"
)

type masterState struct {
	reg     *fleet.Registry
	tokens  *fleet.TokenStore
	sink    *replicaSink
	tracker *fleet.Tracker
	loop    *masterLoop
	joinURL string
	pin     string
	listen  string
	getCfg  func() *config.Config
}

type fleetProvider struct {
	self      core.API
	role      string
	selfName  func() string
	master    *masterState
	link      *fleet.Shipper
	nodeID    string
	masterURL string
}

// fleetAwareAPI is the daemon's core.API plus the optional FleetProvider.
type fleetAwareAPI struct {
	core.API
	*fleetProvider
}

func (p *fleetProvider) Fleet() core.FleetAPI { return fleetAPIImpl{p} }

func (p *fleetProvider) Node(id string) (core.API, error) {
	if id == "" || id == core.SelfNodeID {
		return p.self, nil
	}
	if p.master == nil {
		return nil, core.ErrNoSuchNode
	}
	if _, ok := p.master.reg.Get(id); !ok {
		return nil, core.ErrNoSuchNode
	}
	return p.master.sink.NodeAPI(id, p.master.getCfg)
}

type fleetAPIImpl struct{ p *fleetProvider }

func (f fleetAPIImpl) Status() (core.FleetStatus, error) {
	p := f.p
	st := core.FleetStatus{Role: p.role, Nodes: 1, NodeID: p.nodeID, MasterURL: p.masterURL}
	if p.master != nil {
		st.Nodes += len(p.master.reg.List())
		st.JoinURL, st.CAPin, st.Listen = p.master.joinURL, p.master.pin, p.master.listen
	}
	if p.link != nil {
		ls := p.link.Status()
		st.Link = &core.LinkView{State: ls.State, LastAck: ls.LastAck, LastError: ls.LastError,
			OutboxBytes: ls.Outbox.Bytes, Unacked: ls.Outbox.Unacked, OldestUnacked: ls.Outbox.OldestUnackedTS, Gaps: ls.Outbox.Gaps}
	}
	return st, nil
}

func worstDisk(disks map[string]float64) float64 {
	w := 0.0
	for _, v := range disks {
		if v > w {
			w = v
		}
	}
	return w
}

func (f fleetAPIImpl) Nodes(filter core.NodeFilter) ([]core.NodeSummary, error) {
	p := f.p
	var out []core.NodeSummary
	selfView, _ := p.self.Snapshot()
	self := core.NodeSummary{ID: core.SelfNodeID, Name: p.selfName(), Self: true, State: string(fleet.StateOnline),
		Version: version.String(), LastSeen: time.Now().Unix(), CPU: selfView.CPU, MemPct: selfView.MemPct, Load1: selfView.Load1}
	for _, d := range selfView.Disks {
		if d.UsagePct > self.WorstDiskPct {
			self.WorstDiskPct = d.UsagePct
		}
	}
	if filter.Match(self) {
		out = append(out, self)
	}
	if p.master == nil {
		return out, nil
	}
	for _, n := range p.master.reg.List() {
		s := core.NodeSummary{ID: n.ID, Name: n.Name, Tags: n.Tags, Version: n.Version, LastSeen: n.LastSeen,
			RemoteAddr: n.RemoteAddr, Revoked: n.Revoked, State: string(p.master.tracker.State(n.ID))}
		if s.State == "" {
			s.State = "unknown"
		}
		if u := p.master.sink.LiveOf(n.ID); u != nil {
			var snap Snapshot
			if json.Unmarshal(u.Snapshot, &snap) == nil {
				s.CPU, s.MemPct, s.Load1, s.WorstDiskPct = snap.CPU, snap.MemPct, snap.Load1, worstDisk(snap.Disks)
			}
			s.OutboxBytes, s.OutboxOldest, s.OutboxGaps = u.Outbox.Bytes, u.Outbox.OldestUnackedTS, u.Outbox.Gaps
		}
		st := p.master.sink.Stats(n.ID)
		s.SkewSec = int64(math.Round(st.SkewSec))
		s.DroppedOld, s.DroppedCardinality = st.DroppedOld, st.DroppedCardinality
		if filter.Match(s) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f fleetAPIImpl) requireMaster() (*masterState, error) {
	if f.p.master == nil {
		return nil, core.ErrNotMaster
	}
	return f.p.master, nil
}

func (f fleetAPIImpl) RenameNode(id, name string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return fmt.Errorf("node name must be 1-64 characters")
	}
	return m.reg.Update(id, func(n *fleet.Node) error { n.Name = name; return nil })
}

func (f fleetAPIImpl) SetNodeTags(id string, tags []string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	for _, t := range tags {
		if !fleet.ValidTag(t) {
			return fmt.Errorf("invalid tag %q (lowercase letters, digits, _ . -; max 32)", t)
		}
	}
	return m.reg.Update(id, func(n *fleet.Node) error { n.Tags = tags; return nil })
}

func (f fleetAPIImpl) RevokeNode(id string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	if err := m.reg.Update(id, func(n *fleet.Node) error { n.Revoked = true; return nil }); err != nil {
		return err
	}
	if m.tracker.State(id) == "" {
		// Never tracked (no contact since master start): register it so the
		// revocation sticks instead of being a no-op.
		m.tracker.Seen(id, time.Now().Unix(), -1)
	}
	m.tracker.SetRevoked(id, true)
	return nil
}

func (f fleetAPIImpl) RemoveNode(id string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	if m.loop == nil {
		return core.ErrNotMaster
	}
	return m.loop.remove(id, time.Now())
}

func tokenView(t fleet.Token) core.TokenView {
	return core.TokenView{ID: t.ID, Expires: t.Expires, Uses: t.Uses, Tags: t.Tags, Created: t.Created, Creator: t.Creator}
}

func (f fleetAPIImpl) Tokens() ([]core.TokenView, error) {
	m, err := f.requireMaster()
	if err != nil {
		return nil, err
	}
	var out []core.TokenView
	for _, t := range m.tokens.List(time.Now()) {
		out = append(out, tokenView(t))
	}
	return out, nil
}

func (f fleetAPIImpl) CreateToken(spec core.TokenSpec) (core.CreatedToken, error) {
	m, err := f.requireMaster()
	if err != nil {
		return core.CreatedToken{}, err
	}
	if m.joinURL == "" {
		return core.CreatedToken{}, fmt.Errorf("fleet.address is empty; run `serverwatch fleet init --address ...`")
	}
	ttl := time.Duration(spec.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	uses := spec.Uses
	if uses <= 0 {
		uses = 1
	}
	plain, tok, err := m.tokens.Create(ttl, uses, spec.Tags, spec.Creator, time.Now())
	if err != nil {
		return core.CreatedToken{}, err
	}
	code := fleet.EncodeJoin(fleet.JoinInfo{URL: m.joinURL, Token: plain, Pin: m.pin})
	return core.CreatedToken{Token: tokenView(tok), JoinCode: code}, nil
}

func (f fleetAPIImpl) DeleteToken(id string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	return m.tokens.Delete(id)
}
