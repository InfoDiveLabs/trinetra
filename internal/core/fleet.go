package core

import (
	"errors"
	"path"
	"slices"
	"strings"
)

// SelfNodeID is the node id a caller uses to mean "this daemon", in FleetProvider.Node and
// the control-socket request.Node field. resolveNode treats it the same as "".
const SelfNodeID = "self"

// ErrNotMaster is returned by a FleetAPI implementation for a call that only a fleet master
// can serve (a child or solo daemon has no fleet to report on).
var ErrNotMaster = errors.New("this trinetra is not a fleet master")

// ErrNoSuchNode is returned by FleetProvider.Node.
var ErrNoSuchNode = errors.New("no such fleet node")

// ErrNotFound is a generic "no such thing" sentinel, wrapped via fmt.Errorf("...: %w",
// ErrNotFound) by every FleetAPI lookup or mutation that names an unknown incident.
var ErrNotFound = errors.New("not found")

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
	// SkewSec is the master's filtered estimate of server_time - sent_at for this
	// node: negative means the node's clock is ahead of the master's.
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

// NodeFilter narrows a FleetAPI.Nodes call to nodes matching every non-empty criterion.
type NodeFilter struct {
	Tag   string `json:"tag"`
	State string `json:"state"`
	Query string `json:"query"`
	// Nodes, when non-empty, restricts Match to exactly the named nodes (by id or exact
	// display name), for the compare view's ?nodes=a,b,c.
	Nodes []string `json:"nodes,omitempty"`
}

// Match reports whether n passes every non-empty criterion of f.
func (f NodeFilter) Match(n NodeSummary) bool {
	if len(f.Nodes) > 0 {
		return slices.Contains(f.Nodes, n.ID) || slices.Contains(f.Nodes, n.Name)
	}
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
	// Managed is this child's own managed-config keys from the master's last managed_config
	// push: config key -> id of the fragment supplying it.
	Managed map[string]string `json:"managed,omitempty"`
}

// FleetStatus is the fleet-wide status projection FleetAPI.Status returns: this daemon's
// own role and, depending on that role, master- or child-specific fields.
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

// Incident is one (node, alert key) fire-to-recover episode as the master's alerting engine
// tracks it: opened on the first "fire", joined by more alerts, and resolved on "recover".
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
	// Suppressed, when non-empty, means this member is folded into the incident but
	// never delivered on its own while set: currently only a node-dependency fold
	// ("suppressed: parent <name> down"). Cleared, and the member delivered as its
	// own incident, when the dependency releases (see fleetAlertEngine).
	Suppressed string `json:"suppressed,omitempty"`
	// SilencedBy, when non-empty, is the silence/maintenance reason this member's own fire
	// matched.
	SilencedBy string `json:"silenced_by,omitempty"`
}

// IncidentEvent is one entry in an Incident's pipeline trail (fleet explain prints these):
// fired|grouped|suppressed|delivered|escalated|acked|resolved|receipt.
type IncidentEvent struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
	Actor  string `json:"actor,omitempty"`

	// Leg is "fire" or "recover".
	Leg string `json:"leg,omitempty"`
	// AlertKey/Node/FiredAt identify the IncidentAlert member this event is about,
	// like alertDedupKey; Node is "" for a master-own alert.
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

// Matcher narrows a silence or maintenance window to the alerts it covers: every non-empty
// field must match (AND).
type Matcher struct {
	Tag      string `json:"tag,omitempty"`
	Node     string `json:"node,omitempty"`
	Rule     string `json:"rule,omitempty"`
	Severity string `json:"severity,omitempty"`
}

// Empty reports whether m has no fields set; such a Matcher matches every alert
// and must be rejected for a Silence/Maintenance.
func (m Matcher) Empty() bool {
	return m.Tag == "" && m.Node == "" && m.Rule == "" && m.Severity == ""
}

// Matches reports whether m applies to an alert with the given rule (typically the alert's
// Key) and severity on a node with the given id/name/tags.
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

// CouldApplyToNode reports whether m might match some alert on this node,
// checking only Node/Tag (Rule/Severity describe the alert, so a matcher with
// only those always could apply). The master uses it to choose which silences
// and maintenance windows to push to a node.
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

