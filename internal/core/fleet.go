package core

import (
	"errors"
	"path"
	"slices"
	"strings"
)

// SelfNodeID is the node id a caller uses to mean "this daemon", both in
// core.FleetProvider.Node and in the control-socket request.Node field. A
// solo daemon (no fleet role) still recognizes it: control.Client.ForNode's
// self-view routes through unchanged, since resolveNode (internal/control/
// server.go) treats "" and SelfNodeID identically.
const SelfNodeID = "self"

// ErrNotMaster is returned by a FleetAPI implementation for a call that only
// a fleet master can serve (a child or solo daemon has no fleet to report
// on).
var ErrNotMaster = errors.New("this trinetra is not a fleet master")

// ErrNoSuchNode is returned by FleetProvider.Node (and surfaces from
// FleetAPI.RenameNode/SetNodeTags/RevokeNode/RemoveNode) when id does not name a known
// fleet node.
var ErrNoSuchNode = errors.New("no such fleet node")

// NodeSummary is a fleet master's projection of one node (itself included,
// with Self true) for the fleet nodes list.
type NodeSummary struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Tags         []string `json:"tags,omitempty"`
	DependsOn    []string `json:"depends_on,omitempty"`
	Self         bool     `json:"self,omitempty"`
	State        string   `json:"state"`
	Version      string   `json:"version,omitempty"`
	LastSeen     int64    `json:"last_seen,omitempty"`
	RemoteAddr   string   `json:"remote_addr,omitempty"`
	Revoked      bool     `json:"revoked,omitempty"`
	CPU          float64  `json:"cpu"`
	MemPct       float64  `json:"mem_pct"`
	WorstDiskPct float64  `json:"worst_disk_pct"`
	Load1        float64  `json:"load1"`
	OutboxBytes  int64    `json:"outbox_bytes,omitempty"`
	OutboxOldest int64    `json:"outbox_oldest,omitempty"`
	OutboxGaps   int      `json:"outbox_gaps,omitempty"`
	// SkewSec is the master's filtered estimate of server_time - sent_at for
	// this node: negative means the node's clock is ahead of the master's.
	SkewSec int64 `json:"skew_sec,omitempty"`
	// DroppedOutOfOrder / DroppedCardinality count points the master's
	// replica refused because they were older than the series' last stored
	// point (a clock that jumped back, data re-sent after a divergence) or
	// over the per-node series limit. DroppedDuplicate counts harmless
	// re-sent copies of points it already had (refills, retried batches).
	DroppedOutOfOrder  int64 `json:"dropped_out_of_order,omitempty"`
	DroppedDuplicate   int64 `json:"dropped_duplicate,omitempty"`
	DroppedCardinality int64 `json:"dropped_cardinality,omitempty"`
}

// NodeFilter narrows a FleetAPI.Nodes call to nodes matching every non-empty
// criterion.
type NodeFilter struct {
	Tag   string `json:"tag"`
	State string `json:"state"`
	Query string `json:"query"`
}

// Match reports whether n passes every non-empty criterion of f. Query is a
// case-insensitive substring match over name, id and remote address.
func (f NodeFilter) Match(n NodeSummary) bool {
	if f.State != "" && n.State != f.State {
		return false
	}
	if f.Tag != "" && !slices.Contains(n.Tags, f.Tag) {
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Query)); q != "" {
		hay := strings.ToLower(n.Name + " " + n.ID + " " + n.RemoteAddr)
		if !strings.Contains(hay, q) {
			return false
		}
	}
	return true
}

// LinkView is a child's view of its own link to the fleet master: the
// shipper's connection state, the last successful ack, and outbox backlog.
type LinkView struct {
	// State is connecting, linked, catching up (live updates reach the
	// master but unsent data is being retried), retrying or revoked.
	State         string `json:"state"`
	LastAck       int64  `json:"last_ack"`
	LastError     string `json:"last_error"`
	OutboxBytes   int64  `json:"outbox_bytes"`
	Unacked       uint64 `json:"unacked"`
	OldestUnacked int64  `json:"oldest_unacked"`
	Gaps          int    `json:"gaps"`
	// Managed is this child's own managed-config keys, from the master's
	// last received managed_config push: config key -> the id of the
	// fragment currently supplying it. Populated only on a child (nil on a
	// master/solo daemon's own Status, which has no Link at all). Used by
	// this same daemon's `trinetra config set`/`unset` and by the web
	// config page (internal/web/handlers_config.go) to show these keys
	// read-only and reject an edit naming one -- never grown onto core.API
	// itself (task 8 ruling): this is the one seam that already exists for
	// exactly this purpose.
	Managed map[string]string `json:"managed,omitempty"`
}

