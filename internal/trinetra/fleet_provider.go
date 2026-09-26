// Package trinetra: fleet_provider.go implements core.FleetProvider for
// the daemon. On solo and child it reports just this host ("self"); on a
// master it adds every enrolled node from the registry, with state from the
// liveness tracker and metrics from each node's latest live update.
package trinetra

import (
	"encoding/json"
	"fmt"
	"math"
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
	joinURL  string
	pin      string
	listen   string
	getCfg   func() *config.Config
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
// actor is "unknown" for every mutation whose FleetAPI signature has no
// actor parameter today (Rename/SetNodeTags/Revoke/Remove/DeleteToken) --
// TODO(plan C): thread a real actor (CLI user / web session) through those
// signatures; changing them now would break plan A's already-compiling web
// code, which this task must not touch.
func (m *masterState) audited(actor, action, target, detail string) {
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

func (f fleetAPIImpl) RenameNode(id, name string) error {
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
	m.audited("unknown", "rename_node", id, name)
	return nil
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
	if err := m.reg.Update(id, func(n *fleet.Node) error { n.Tags = tags; return nil }); err != nil {
		return err
	}
	m.audited("unknown", "set_node_tags", id, strings.Join(tags, ","))
	return nil
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
	if m.hub != nil {
		m.hub.Disconnect(id) // a revoked node keeps no lease, no open stream
	}
	m.audited("unknown", "revoke_node", id, "")
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
	if err := m.loop.remove(id, time.Now()); err != nil {
		return err
	}
	if m.hub != nil {
		m.hub.Disconnect(id)
	}
	m.audited("unknown", "remove_node", id, "")
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

func (f fleetAPIImpl) DeleteToken(id string) error {
	m, err := f.requireMaster()
	if err != nil {
		return err
	}
	if err := m.tokens.Delete(id); err != nil {
		return err
	}
	m.audited("unknown", "delete_token", id, "")
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
		return core.Incident{}, fmt.Errorf("no such incident %q", id)
	}
	inc, ok := m.engine.incidents.Get(id)
	if !ok {
		return core.Incident{}, fmt.Errorf("no such incident %q", id)
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
		return fmt.Errorf("no such incident %q", id)
	}
	inc, ok := m.engine.incidents.Get(id)
	if !ok {
		return fmt.Errorf("no such incident %q", id)
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
		return fmt.Errorf("no such silence %q", id)
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
		return fmt.Errorf("no such maintenance %q", id)
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
