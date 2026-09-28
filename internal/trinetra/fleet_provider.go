// Package trinetra: fleet_provider.go implements core.FleetProvider for
// the daemon. On solo and child it reports just this host ("self"); on a
// master it adds every enrolled node from the registry, with state from the
// liveness tracker and metrics from each node's latest live update.
package trinetra

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
	"github.com/InfoDiveLabs/trinetra/internal/version"
)

type masterState struct {
	reg     *fleet.Registry
	tokens  *fleet.TokenStore
	sink    *replicaSink
	tracker *fleet.Tracker
	loop    *masterLoop
	// hub fans lease/receipt/ack/rpc frames out to connected nodes; engine
	// is the alerting engine (fleet_engine.go), and audit the fleet audit
	// log (fleet_audit.go). All three are nil-safe to call through
	// fleetAPIImpl's helpers when absent (should not happen once startMaster
	// has run, but keeps older tests that build a bare masterState working).
	hub      *fleet.Hub
	engine   *fleetAlertEngine
	audit    *auditLog
	silences *silenceStore
	alerting *alertingStore
	// managed/managedPush (task 8) are the master's managed-config fragment
	// store and its push cadence, nil-safe like silences/alerting for a
	// bare-bones masterState built by an older test suite.
	managed     *managedFragmentStore
	managedPush *managedPusher
	joinURL     string
	pin         string
	listen      string
	getCfg      func() *config.Config
}

type fleetProvider struct {
	self      core.API
	role      string
	selfName  func() string
	master    *masterState
	link      *fleet.Shipper
	nodeID    string
	masterURL string
	// managed (task 8) is this CHILD's own managed-config state (nil for a
	// master/solo daemon): fleetAPIImpl.Status() reads it directly (this is
	// the live daemon process itself, not a separate CLI invocation) to
	// populate core.LinkView.Managed.
	managed *managedChild
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
			OutboxBytes: ls.Outbox.Bytes, Unacked: ls.Outbox.Unacked, OldestUnacked: ls.Outbox.OldestUnackedTS, Gaps: ls.Outbox.Gaps,
			Managed: p.managed.FragmentsSnapshot()}
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
		s := core.NodeSummary{ID: n.ID, Name: n.Name, Tags: n.Tags, DependsOn: n.DependsOn, Version: n.Version, LastSeen: n.LastSeen,
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
		s.DroppedOutOfOrder, s.DroppedDuplicate, s.DroppedCardinality = st.DroppedOutOfOrder, st.DroppedDuplicate, st.DroppedCardinality
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

// audit appends an entry to m's audit log (nil-safe: see auditLog.Append).
// actor is normalized to "unknown" when the caller passed "" (a request
// this daemon could not resolve any acting identity for at all -- should
// not happen for a live web/CLI caller, both of which always resolve to
// something (auditUser(r), "cli"), but keeps the audit log's Actor column
// never blank for an older wire client or a defensive future caller).
func (m *masterState) audited(actor, action, target, detail string) {
	if actor == "" {
		actor = "unknown"
	}
	_ = m.audit.Append(actor, action, target, detail, time.Now().Unix())
}

// nonRevokedNodeIDs returns every non-revoked node id in reg.
func nonRevokedNodeIDs(reg *fleet.Registry) []string {
	var ids []string
	for _, n := range reg.List() {
		if !n.Revoked {
			ids = append(ids, n.ID)
		}
	}
	return ids
}

// pushSilencesToAll immediately refreshes every connected node's pushed
// silence set -- called after any silence/maintenance mutation ("on
// change").
func (m *masterState) pushSilencesToAll(now time.Time) {
	if m.engine == nil {
		return
	}
	m.engine.PushSilencesToAll(now, nonRevokedNodeIDs(m.reg))
}

func (f fleetAPIImpl) RenameNode(id, name, actor string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return fmt.Errorf("node name must be 1-64 characters")
	}
	// Names must stay unique (case-insensitively) so a Matcher.Node glob has
	// a precise target (review round 2, item b): unlike a join, a rename is
	// a deliberate operator action, so a collision is refused rather than
	// silently suffixed. Registry.Rename checks and applies this atomically
	// under one lock (review round 3, item 2: a separate NameConflict-then-
	// Update here was a TOCTOU race two concurrent renames, or a rename
	// racing a join, could both slip through).
	if err := m.reg.Rename(id, name); err != nil {
		return err
	}
	m.audited(actor, "rename_node", id, name)
	return nil
}