// FleetStatus is the fleet-wide status projection FleetAPI.Status returns:
// this daemon's own role and, depending on that role, master- or
// child-specific fields.
type FleetStatus struct {
	Role      string    `json:"role"`
	NodeID    string    `json:"node_id,omitempty"`
	MasterURL string    `json:"master_url,omitempty"`
	Listen    string    `json:"listen,omitempty"`
	JoinURL   string    `json:"join_url,omitempty"`
	CAPin     string    `json:"ca_pin,omitempty"`
	Nodes     int       `json:"nodes"`
	Link      *LinkView `json:"link,omitempty"`
}

// TokenSpec describes a join token to create via FleetAPI.CreateToken.
type TokenSpec struct {
	TTLSeconds int64    `json:"ttl_seconds"`
	Uses       int      `json:"uses"`
	Tags       []string `json:"tags,omitempty"`
	Creator    string   `json:"creator,omitempty"`
}

// TokenView is a join token as listed by FleetAPI.Tokens: no secret
// material, just enough to audit or revoke it.
type TokenView struct {
	ID      string   `json:"id"`
	Expires int64    `json:"expires"`
	Uses    int      `json:"uses"`
	Tags    []string `json:"tags,omitempty"`
	Created int64    `json:"created"`
	Creator string   `json:"creator,omitempty"`
}

// CreatedToken is FleetAPI.CreateToken's result: the token's own view plus
// the one-time join code (prefixed swj1_) a child uses to enroll.
type CreatedToken struct {
	Token    TokenView `json:"token"`
	JoinCode string    `json:"join_code"`
}

// Incident is one (node, alert key) fire→recover episode as the master's
// alerting engine tracks it: opened on the first "fire", updated as more
// alerts join it (grouping arrives in a later task; for now one incident
// covers exactly one (node, key) pair), and resolved on "recover".
type Incident struct {
	ID       string          `json:"id"`
	GroupKey string          `json:"group_key"`
	Title    string          `json:"title"`
	Severity string          `json:"severity"`
	State    string          `json:"state"` // firing|acked|resolved|suppressed
	Nodes    []string        `json:"nodes"`
	Alerts   []IncidentAlert `json:"alerts"`
	Opened   int64           `json:"opened"`
	Updated  int64           `json:"updated"`
	Resolved int64           `json:"resolved,omitempty"`
	AckedBy  string          `json:"acked_by,omitempty"`
	Timeline []IncidentEvent `json:"timeline"`
}

// IncidentAlert is one alert instance folded into an Incident.
type IncidentAlert struct {
	Node             string `json:"node"`
	NodeName         string `json:"node_name,omitempty"`
	Key              string `json:"key"`
	Title            string `json:"title"`
	Severity         string `json:"severity"`
	FiredAt          int64  `json:"fired_at"`
	ResolvedAt       int64  `json:"resolved_at,omitempty"`
	DeliveredLocally bool   `json:"delivered_locally,omitempty"`
	// Suppressed, when non-empty, means this member alert is folded into the
	// incident but was never (and, while this stays set, will never be)
	// delivered on its own -- currently only a node-dependency fold (task 6
	// part 3): "suppressed: parent <name> down". Cleared (and the member
	// delivered as its own incident) when the dependency releases -- see
	// fleetAlertEngine's dependency handling.
	Suppressed string `json:"suppressed,omitempty"`
	// SilencedBy, when non-empty, is the silence/maintenance-window reason
	// this specific member's own fire matched (task 6 fix round 1, CRITICAL
	// 1): kept per member, NOT as incident-wide state, so one silenced
	// member can never starve an unsilenced sibling's delivery/escalation,
	// and an unsilenced sibling can never mask a silenced member from ever
	// being delivered once its silence ends. Cleared, and that member
	// delivered alone, once no silence matches it any more -- see
	// fleetAlertEngine.tryDeliverUnsilenced. Kept separate from Suppressed
	// (a dependency fold): the two are independent reasons a member can be
	// held back, and a member is never both at once in practice.
	SilencedBy string `json:"silenced_by,omitempty"`
}

