package fleet

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// Node is one enrolled fleet member as the master sees it.
type Node struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Tags       []string `json:"tags,omitempty"`
	Self       bool     `json:"self,omitempty"`
	PubKey     string   `json:"pubkey,omitempty"`
	CertSerial string   `json:"cert_serial,omitempty"`
	// PrevCertSerial is the certificate a renewal replaced. It is still
	// accepted until the node first presents CertSerial (so a renew response
	// lost in transit cannot lock the node out), then cleared.
	PrevCertSerial string `json:"prev_cert_serial,omitempty"`
	CertNotAfter   int64  `json:"cert_not_after,omitempty"`
	Revoked        bool   `json:"revoked,omitempty"`
	Version        string `json:"version,omitempty"`
	Joined         int64  `json:"joined,omitempty"`
	LastSeen       int64  `json:"last_seen,omitempty"`
	RemoteAddr     string `json:"remote_addr,omitempty"`
}

// NewNodeID returns 128 random bits as 32 hex chars.
func NewNodeID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Registry is the master's node list, persisted to one JSON file. LastSeen
// and friends change every few seconds, so Touch only updates memory and a
// periodic FlushIfDirty persists them; structural changes (Add/Update) save
// immediately.
type Registry struct {
	path  string
	mu    sync.RWMutex
	nodes map[string]*Node
	dirty bool
}

// OpenRegistry loads path (missing file = empty registry).
func OpenRegistry(path string) (*Registry, error) {
	r := &Registry{path: path, nodes: map[string]*Node{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Node
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("fleet: parse %s: %w", path, err)
	}
	for i := range list {
		n := list[i]
		r.nodes[n.ID] = &n
	}
	return r, nil
}

func (r *Registry) listLocked() []Node {
	out := make([]Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		c := *n
		c.Tags = append([]string(nil), n.Tags...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (r *Registry) saveLocked() error {
	b, err := json.MarshalIndent(r.listLocked(), "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(r.path, b, 0o600); err != nil {
		return err
	}
	r.dirty = false
	return nil
}

// Add registers a new node; the ID must be unused. If n.Name is already used
// (case-insensitively) by another node, it is suffixed with "-2", "-3", ...
// until unique -- names must be unique so a silence/maintenance Matcher.Node
// glob has a precise, non-ambiguous target (review round 2, item b). A
// caller that needs to know the name actually stored (e.g. a join response)
// should Get(n.ID) after Add returns.
func (r *Registry) Add(n Node) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[n.ID]; ok {
		return fmt.Errorf("fleet: node %s already registered", n.ID)
	}
	n.Name = r.uniqueNameLocked(n.Name, "")
	r.nodes[n.ID] = &n
	if err := r.saveLocked(); err != nil {
		delete(r.nodes, n.ID)
		return err
	}
	return nil
}

// uniqueNameLocked returns a name that does not collide (case-insensitively)
// with any node other than excludeID, appending "-2", "-3", ... to base as
// needed. Caller holds r.mu.
func (r *Registry) uniqueNameLocked(base, excludeID string) string {
	name := base
	for i := 2; r.nameTakenLocked(name, excludeID); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

func (r *Registry) nameTakenLocked(name, excludeID string) bool {
	for id, n := range r.nodes {
		if id == excludeID {
			continue
		}
		if strings.EqualFold(n.Name, name) {
			return true
		}
	}
	return false
}

// NameConflict reports whether name is already used (case-insensitively) by
// a node other than excludeID, returning that node if so -- used by a
// rename to reject a collision before it happens (Registry.Add/
// uniqueNameLocked handles the join case automatically; a rename is a
// deliberate operator action, so it is refused instead of silently
// suffixed).
func (r *Registry) NameConflict(name, excludeID string) (Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for id, n := range r.nodes {
		if id == excludeID {
			continue
		}
		if strings.EqualFold(n.Name, name) {
			c := *n
			c.Tags = append([]string(nil), n.Tags...)
			return c, true
		}
	}
	return Node{}, false
}

// ShortNodeID returns id truncated to 8 characters (or id itself if
// shorter), the short form used in log lines and error messages that name a
// node without printing its full 32-hex-char id.
func ShortNodeID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// DuplicateNames returns a human-readable summary ("name (id1, id2)") for
// every name shared, case-insensitively, by 2+ nodes in nodes -- used only
// for a one-time startup warning: an EXISTING registry (from before names
// were required to be unique) is loaded as-is, never auto-renamed, but the
// operator is told about it once (review round 2, item b).
func DuplicateNames(nodes []Node) []string {
	type group struct {
		name string
		ids  []string
	}
	byKey := map[string]*group{}
	var order []string
	for _, n := range nodes {
		key := strings.ToLower(n.Name)
		g, ok := byKey[key]
		if !ok {
			g = &group{name: n.Name}
			byKey[key] = g
			order = append(order, key)
		}
		g.ids = append(g.ids, n.ID)
	}
	var out []string
	for _, key := range order {
		g := byKey[key]
		if len(g.ids) < 2 {
			continue
		}
		short := make([]string, len(g.ids))
		for i, id := range g.ids {
			short[i] = ShortNodeID(id)
		}
		out = append(out, fmt.Sprintf("%s (%s)", g.name, strings.Join(short, ", ")))
	}
	return out
}

// Get returns a copy of node id.
func (r *Registry) Get(id string) (Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[id]
	if !ok {
		return Node{}, false
	}
	c := *n
	c.Tags = append([]string(nil), n.Tags...)
	return c, true
}

// List returns copies of all nodes sorted by name then id.
func (r *Registry) List() []Node {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listLocked()
}

// Update mutates node id through fn and persists. If fn errors nothing changes.
func (r *Registry) Update(id string, fn func(*Node) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[id]
	if !ok {
		return fmt.Errorf("fleet: no node %s", id)
	}
	c := *n
	if err := fn(&c); err != nil {
		return err
	}
	prev := *n
	*n = c
	if err := r.saveLocked(); err != nil {
		*n = prev
		return err
	}
	return nil
}

// Delete removes node id and persists.
func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[id]
	if !ok {
		return fmt.Errorf("fleet: no node %s", id)
	}
	delete(r.nodes, id)
	if err := r.saveLocked(); err != nil {
		r.nodes[id] = n
		return err
	}
	return nil
}

// Touch records contact from id in memory only.
func (r *Registry) Touch(id string, now int64, remoteAddr, version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[id]
	if !ok {
		return
	}
	n.LastSeen = now
	if remoteAddr != "" {
		n.RemoteAddr = remoteAddr
	}
	if version != "" {
		n.Version = version
	}
	r.dirty = true
}

// FlushIfDirty persists Touch updates, if any.
func (r *Registry) FlushIfDirty() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.dirty {
		return nil
	}
	return r.saveLocked()
}

// IsRevoked reports whether id may NOT talk to the master: unknown ids are
// treated as revoked (a cert for a deleted node must not be honoured).
func (r *Registry) IsRevoked(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[id]
	return !ok || n.Revoked
}