// Silence mutes matching alerts between Start and End (unix seconds): recorded, never
// delivered, while active.
type Silence struct {
	ID       string    `json:"id"`
	Matchers []Matcher `json:"matchers"`
	Start    int64     `json:"start"`
	End      int64     `json:"end"`
	Author   string    `json:"author"`
	Comment  string    `json:"comment,omitempty"`
}

// Maintenance is a recurring window that acts like a silence (reason "maintenance <name>")
// while active: Weekdays are time.Weekday ints (Sunday=0), From/To are "HH:MM" in TZ.
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

// ManagedKeys is the closed set of config.Config keys a managed-config fragment may set:
// five thresholds, the three baseline/anomaly keys.
var ManagedKeys = []string{
	"thresholds.cpu_pct", "thresholds.mem_pct", "thresholds.swap_pct",
	"thresholds.temp_c", "thresholds.disk_pct",
	"baseline_sigma", "baseline_min_pct", "baseline_alerts",
	"quiet_hours", "critical_overrides_quiet",
}

// ManagedFragment is one master-pushed config fragment: allowlisted config.Config
// keys/values for every node (Tag "") or every node carrying Tag.
type ManagedFragment struct {
	ID      string            `json:"id"`
	Tag     string            `json:"tag,omitempty"` // "" = every node
	Values  map[string]string `json:"values"`
	Version int64             `json:"version"`
	Author  string            `json:"author,omitempty"`
	// Merge, when true, makes SaveManaged merge Values into the existing fragment (new keys
	// winning) instead of replacing them wholesale.
	Merge bool `json:"merge,omitempty"`
}

// ManagedConflict records that more than one applicable ManagedFragment set the same key
// for a node: Fragments lists every fragment id that set Key in application order.
type ManagedConflict struct {
	Key       string   `json:"key"`
	Fragments []string `json:"fragments"`
}

// ManagedStatus is one node's managed-config status.
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

// Route picks a Policy for an alert matching any of its Matchers (ORed; fields within one
// are ANDed).
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

// Policy is a named escalation schedule: Steps fire in order as their After duration
// elapses since the incident's first delivery (unacked and still firing).
type Policy struct {
	Name         string       `json:"name"`
	Steps        []PolicyStep `json:"steps"`
	RepeatEvery  string       `json:"repeat_every,omitempty"`
	SendResolved *bool        `json:"send_resolved,omitempty"`
}

// AggregateRule names a grouping/aggregation rule; AlertingConfig stores it,
// validated at Set time.
type AggregateRule struct {
	Name     string `json:"name"`
	Expr     string `json:"expr"`
	Severity string `json:"severity,omitempty"`
}

// AlertingConfig is the fleet master's routing/escalation configuration, persisted to
// fleet/alerting.json.
type AlertingConfig struct {
	Version       int64           `json:"version"`
	Routes        []Route         `json:"routes,omitempty"`
	Policies      []Policy        `json:"policies,omitempty"`
	DefaultPolicy string          `json:"default_policy,omitempty"`
	Rules         []AggregateRule `json:"rules,omitempty"`
}

// RuleState is one aggregate rule's current state, returned by FleetAPI.RuleStates in
// AlertingConfig.Rules order.
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

// ErrConflict is returned by FleetAPI.SetAlerting when the config's Version no
// longer matches what is stored; Version 0 bypasses the check.
var ErrConflict = errors.New("the alerting config changed since it was loaded")

// TestAlert is a synthetic alert FleetAPI.RouteTest evaluates against the current routing
// config without actually firing anything: Node is a display name or exact node id.
type TestAlert struct {
	Node     string   `json:"node"`
	Tags     []string `json:"tags,omitempty"`
	Rule     string   `json:"rule"`
	Severity string   `json:"severity"`
}

// RouteDecision is FleetAPI.RouteTest's result: which route matched (empty when none did
// and DefaultPolicy was used), EVERY policy that applies.
type RouteDecision struct {
	Route      string   `json:"route,omitempty"`
	Policies   []Policy `json:"policies"`
	Suppressed string   `json:"suppressed,omitempty"`
}