// IncidentEvent is one entry in an Incident's pipeline trail (fleet explain
// prints these): fired|grouped|suppressed|delivered|escalated|acked|resolved|receipt.
//
// Leg/AlertKey/Node/FiredAt/Policy/Step/Channels (fleet phase 2 task 6,
// "structured timeline events") are the machine-readable form of what Detail
// otherwise only records as free text: which member alert (Node, AlertKey,
// FiredAt) and which leg ("fire" or "recover") this event covers, and, for a
// routed delivery/escalation/repeat, which policy/step/channels produced it.
// Every event this codebase writes from here on sets them; Detail is kept
// too, for display. An event with neither Leg nor Policy set is a LEGACY
// event recorded before this existed -- every reader of these fields must
// fall back to parsing Detail only for such an event, never for one that has
// them (see e.g. legDeliveredStatusFor, stepEventInfo in the trinetra
// package).
type IncidentEvent struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
	Actor  string `json:"actor,omitempty"`

	// Leg is "fire" or "recover": which half of an alert's lifecycle this
	// event covers. Empty for an event that isn't leg-specific (acked,
	// grouped) or a legacy event recorded before this field existed.
	Leg string `json:"leg,omitempty"`
	// AlertKey/Node/FiredAt identify the specific IncidentAlert member this
	// event is about, exactly like alertDedupKey: Node is "" for a
	// master-own alert.
	AlertKey string `json:"alert_key,omitempty"`
	Node     string `json:"node,omitempty"`
	FiredAt  int64  `json:"fired_at,omitempty"`
	// Policy/Step/Channels identify a routed delivery/escalation/repeat: which
	// policy and step index produced it, and which channels it went to.
	Policy   string   `json:"policy,omitempty"`
	Step     int      `json:"step,omitempty"`
	Channels []string `json:"channels,omitempty"`
}

// IncidentFilter narrows a FleetAPI.Incidents call to incidents matching
// every non-empty criterion; Limit <= 0 means unlimited.
type IncidentFilter struct {
	State string `json:"state"`
	Node  string `json:"node"`
	Tag   string `json:"tag"`
	Limit int    `json:"limit"`
}

// Matcher narrows a silence or maintenance window to the alerts it covers:
// every non-empty field must match (AND); an entirely empty Matcher matches
// anything, which is why a Silence/Maintenance must reject one (see
// Matcher.Empty) -- though an explicit wildcard like Rule:"*" is fine and
// deliberate (it still has a non-empty field; Empty is about a matcher with
// NO fields set at all). Rule is a shell glob (path.Match); Tag matches if
// the node carries it exactly; Severity is an exact, case-insensitive match.
//
// Node matches if it globs (path.Match) the node's DISPLAY NAME (what
// `fleet nodes` shows, e.g. "db1"), OR if it EXACTLY equals the node's
// internal id: the name is what an operator can type and glob
// (`--match node=db*`), but names are not immutable (`fleet node rename`),
// so the exact-id form gives a precise, rename-proof target when that
// matters more than typability. Renaming a node stops a name-based silence
// from matching it (its old name no longer globs the new one) -- an
// id-based silence keeps matching regardless. THE MASTER IS THE ONLY PLACE
// Node is ever evaluated (Submit's suppression check, and per-node
// filtering before the "silences" push): node names are enforced unique on
// the registry (Registry.Add/RenameNode, review round 2) specifically so
// this glob has one precise target, and the child trusts whatever the
// master already filtered for it rather than re-deriving anything (a child
// only reliably knows its OWN identity, and by the time it hears about a
// rename its cached copy is stale anyway -- see pushedSilences.Suppressed).
type Matcher struct {
	Tag      string `json:"tag,omitempty"`
	Node     string `json:"node,omitempty"`
	Rule     string `json:"rule,omitempty"`
	Severity string `json:"severity,omitempty"`
}