func (f fleetAPIImpl) SetNodeTags(id string, tags []string, actor string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	for _, t := range tags {
		if !fleet.ValidTag(t) {
			return fmt.Errorf("invalid tag %q (lowercase letters, digits, _ . -; max 32)", t)
		}
	}
	if err := m.reg.Update(id, func(n *fleet.Node) error { n.Tags = tags; return nil }); err != nil {
		return err
	}
	m.audited(actor, "set_node_tags", id, strings.Join(tags, ","))
	return nil
}

// validDepEntry reports whether an entry in Node.DependsOn is well-formed:
// either a bare node id (validated against the registry by the caller) or
// "tag:<t>" naming a valid tag.
func validDepEntry(entry string) (tag string, isTag bool) {
	if t, ok := strings.CutPrefix(entry, "tag:"); ok {
		return t, true
	}
	return "", false
}

func (f fleetAPIImpl) SetNodeDeps(id string, deps []string, actor string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	if _, ok := m.reg.Get(id); !ok {
		return core.ErrNoSuchNode
	}
	seen := map[string]bool{}
	var clean []string
	for _, d := range deps {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		if tag, isTag := validDepEntry(d); isTag {
			if !fleet.ValidTag(tag) {
				return fmt.Errorf("invalid dependency %q: not a valid tag", d)
			}
		} else {
			if d == id {
				return fmt.Errorf("node %s cannot depend on itself", id)
			}
			if _, ok := m.reg.Get(d); !ok {
				return fmt.Errorf("dependency %q: %w", d, core.ErrNoSuchNode)
			}
		}
		seen[d] = true
		clean = append(clean, d)
	}
	if err := m.reg.Update(id, func(n *fleet.Node) error { n.DependsOn = clean; return nil }); err != nil {
		return err
	}
	m.audited(actor, "fleet.node.deps", id, strings.Join(clean, ","))
	// task 6 fix round 1, IMPORTANT 4: a dependency changing (in particular,
	// a down dependency being REMOVED) may free up a node that was folded
	// waiting on it -- releaseFoldedDependents only ever runs off that
	// dependency's own recover, which never happens here, so this must be
	// triggered explicitly.
	if m.engine != nil {
		m.engine.ReleaseIfDependenciesClear(id, time.Now().Unix())
	}
	return nil
}

func (f fleetAPIImpl) RevokeNode(id, actor string) error {
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
	if m.hub != nil {
		m.hub.Disconnect(id) // a revoked node keeps no lease, no open stream
	}
	m.audited(actor, "revoke_node", id, "")
	return nil
}

func (f fleetAPIImpl) RemoveNode(id, actor string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	if m.loop == nil {
		return core.ErrNotMaster
	}
	if err := m.loop.remove(id, time.Now()); err != nil {
		return err
	}
	if m.hub != nil {
		m.hub.Disconnect(id)
	}
	m.audited(actor, "remove_node", id, "")
	return nil
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
		return core.CreatedToken{}, fmt.Errorf("fleet.address is empty; run `trinetra fleet init --address ...`")
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
	actor := spec.Creator
	if actor == "" {
		actor = "unknown"
	}
	m.audited(actor, "create_token", tok.ID, strings.Join(spec.Tags, ","))
	return core.CreatedToken{Token: tokenView(tok), JoinCode: code}, nil
}

