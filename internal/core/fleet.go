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
	Key              string `json:"key"`
	Title            string `json:"title"`
	Severity         string `json:"severity"`
	FiredAt          int64  `json:"fired_at"`
	ResolvedAt       int64  `json:"resolved_at,omitempty"`
	DeliveredLocally bool   `json:"delivered_locally,omitempty"`
}

// IncidentEvent is one entry in an Incident's pipeline trail (fleet explain
// prints these): fired|grouped|suppressed|delivered|escalated|acked|resolved|receipt.
type IncidentEvent struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
	Actor  string `json:"actor,omitempty"`
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
// NO fields set at all). Node and Rule are shell globs (path.Match); Tag
// matches if the node carries it exactly; Severity is an exact,
// case-insensitive match.
//
// Node matches against the node's DISPLAY NAME (what `fleet nodes` shows,
// e.g. "db1"), never its internal hex node id: an operator writing
// `--match node=db*` has no reason to know or type the random id, and only
// the name is typable/globbable. Both the master (Submit's suppression
// check, the per-node "silences" push) and the child (its own defense-in-depth
// re-check, see pushedSilences.Suppressed) match Node this way.
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
// display name/tags (both "" / nil for a master-own alert, which no
// Node/Tag matcher ever pins down but a Rule/Severity-only matcher still
// can).
func (m Matcher) Matches(nodeName string, nodeTags []string, rule, severity string) bool {
	if m.Node != "" {
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
// (identified by its display name/tags), checking only the Node/Tag fields
// (Rule/Severity describe the alert, not the node, so a Rule- or
// Severity-only matcher always could apply). Used to decide which nodes a
// silence/maintenance window -- and which of its OR'd Matchers -- are worth
// pushing to a given node.
func (m Matcher) CouldApplyToNode(nodeName string, nodeTags []string) bool {
	if m.Node != "" {
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

// AuditEntry is one line in the fleet audit log: a record of who did what to
// the fleet (rename, revoke, ack, token create, ...) and when.
type AuditEntry struct {
	TS     int64  `json:"ts"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail,omitempty"`
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
}

// FleetProvider is optional; implementations of API that know about a fleet
// also implement it. Consumers type-assert. Solo daemons report a single
// node, SelfNodeID.
type FleetProvider interface {
	Fleet() FleetAPI
	Node(id string) (API, error)
}