// Empty reports whether m has no fields set -- such a Matcher matches every
// alert on every node, which a Silence/Maintenance must never be allowed to
// contain (see the "a silence must match something" validation).
func (m Matcher) Empty() bool {
	return m.Tag == "" && m.Node == "" && m.Rule == "" && m.Severity == ""
}

// Matches reports whether m applies to an alert with the given rule
// (typically the alert's Key) and severity, on a node with the given
// id/display name/tags (all "" / nil for a master-own alert, which no
// Node/Tag matcher ever pins down but a Rule/Severity-only matcher still
// can). See Matcher's doc comment for how Node matches nodeID/nodeName.
func (m Matcher) Matches(nodeID, nodeName string, nodeTags []string, rule, severity string) bool {
	if m.Node != "" && m.Node != nodeID {
		if ok, _ := path.Match(m.Node, nodeName); !ok {
			return false
		}
	}
	if m.Tag != "" && !slices.Contains(nodeTags, m.Tag) {
		return false
	}
	if m.Rule != "" {
		if ok, _ := path.Match(m.Rule, rule); !ok {
			return false
		}
	}
	if m.Severity != "" && !strings.EqualFold(m.Severity, severity) {
		return false
	}
	return true
}

// CouldApplyToNode reports whether m might match some alert on this node
// (identified by its id/display name/tags), checking only the Node/Tag
// fields (Rule/Severity describe the alert, not the node, so a Rule- or
// Severity-only matcher always could apply). Used to decide which nodes a
// silence/maintenance window -- and which of its OR'd Matchers -- are worth
// pushing to a given node; the master is the only place this (or Matches)
// is ever called, and the exact set of Matchers it decides applies here is
// what the child trusts verbatim (see Matcher's doc comment).
func (m Matcher) CouldApplyToNode(nodeID, nodeName string, nodeTags []string) bool {
	if m.Node != "" && m.Node != nodeID {
		if ok, _ := path.Match(m.Node, nodeName); !ok {
			return false
		}
	}
	if m.Tag != "" && !slices.Contains(nodeTags, m.Tag) {
		return false
	}
	return true
}

// Silence mutes matching alerts between Start and End (unix seconds):
// recorded, never delivered, while active. Matchers is ORed (the silence
// applies if ANY entry matches); the fields WITHIN one Matcher are ANDed
// (see Matcher.Matches) -- a Silence with several Matchers is really
// several independent silences sharing one ID/window/author.
type Silence struct {
	ID       string    `json:"id"`
	Matchers []Matcher `json:"matchers"`
	Start    int64     `json:"start"`
	End      int64     `json:"end"`
	Author   string    `json:"author"`
	Comment  string    `json:"comment,omitempty"`
}