// FleetAPI is the set of fleet-master operations exposed alongside a FleetProvider's
// per-node API surface: fleet-wide status, the node roster, node management, join tokens.
type FleetAPI interface {
	Status() (FleetStatus, error)
	Nodes(NodeFilter) ([]NodeSummary, error)
	// RenameNode renames id, recording a "fleet.node.rename" audit entry under
	// actor (the CLI's "cli", or the signed-in web user).
	RenameNode(id, name, actor string) error
	// SetNodeTags replaces id's tag set, recording a "fleet.node.tags" audit entry
	// under actor.
	SetNodeTags(id string, tags []string, actor string) error
	// RevokeNode revokes id, recording a "fleet.node.revoke" audit entry under actor.
	RevokeNode(id, actor string) error
	// RemoveNode deletes id from the fleet (registry and liveness), resolving any open
	// node-down alert; its replicated history stays on disk.
	RemoveNode(id, actor string) error
	// SetNodeDeps replaces id's dependency list (node ids or "tag:<t>" entries): every
	// referenced node id must exist and id may not depend on itself, directly.
	SetNodeDeps(id string, deps []string, actor string) error
	Tokens() ([]TokenView, error)
	// CreateToken mints a join token.
	CreateToken(TokenSpec) (CreatedToken, error)
	// DeleteToken removes join token id, recording a "fleet.token.delete" audit entry
	// under actor.
	DeleteToken(id, actor string) error

	// Incidents lists incidents matching filter, newest-updated first.
	Incidents(IncidentFilter) ([]Incident, error)
	// Incident returns one incident by id.
	Incident(id string) (Incident, error)
	// AckIncident acknowledges an incident: it pushes an ack frame to every member node
	// (applied locally via AlertState.Ack) and records actor as the acknowledger.
	AckIncident(id, actor string) error
	// Explain returns the pipeline trail for an alert key or an incident id.
	Explain(key string) ([]IncidentEvent, error)
	// Audit returns the most recent audit entries, newest first, up to
	// limit (<= 0 means unlimited).
	Audit(limit int) ([]AuditEntry, error)

	// Silences lists every silence (active, future or expired) known to the master.
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

	// Alerting returns the current routing/escalation config, synthesizing the built-in
	// default (see AlertingConfig's doc comment) when alerting.json has never been saved.
	Alerting() (AlertingConfig, error)
	// SetAlerting validates and atomically replaces the routing/escalation config: an unknown
	// channel or policy name, a bad duration, a bad glob.
	SetAlerting(cfg AlertingConfig, actor string) error
	// RouteTest dry-runs alert through the exact same route/policy selection the alerting
	// engine uses for real delivery, plus a current-silence check, without firing anything.
	RouteTest(alert TestAlert) (RouteDecision, error)
	// RuleStates returns every aggregate rule's current state, in AlertingConfig.Rules order.
	RuleStates() ([]RuleState, error)

	// Managed lists every managed-config fragment, in no guaranteed order.
	Managed() ([]ManagedFragment, error)
	// SaveManaged validates and stores a fragment: a new one (fresh random id) if frag.ID is
	// "", otherwise an in-place update of the existing fragment with that id.
	SaveManaged(frag ManagedFragment, actor string) (ManagedFragment, error)
	// DeleteManaged removes fragment id, records a "fleet.managed.delete" audit entry under
	// actor, and pushes the updated desired set to every affected, connected node.
	DeleteManaged(id, actor string) error
	// ManagedStatus reports every node with at least one applicable fragment (or an unresolved
	// conflict): its desired-set generation, what it last reported applying.
	ManagedStatus() ([]ManagedStatus, error)

	// FleetSeries reads metric's time series across every node matching filter, backing the
	// web compare view. agg "none" returns one series per node (Node set to its display name).
	FleetSeries(metric string, filter NodeFilter, agg Agg, from, to int64, res Resolution) ([]FleetSeriesPoint, error)
}

// Agg picks how FleetSeries combines nodes: "none" keeps one series per node,
// "avg"/"max"/"min" collapse them into one.
type Agg string

const (
	AggNone Agg = "none"
	AggAvg  Agg = "avg"
	AggMax  Agg = "max"
	AggMin  Agg = "min"
)

// FleetSeriesPoint is one sample of a FleetAPI.FleetSeries result: Node is the node's
// display name for an agg="none" per-node series, or "" for an aggregated.
type FleetSeriesPoint struct {
	Node  string  `json:"node"`
	TS    int64   `json:"ts"`
	Value float64 `json:"value"`
}

// FleetProvider is optional; implementations of API that know about a fleet also implement
// it.
type FleetProvider interface {
	Fleet() FleetAPI
	Node(id string) (API, error)
}