func (f fleetAPIImpl) DeleteToken(id, actor string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	if err := m.tokens.Delete(id); err != nil {
		return err
	}
	m.audited(actor, "delete_token", id, "")
	return nil
}

func (f fleetAPIImpl) Incidents(filter core.IncidentFilter) ([]core.Incident, error) {
	m, err := f.requireMaster()
	if err != nil {
		return nil, err
	}
	if m.engine == nil || m.engine.incidents == nil {
		return nil, nil
	}
	tagsOf := func(nodeID string) []string {
		if n, ok := m.reg.Get(nodeID); ok {
			return n.Tags
		}
		return nil
	}
	return m.engine.incidents.List(filter, tagsOf), nil
}

func (f fleetAPIImpl) Incident(id string) (core.Incident, error) {
	m, err := f.requireMaster()
	if err != nil {
		return core.Incident{}, err
	}
	if m.engine == nil || m.engine.incidents == nil {
		return core.Incident{}, fmt.Errorf("no such incident %q: %w", id, core.ErrNotFound)
	}
	inc, ok := m.engine.incidents.Get(id)
	if !ok {
		return core.Incident{}, fmt.Errorf("no such incident %q: %w", id, core.ErrNotFound)
	}
	return inc, nil
}

// AckIncident acknowledges incident id: it pushes an "ack" frame (applied
// via AlertState.Ack on the child, fleet_lease.go's applyAckFrame) for every
// still-open alert on every member node, then records the ack itself.
func (f fleetAPIImpl) AckIncident(id, actor string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	if m.engine == nil || m.engine.incidents == nil {
		return fmt.Errorf("no such incident %q: %w", id, core.ErrNotFound)
	}
	inc, ok := m.engine.incidents.Get(id)
	if !ok {
		return fmt.Errorf("no such incident %q: %w", id, core.ErrNotFound)
	}
	if actor == "" {
		actor = "unknown"
	}
	for _, al := range inc.Alerts {
		if al.Node == "" || al.ResolvedAt != 0 {
			continue
		}
		m.engine.PushAck(al.Node, al.Key, false)
	}
	if _, err := m.engine.incidents.Ack(id, actor, time.Now().Unix()); err != nil {
		return err
	}
	m.audited(actor, "ack_incident", id, "")
	return nil
}

// Explain returns the pipeline trail for an alert key or an incident id.
func (f fleetAPIImpl) Explain(key string) ([]core.IncidentEvent, error) {
	m, err := f.requireMaster()
	if err != nil {
		return nil, err
	}
	if m.engine == nil || m.engine.incidents == nil {
		return nil, fmt.Errorf("no incident or alert key %q found", key)
	}
	if inc, ok := m.engine.incidents.Get(key); ok {
		return inc.Timeline, nil
	}
	if inc, ok := m.engine.incidents.FindByAlertKey(key); ok {
		return inc.Timeline, nil
	}
	return nil, fmt.Errorf("no incident or alert key %q found", key)
}

func (f fleetAPIImpl) Audit(limit int) ([]core.AuditEntry, error) {
	m, err := f.requireMaster()
	if err != nil {
		return nil, err
	}
	return m.audit.Recent(limit)
}

func (f fleetAPIImpl) Silences() ([]core.Silence, error) {
	m, err := f.requireMaster()
	if err != nil {
		return nil, err
	}
	if m.silences == nil {
		return nil, nil
	}
	return m.silences.List(), nil
}

// CreateSilence validates and stores s, audits it under s.Author (the CLI
// always sets this to "cli"; a caller with none is recorded as "unknown"),
// and immediately pushes the updated silence set to every connected node.
func (f fleetAPIImpl) CreateSilence(s core.Silence) (core.Silence, error) {
	m, err := f.requireMaster()
	if err != nil {
		return core.Silence{}, err
	}
	if m.silences == nil {
		return core.Silence{}, fmt.Errorf("silences are not available")
	}
	created, err := m.silences.Create(s)
	if err != nil {
		return core.Silence{}, err
	}
	actor := created.Author
	if actor == "" {
		actor = "unknown"
	}
	m.audited(actor, "create_silence", created.ID, formatMatchers(created.Matchers))
	m.pushSilencesToAll(time.Now())
	return created, nil
}