// Maintenance is a recurring window that acts exactly like a silence
// (reason "maintenance <name>") whenever it is active: Weekdays are
// time.Weekday ints (Sunday=0), From/To are "HH:MM" in TZ (an IANA name
// loaded with time.LoadLocation), and From > To means the window crosses
// midnight (owned by the weekday of its START, i.e. a Sunday 22:00 -> Monday
// 02:00 window fires on Sunday, not Monday). Matchers is ORed, its fields
// ANDed, exactly like Silence.Matchers.
type Maintenance struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Matchers []Matcher `json:"matchers"`
	Weekdays []int     `json:"weekdays"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	TZ       string    `json:"tz"`
	Author   string    `json:"author"`
}

// ManagedFragment is one master-pushed config fragment (task 8, spec 6): a
// set of allowlisted config.Config keys/values targeted at either every
// node (Tag "") or every node carrying Tag, merged with every other
// applicable fragment into that node's DESIRED set (see the trinetra
// package's managedFragmentStore.Desired) and pushed down the
// master-to-child stream as a "managed_config" frame. Version is bumped by
// SaveManaged on every save (the same counter used for the store's
// desired-set generation, so a fragment's own Version also tells you which
// generation last touched it); Author is the actor that saved it.
//
// Only ten keys are ever allowed in Values (thresholds.cpu_pct/mem_pct/
// swap_pct/temp_c/disk_pct, baseline_sigma, baseline_min_pct,
// baseline_alerts, quiet_hours, critical_overrides_quiet) -- SaveManaged
// rejects anything else, naming the key. Pushing a fallback channel set is
// explicitly OUT of scope for this fragment mechanism; it is deferred to a
// later task.
type ManagedFragment struct {
	ID      string            `json:"id"`
	Tag     string            `json:"tag,omitempty"` // "" = every node
	Values  map[string]string `json:"values"`
	Version int64             `json:"version"`
	Author  string            `json:"author,omitempty"`
}

// ManagedConflict records that more than one applicable ManagedFragment set
// the same key for a node: Fragments lists every fragment id that set Key,
// in the order they were applied (so the LAST entry is the one that
// actually won -- see managedFragmentStore.Desired's "later wins" rule).
// Recorded, never an error: a conflict does not block the desired set from
// being computed or pushed, it is only surfaced for an operator to notice
// and resolve.
type ManagedConflict struct {
	Key       string   `json:"key"`
	Fragments []string `json:"fragments"`
}

// ManagedStatus is one node's managed-config status, as FleetAPI.
// ManagedStatus reports it: Version is the version the node itself last
// reported having successfully applied (0/Applied=false if it never has, or
// its last attempt failed -- see Error); Desired is the master's CURRENT
// desired-set generation for this node, which may be ahead of Version if a
// push is still in flight or the node is unreachable. Drift lists every
// currently-desired key whose child-reported effective value does not match
// the desired value (computed from the node's last LiveUpdate.Managed,
// which may itself be stale if the node is unreachable).
type ManagedStatus struct {
	Node      string            `json:"node"`
	Version   int64             `json:"version"`
	Desired   int64             `json:"desired"`
	Applied   bool              `json:"applied"`
	Error     string            `json:"error,omitempty"`
	Drift     []string          `json:"drift,omitempty"`
	Conflicts []ManagedConflict `json:"conflicts,omitempty"`
}

// AuditEntry is one line in the fleet audit log: a record of who did what to
// the fleet (rename, revoke, ack, token create, ...) and when.
type AuditEntry struct {
	TS     int64  `json:"ts"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Route picks a Policy for an alert matching any of its Matchers (OR'd,
// exactly like Silence.Matchers -- each field within one Matcher is ANDed,
// see Matcher.Matches). Routes are evaluated in order: the first match wins,
// unless Continue is true, in which case evaluation keeps going into later
// routes too and every matched route's Policy contributes its own steps
// (fan-out) -- see the trinetra package's resolveRoute, the one function
// both the alerting engine's real delivery and RouteTest ever call. GroupBy
// is reserved for B6 (grouping); it is stored but not yet interpreted.
type Route struct {
	Name     string    `json:"name"`
	Matchers []Matcher `json:"matchers"`
	Policy   string    `json:"policy"`
	GroupBy  []string  `json:"group_by,omitempty"`
	Continue bool      `json:"continue,omitempty"`
}

// PolicyStep is one delivery step of a Policy: After is a time.ParseDuration
// string measured from the incident's first successful delivery (After:"0s"
// is the immediate, first delivery), and Channels are channel NAMES from the
// master's own config.Config.Channels -- or the literal "*", meaning every
// currently enabled channel (still gated by each channel's own Route.Allows:
// quiet hours, severity, kind).
type PolicyStep struct {
	After    string   `json:"after"`
	Channels []string `json:"channels"`
}

// Policy is a named escalation schedule: Steps fire in order as their After
// duration elapses since the incident's first delivery (unacked and still
// firing); once the last step is reached, RepeatEvery (if non-empty)
// re-notifies that last step's channels on that cadence until the incident
// is acked or resolved. SendResolved (default true when nil) gates whether a
// recover is actually delivered for incidents using this policy -- always
// recorded either way.
type Policy struct {
	Name         string       `json:"name"`
	Steps        []PolicyStep `json:"steps"`
	RepeatEvery  string       `json:"repeat_every,omitempty"`
	SendResolved *bool        `json:"send_resolved,omitempty"`
}

// AggregateRule names a grouping/aggregation rule (task 7 fills in Expr's
// grammar/evaluation, trinetra package's parseRuleExpr); AlertingConfig
// stores it, validated at Set time.
type AggregateRule struct {
	Name     string `json:"name"`
	Expr     string `json:"expr"`
	Severity string `json:"severity,omitempty"`
}

// AlertingConfig is the fleet master's routing/escalation configuration
// (spec 6), persisted to fleet/alerting.json. Version is an optimistic-
// concurrency token: SetAlerting rejects a stale Version with ErrConflict,
// except Version 0, which is unconditional (used by `fleet alerting apply`,
// which does not do optimistic locking). An absent alerting.json behaves
// exactly like {DefaultPolicy:"default", Policies:[{Name:"default",
// Steps:[{After:"0s",Channels:["*"]}], SendResolved:true}]} -- today's
// behaviour, byte for byte.
type AlertingConfig struct {
	Version       int64           `json:"version"`
	Routes        []Route         `json:"routes,omitempty"`
	Policies      []Policy        `json:"policies,omitempty"`
	DefaultPolicy string          `json:"default_policy,omitempty"`
	Rules         []AggregateRule `json:"rules,omitempty"`
}

// RuleState is one aggregate rule's current value/firing state (task 7),
// returned by FleetAPI.RuleStates in AlertingConfig.Rules order. Value is
// meaningless when HasValue is false (the rule has never produced a value
// yet); NoData is true when the rule's last evaluation found nothing to
// compute from (task-7 ruling: "no data does not fire and does not recover;
// it holds the previous state") -- Firing/Since then still reflect whatever
// they were before that. Error is set when the rule's Expr currently fails
// to parse (should not happen: SetAlerting validates every Expr before
// saving it), in which case the rule is treated as "no data" too.
type RuleState struct {
	Name     string  `json:"name"`
	Expr     string  `json:"expr"`
	Value    float64 `json:"value"`
	HasValue bool    `json:"has_value"`
	Firing   bool    `json:"firing"`
	Since    int64   `json:"since,omitempty"`
	Error    string  `json:"error,omitempty"`
	NoData   bool    `json:"no_data,omitempty"`
}

// ErrConflict is returned by FleetAPI.SetAlerting when the config's Version
// no longer matches what is actually stored (someone else saved a change
// since it was loaded) -- Version 0 bypasses this check unconditionally.
var ErrConflict = errors.New("the alerting config changed since it was loaded")

// TestAlert is a synthetic alert FleetAPI.RouteTest evaluates against the
// current routing config without actually firing anything: Node is a
// display name or exact node id (as core.Matcher.Node accepts), Rule is the
// alert key/rule glob target, Severity is the alert's severity.
type TestAlert struct {
	Node     string   `json:"node"`
	Tags     []string `json:"tags,omitempty"`
	Rule     string   `json:"rule"`
	Severity string   `json:"severity"`
}

// RouteDecision is FleetAPI.RouteTest's result: which route matched (empty
// when none did -- DefaultPolicy was used), EVERY policy that applies (more
// than one when Continue chained several matched routes together -- B5
// fix round 1: each matched policy escalates independently, so there is no
// single merged policy/step list any more), and, if a current silence/
// maintenance window would suppress this exact alert, why.
type RouteDecision struct {
	Route      string   `json:"route,omitempty"`
	Policies   []Policy `json:"policies"`
	Suppressed string   `json:"suppressed,omitempty"`
}

// FleetAPI is the set of fleet-master operations exposed alongside a
// FleetProvider's per-node API surface: fleet-wide status, the node roster,
// node management, join tokens, incidents and the audit log.
type FleetAPI interface {
	Status() (FleetStatus, error)
	Nodes(NodeFilter) ([]NodeSummary, error)
	RenameNode(id, name string) error
	SetNodeTags(id string, tags []string) error
	RevokeNode(id string) error
	// RemoveNode deletes id from the fleet (registry and liveness), resolving
	// any open node-down alert; its replicated history stays on disk.
	RemoveNode(id string) error
	// SetNodeDeps replaces id's dependency list (node ids or "tag:<t>"
	// entries): every referenced node id must exist and id may not depend on
	// itself, directly. An empty deps clears the list. Records a
	// "fleet.node.deps" audit entry naming actor.
	SetNodeDeps(id string, deps []string, actor string) error
	Tokens() ([]TokenView, error)
	CreateToken(TokenSpec) (CreatedToken, error)
	DeleteToken(id string) error

	// Incidents lists incidents matching filter, newest-updated first.
	Incidents(IncidentFilter) ([]Incident, error)
	// Incident returns one incident by id.
	Incident(id string) (Incident, error)
	// AckIncident acknowledges an incident: it pushes an ack frame to every
	// member node (applied locally via AlertState.Ack) and records actor as
	// the acknowledger.
	AckIncident(id, actor string) error
	// Explain returns the pipeline trail for an alert key or an incident id.
	Explain(key string) ([]IncidentEvent, error)
	// Audit returns the most recent audit entries, newest first, up to
	// limit (<= 0 means unlimited).
	Audit(limit int) ([]AuditEntry, error)

	// Silences lists every silence (active, future or expired) known to the
	// master.
	Silences() ([]Silence, error)
	// CreateSilence validates and stores a new silence, assigning it an ID.
	CreateSilence(Silence) (Silence, error)
	// ExpireSilence ends silence id immediately (its End is pulled back to
	// now, if it isn't already in the past), recording actor.
	ExpireSilence(id, actor string) error
	// Maintenances lists every configured maintenance window.
	Maintenances() ([]Maintenance, error)
	// SaveMaintenance validates and stores m: a new window if m.ID is "",
	// otherwise an update to the existing window with that ID.
	SaveMaintenance(Maintenance) (Maintenance, error)
	// DeleteMaintenance removes maintenance window id, recording actor.
	DeleteMaintenance(id, actor string) error

	// Alerting returns the current routing/escalation config, synthesizing
	// the built-in default (see AlertingConfig's doc comment) when
	// alerting.json has never been saved.
	Alerting() (AlertingConfig, error)
	// SetAlerting validates and atomically replaces the routing/escalation
	// config: an unknown channel or policy name, a bad duration, a bad glob,
	// or a duplicate route/policy name is rejected with nothing saved. A
	// stale cfg.Version (see AlertingConfig's doc comment) is rejected with
	// ErrConflict. actor is recorded on the "fleet.alerting.set" audit entry.
	SetAlerting(cfg AlertingConfig, actor string) error
	// RouteTest dry-runs alert through the exact same route/policy selection
	// the alerting engine uses for real delivery, plus a current-silence
	// check, without firing anything.
	RouteTest(alert TestAlert) (RouteDecision, error)
	// RuleStates returns every aggregate rule's current value/firing state
	// (task 7), in AlertingConfig.Rules order.
	RuleStates() ([]RuleState, error)

	// Managed lists every managed-config fragment (task 8), in no
	// particular guaranteed order.
	Managed() ([]ManagedFragment, error)
	// SaveManaged validates and stores a fragment: a new one (fresh random
	// id) if frag.ID is "", otherwise an in-place update of the existing
	// fragment with that id. Every key in frag.Values must be one of the
	// ten allowlisted managed-config keys and must itself validate against
	// config.Config.Set on a scratch config.Default() copy -- an unknown
	// key or a bad value is rejected with nothing saved, naming the key.
	// Records a "fleet.managed.save" audit entry under actor and pushes the
	// updated desired set to every affected, connected node.
	SaveManaged(frag ManagedFragment, actor string) (ManagedFragment, error)
	// DeleteManaged removes fragment id, records a "fleet.managed.delete"
	// audit entry under actor, and pushes the updated desired set to every
	// affected, connected node.
	DeleteManaged(id, actor string) error
	// ManagedStatus reports every node with at least one applicable
	// fragment (or an unresolved conflict): its desired-set generation, what
	// it last reported applying, and any drift/conflicts.
	ManagedStatus() ([]ManagedStatus, error)
}

// FleetProvider is optional; implementations of API that know about a fleet
// also implement it. Consumers type-assert. Solo daemons report a single
// node, SelfNodeID.
type FleetProvider interface {
	Fleet() FleetAPI
	Node(id string) (API, error)
}
