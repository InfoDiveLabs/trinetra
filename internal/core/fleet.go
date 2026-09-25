package core

import (
	"errors"
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

// FleetAPI is the set of fleet-master operations exposed alongside a
// FleetProvider's per-node API surface: fleet-wide status, the node roster,
// node management, and join tokens.
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
}

// FleetProvider is optional; implementations of API that know about a fleet
// also implement it. Consumers type-assert. Solo daemons report a single
// node, SelfNodeID.
type FleetProvider interface {
	Fleet() FleetAPI
	Node(id string) (API, error)
}