func (f fleetAPIImpl) ExpireSilence(id, actor string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	if m.silences == nil {
		return fmt.Errorf("no such silence %q: %w", id, core.ErrNotFound)
	}
	if err := m.silences.Expire(id, time.Now().Unix()); err != nil {
		return err
	}
	m.audited(actor, "expire_silence", id, "")
	m.pushSilencesToAll(time.Now())
	return nil
}

func (f fleetAPIImpl) Maintenances() ([]core.Maintenance, error) {
	m, err := f.requireMaster()
	if err != nil {
		return nil, err
	}
	if m.silences == nil {
		return nil, nil
	}
	return m.silences.Maintenances(), nil
}

// SaveMaintenance validates and stores mw (new if mw.ID is "", else an
// update to the existing window), audits it under mw.Author, and
// immediately pushes the updated silence set to every connected node.
func (f fleetAPIImpl) SaveMaintenance(mw core.Maintenance) (core.Maintenance, error) {
	m, err := f.requireMaster()
	if err != nil {
		return core.Maintenance{}, err
	}
	if m.silences == nil {
		return core.Maintenance{}, fmt.Errorf("silences are not available")
	}
	saved, err := m.silences.SaveMaintenance(mw)
	if err != nil {
		return core.Maintenance{}, err
	}
	actor := saved.Author
	if actor == "" {
		actor = "unknown"
	}
	m.audited(actor, "save_maintenance", saved.ID, saved.Name)
	m.pushSilencesToAll(time.Now())
	return saved, nil
}

func (f fleetAPIImpl) DeleteMaintenance(id, actor string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	if m.silences == nil {
		return fmt.Errorf("no such maintenance %q: %w", id, core.ErrNotFound)
	}
	if err := m.silences.DeleteMaintenance(id); err != nil {
		return err
	}
	m.audited(actor, "delete_maintenance", id, "")
	m.pushSilencesToAll(time.Now())
	return nil
}

// Alerting returns the current routing/escalation config (task 5):
// defaultAlertingConfig if nothing has ever been saved (m.alerting == nil
// covers a masterState built by an older test suite that never wired one).
func (f fleetAPIImpl) Alerting() (core.AlertingConfig, error) {
	m, err := f.requireMaster()
	if err != nil {
		return core.AlertingConfig{}, err
	}
	if m.alerting == nil {
		return defaultAlertingConfig(), nil
	}
	return m.alerting.Get(), nil
}

// validChannelName reports whether name is a configured channel (used by
// SetAlerting's validation): "*" itself is checked separately by
// validateAlertingConfig, never passed here.
func (m *masterState) validChannelName(name string) bool {
	if m.getCfg == nil {
		return false
	}
	c := m.getCfg()
	if c == nil {
		return false
	}
	for _, cc := range c.Channels {
		if cc.Name == name {
			return true
		}
	}
	return false
}

// SetAlerting validates and atomically replaces the routing/escalation
// config, auditing the change under actor.
func (f fleetAPIImpl) SetAlerting(cfg core.AlertingConfig, actor string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	if m.alerting == nil {
		return fmt.Errorf("alerting config is not available")
	}
	saved, err := m.alerting.Set(cfg, m.validChannelName)
	if err != nil {
		return err
	}
	if actor == "" {
		actor = "unknown"
	}
	m.audited(actor, "fleet.alerting.set", "", fmt.Sprintf("%d routes, %d policies", len(saved.Routes), len(saved.Policies)))
	return nil
}

// RouteTest dry-runs alert through resolveRoute -- the exact same function
// the alerting engine's real delivery uses (fleet_engine.go's
// deliverAndReceiptDetail/tryEscalate) -- plus a current-silence check,
// without firing anything. alert.Node is resolved against the registry (by
// id or display name) when it names a known node, so Matcher.Node's
// exact-id-or-name-glob semantics apply exactly as they would for a real
// alert from that node; an unrecognized Node is tried as a display name only
// (a dry run against a node that doesn't exist yet, or a typo, is still a
// useful "what would this match" answer, not an error).
func (f fleetAPIImpl) RouteTest(alert core.TestAlert) (core.RouteDecision, error) {
	m, err := f.requireMaster()
	if err != nil {
		return core.RouteDecision{}, err
	}
	cfg := defaultAlertingConfig()
	if m.alerting != nil {
		cfg = m.alerting.Get()
	}
	id, name, tags := alert.Node, alert.Node, alert.Tags
	if m.reg != nil {
		for _, n := range m.reg.List() {
			if n.ID == alert.Node || n.Name == alert.Node {
				id, name = n.ID, n.Name
				if len(alert.Tags) == 0 {
					tags = n.Tags
				}
				break
			}
		}
	}
	res := resolveRoute(cfg, id, name, tags, alert.Rule, alert.Severity)
	suppressed := ""
	if m.silences != nil {
		if info := m.silences.Suppressed(time.Now().Unix(), id, name, tags, alert.Rule, alert.Severity); info != nil {
			suppressed = info.Reason
		}
	}
	return core.RouteDecision{Route: res.Route, Policies: res.Policies, Suppressed: suppressed}, nil
}

// RuleStates returns every aggregate rule's current value/firing state
// (task 7); nil (never an error) when the engine has no rule evaluator
// wired (m.engine == nil never happens once startMaster has run, but keeps
// a bare-bones masterState test working, exactly RuleStates' sibling
// Alerting/Incidents accessors' own nil-safety pattern).
func (f fleetAPIImpl) RuleStates() ([]core.RuleState, error) {
	m, err := f.requireMaster()
	if err != nil {
		return nil, err
	}
	if m.engine == nil {
		return nil, nil
	}
	return m.engine.RuleStates(), nil
}

// Managed lists every managed-config fragment (task 8).
func (f fleetAPIImpl) Managed() ([]core.ManagedFragment, error) {
	m, err := f.requireMaster()
	if err != nil {
		return nil, err
	}
	if m.managed == nil {
		return nil, nil
	}
	return m.managed.List(), nil
}

// SaveManaged validates and stores frag, audits it under actor, and
// immediately pushes the updated desired set to every non-revoked,
// connected node.
func (f fleetAPIImpl) SaveManaged(frag core.ManagedFragment, actor string) (core.ManagedFragment, error) {
	m, err := f.requireMaster()
	if err != nil {
		return core.ManagedFragment{}, err
	}
	if m.managed == nil {
		return core.ManagedFragment{}, fmt.Errorf("managed config is not available")
	}
	saved, err := m.managed.Save(frag, actor)
	if err != nil {
		return core.ManagedFragment{}, err
	}
	a := actor
	if a == "" {
		a = "unknown"
	}
	m.audited(a, "fleet.managed.save", saved.ID, saved.Tag)
	m.managedPush.PushToAll(nonRevokedNodeIDs(m.reg))
	return saved, nil
}

// DeleteManaged removes fragment id, audits it under actor, and immediately
// pushes the updated desired set to every non-revoked, connected node.
func (f fleetAPIImpl) DeleteManaged(id, actor string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	if m.managed == nil {
		return fmt.Errorf("no such managed-config fragment %q: %w", id, core.ErrNotFound)
	}
	if err := m.managed.Delete(id); err != nil {
		return err
	}
	a := actor
	if a == "" {
		a = "unknown"
	}
	m.audited(a, "fleet.managed.delete", id, "")
	m.managedPush.PushToAll(nonRevokedNodeIDs(m.reg))
	return nil
}

// ManagedStatus reports every node with at least one applicable fragment (or
// an unresolved conflict): its desired-set generation, what it last
// reported applying (from its most recent LiveUpdate.Managed), and any
// drift/conflicts. A node that has never reported anything managed yet
// (never connected, or connected before ever being targeted) shows every
// currently-desired key as drift, since nothing is confirmed applied.
func (f fleetAPIImpl) ManagedStatus() ([]core.ManagedStatus, error) {
	m, err := f.requireMaster()
	if err != nil {
		return nil, err
	}
	if m.managed == nil {
		return nil, nil
	}
	var out []core.ManagedStatus
	for _, n := range m.reg.List() {
		desired, _, conflicts := m.managed.Desired(n.Tags)
		if len(desired) == 0 && len(conflicts) == 0 {
			continue
		}
		st := core.ManagedStatus{Node: n.ID, Desired: m.managed.Version(), Conflicts: conflicts}
		var reported *fleet.ManagedReport
		if u := m.sink.LiveOf(n.ID); u != nil {
			reported = u.Managed
		}
		if reported != nil {
			st.Version, st.Applied, st.Error = reported.Version, reported.Applied, reported.Error
			for k, dv := range desired {
				if cv, ok := reported.Values[k]; !ok || cv != dv {
					st.Drift = append(st.Drift, k)
				}
			}
		} else {
			for k := range desired {
				st.Drift = append(st.Drift, k)
			}
		}
		sort.Strings(st.Drift)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out, nil
}

// fleetSeriesCap is FleetSeries' agg="none" node-count ceiling (plan C, task
// 1b: "capped at 10 nodes"); it also bounds the web compare page's checkbox
// selection.
const fleetSeriesCap = 10

// fleetSeriesRawWindowCapSeconds bounds how wide a [from, to] window
// FleetSeries will serve at raw resolution (task-1b review round 1, minors:
// "cap the raw window at 24h with an error") -- a raw query over a much
// wider span would mean reading (and returning) a huge number of points for
// no real benefit over 1m; a caller wanting a wider view should ask for
// core.Res1m instead.
const fleetSeriesRawWindowCapSeconds int64 = 24 * 3600

// toSeriesResolution maps a core.Resolution onto this package's own
// Resolution (mirroring coreapi_inproc.go's Series conversion): core.ResRaw
// is raw, everything else (core.Res1m, and core.ResAuto -- FleetSeries has
// no age-dependent picker of its own, unlike Series/PickResolution, since
// the web compare page already decides raw-vs-1m itself off the requested
// range) is Res1m.
func toSeriesResolution(res core.Resolution) Resolution {
	if res == core.ResRaw {
		return ResRaw
	}
	return Res1m
}

// diskSeriesPoints returns, for every timestamp bucket present in ANY of
// store's disk:<mount> series over [from, to] at res, that bucket's WORST
// (highest) mount value -- the bucketed counterpart of fleet_rules.go's
// diskSeriesAverage (a single window-wide worst-mount average), used by
// FleetSeries' "disk" metric so a compare chart sees the same "disk (worst)"
// framing NodeSummary.WorstDiskPct/the aggregate rules already use, just
// across a whole series instead of one instant or one window average. A
// mount missing a point at a given bucket simply doesn't contribute to that
// bucket's max, exactly like diskSeriesAverage excluding a mount with no
// points from its own average.
func diskSeriesPoints(store SampleStore, from, to int64, res Resolution) ([]Point, error) {
	metrics, ok := diskMountMetrics(store, res)
	if !ok || len(metrics) == 0 {
		return nil, nil
	}
	worst := map[int64]float64{}
	seen := map[int64]bool{}
	for _, m := range metrics {
		pts, err := store.Query(m, from, to, res)
		if err != nil {
			continue
		}
		for _, p := range pts {
			if !seen[p.TS] || p.Avg > worst[p.TS] {
				worst[p.TS], seen[p.TS] = p.Avg, true
			}
		}
	}
	out := make([]Point, 0, len(worst))
	for ts, v := range worst {
		out = append(out, Point{TS: ts, Min: v, Avg: v, Max: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out, nil
}

// querySeriesPoints reads metric's points from store over [from, to] at res,
// special-casing "disk" to diskSeriesPoints' worst-mount-per-bucket series
// (see its doc); a nil store (no data source at all for this node -- e.g.
// self before the aggregate-rule evaluator has ever been wired) degrades to
// no points rather than panicking.
func querySeriesPoints(store SampleStore, metric string, from, to int64, res Resolution) ([]Point, error) {
	if store == nil {
		return nil, nil
	}
	if metric == "disk" {
		return diskSeriesPoints(store, from, to, res)
	}
	return store.Query(metric, from, to, res)
}

// fleetSeriesBucketSeconds picks the fixed bucket width FleetSeries'
// avg/max/min aggregation groups every source's points into (task-1b review
// round 1): 60s at 1m resolution (matching the underlying rollup's own
// granularity exactly), or at raw resolution the master's own configured
// raw ("fast tier") sample interval when known (getCfg non-nil and
// Config.FastInterval > 0 -- the cadence cpu/mem/... are actually collected
// at, config.go's FastInterval), otherwise a 10s default. Exactly ONE bucket
// width is used for the whole request, computed once (never re-derived per
// node): every node's points must land on the SAME shared grid for the
// across-node aggregation step to combine them meaningfully.
func fleetSeriesBucketSeconds(res Resolution, getCfg func() *config.Config) int64 {
	if res == Res1m {
		return 60
	}
	if getCfg != nil {
		if cfg := getCfg(); cfg != nil && cfg.FastInterval > 0 {
			return int64(cfg.FastInterval)
		}
	}
	return 10
}

// floorToBucket floors ts down to the start of its bucketSeconds-wide
// bucket; bucketSeconds<=0 (shouldn't happen -- fleetSeriesBucketSeconds
// always returns a positive value) degrades to "no bucketing" rather than a
// divide-by-zero.
func floorToBucket(ts, bucketSeconds int64) int64 {
	if bucketSeconds <= 0 {
		return ts
	}
	return (ts / bucketSeconds) * bucketSeconds
}

// bucketNodeSeries floors every point in pts into its bucketSeconds-wide
// bucket and, for a bucket more than one point lands in, keeps only the
// LAST one by actual (unfloored) TS (task-1b review round 1: "Per node,
// take the last value in the bucket") -- e.g. raw resolution can pack
// several samples into one bucket when bucketSeconds is coarser than the
// data's real cadence. Returns one value per bucket this node actually has
// data in.
func bucketNodeSeries(pts []Point, bucketSeconds int64) map[int64]float64 {
	lastTS := map[int64]int64{}
	out := map[int64]float64{}
	for _, p := range pts {
		b := floorToBucket(p.TS, bucketSeconds)
		if prev, ok := lastTS[b]; !ok || p.TS >= prev {
			lastTS[b], out[b] = p.TS, p.Avg
		}
	}
	return out
}

// fleetSeriesAggregate combines vals (one value per contributing node at a
// shared timestamp bucket) per agg; avg is the default for any value other
// than max/min (including AggNone, which never reaches here -- see
// FleetSeries).
func fleetSeriesAggregate(agg core.Agg, vals []float64) float64 {
	switch agg {
	case core.AggMax:
		m := vals[0]
		for _, v := range vals[1:] {
			if v > m {
				m = v
			}
		}
		return m
	case core.AggMin:
		m := vals[0]
		for _, v := range vals[1:] {
			if v < m {
				m = v
			}
		}
		return m
	default: // core.AggAvg
		sum := 0.0
		for _, v := range vals {
			sum += v
		}
		return sum / float64(len(vals))
	}
}

// FleetSeries implements core.FleetAPI (plan C, task 1b): metric's time
// series across every node matching filter, either one series per node
// (agg="none", capped at fleetSeriesCap nodes) or one aggregated series
// (agg avg/max/min, Node ""). The master's own node is included the same
// way B7's aggregate rules include it (fleet_rules.go's ruleSelfSource):
// this reuses the exact self Name/Store the master's own rule evaluator was
// wired with (m.engine.rules.self) rather than plumbing a second reference
// onto masterState, so a nil/never-wired evaluator (bare-bones test
// masterState) just means self contributes no data, not a panic.
func (f fleetAPIImpl) FleetSeries(metric string, filter core.NodeFilter, agg core.Agg, from, to int64, res core.Resolution) ([]core.FleetSeriesPoint, error) {
	m, err := f.requireMaster()
	if err != nil {
		return nil, err
	}
	switch agg {
	case core.AggNone, core.AggAvg, core.AggMax, core.AggMin, "":
	default:
		return nil, fmt.Errorf("unknown aggregation")
	}
	if to <= from {
		return nil, fmt.Errorf("to must be after from")
	}
	storeRes := toSeriesResolution(res)
	if storeRes == ResRaw && to-from > fleetSeriesRawWindowCapSeconds {
		return nil, fmt.Errorf("raw resolution is limited to a 24h window")
	}

	type nodeSource struct {
		name  string
		store SampleStore
	}
	var sources []nodeSource

	selfName := f.p.selfName()
	if filter.Match(core.NodeSummary{ID: core.SelfNodeID, Name: selfName, Self: true, State: string(fleet.StateOnline)}) {
		var store SampleStore
		if m.engine != nil && m.engine.rules != nil {
			store = m.engine.rules.self.Store
		}
		sources = append(sources, nodeSource{name: selfName, store: store})
	}
	for _, n := range m.reg.List() {
		if n.Revoked {
			continue
		}
		s := core.NodeSummary{ID: n.ID, Name: n.Name, Tags: n.Tags, State: string(m.tracker.State(n.ID))}
		if s.State == "" {
			s.State = "unknown"
		}
		if !filter.Match(s) {
			continue
		}
		rn, err := m.sink.node(n.ID)
		if err != nil {
			continue
		}
		sources = append(sources, nodeSource{name: n.Name, store: rn.store})
	}

	if agg == core.AggNone || agg == "" {
		if len(sources) > fleetSeriesCap {
			return nil, fmt.Errorf("compare at most %d nodes", fleetSeriesCap)
		}
		var out []core.FleetSeriesPoint
		for _, s := range sources {
			pts, err := querySeriesPoints(s.store, metric, from, to, storeRes)
			if err != nil {
				continue
			}
			for _, p := range pts {
				out = append(out, core.FleetSeriesPoint{Node: s.name, TS: p.TS, Value: p.Avg})
			}
		}
		return out, nil
	}

	// avg/max/min (task-1b review round 1, item 3): every source's points
	// are floored onto ONE shared bucket grid (fleetSeriesBucketSeconds) --
	// 60s at 1m resolution, the master's configured raw sample interval (or
	// 10s) at raw resolution -- taking each node's LAST value within a
	// bucket (bucketNodeSeries), before combining across nodes with agg.
	bucketSeconds := fleetSeriesBucketSeconds(storeRes, m.getCfg)
	buckets := map[int64][]float64{}
	for _, s := range sources {
		pts, err := querySeriesPoints(s.store, metric, from, to, storeRes)
		if err != nil {
			continue
		}
		for b, v := range bucketNodeSeries(pts, bucketSeconds) {
			buckets[b] = append(buckets[b], v)
		}
	}
	tsList := make([]int64, 0, len(buckets))
	for ts := range buckets {
		tsList = append(tsList, ts)
	}
	sort.Slice(tsList, func(i, j int) bool { return tsList[i] < tsList[j] })
	out := make([]core.FleetSeriesPoint, 0, len(tsList))
	for _, ts := range tsList {
		out = append(out, core.FleetSeriesPoint{TS: ts, Value: fleetSeriesAggregate(agg, buckets[ts])})
	}
	return out, nil
}
