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
	// DependsOn is this node's dependency list: each entry is another node's id or
	// "tag:<t>" for every node carrying tag t. While a dependency is down, this
	// node's node-down alert is folded into the dependency's open incident as a
	// suppressed member (see fleetAlertEngine).
	DependsOn []string `json:"depends_on,omitempty"`
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
		c.DependsOn = append([]string(nil), n.DependsOn...)
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

// Add registers a new node; the ID must be unused. A name already used
// (case-insensitively) by another node gets "-2", "-3", ... until unique, so
// silence/maintenance Matcher.Node globs have a precise target. Get(n.ID) after
// Add returns the stored name.
func (r *Registry) Add(n Node) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[n.ID]; ok {
		return fmt.Errorf("fleet: node %s already registered", n.ID)
	}
	name, err := r.uniqueNameLocked(n.Name, "")
	if err != nil {
		return err
	}
	n.Name = name
	r.nodes[n.ID] = &n
	if err := r.saveLocked(); err != nil {
		delete(r.nodes, n.ID)
		return err
	}
	return nil
}

// uniqueNameCap bounds the "-N" suffixes uniqueNameLocked tries, as a sanity cap
// against a join flow racing many joins under one name. A var so a test can
// shrink it.
var uniqueNameCap = 1000

// uniqueNameLocked returns a name not colliding case-insensitively with any node
// other than excludeID, suffixing base as needed. Caller holds r.mu.
func (r *Registry) uniqueNameLocked(base, excludeID string) (string, error) {
	name := base
	for i := 2; i <= uniqueNameCap; i++ {
		if _, taken := r.nameConflictLocked(name, excludeID); !taken {
			return name, nil
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return "", fmt.Errorf("fleet: could not find a unique name for %q after %d attempts", base, uniqueNameCap)
}

// nameConflictLocked returns the id of a node other than excludeID whose name
// matches name case-insensitively, if any. Either lock suffices.
func (r *Registry) nameConflictLocked(name, excludeID string) (string, bool) {
	for id, n := range r.nodes {
		if id == excludeID {
			continue
		}
		if strings.EqualFold(n.Name, name) {
			return id, true
		}
	}
	return "", false
}

// NameConflict reports whether name is used case-insensitively by a node other
// than excludeID. Read-only; use Rename to apply a rename, which checks under
// the same lock (a separate check-then-act would race).
func (r *Registry) NameConflict(name, excludeID string) (Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.nameConflictLocked(name, excludeID)
	if !ok {
		return Node{}, false
	}
	n := r.nodes[id]
	c := *n
	c.Tags = append([]string(nil), n.Tags...)
	c.DependsOn = append([]string(nil), n.DependsOn...)
	return c, true
}

// Rename checks name for a case-insensitive conflict with any node other than id
// and, if none, sets it and persists, all in one critical section so concurrent
// renames (or a rename racing a join) cannot both pass the check and leave a
// duplicate. The error text is what RenameNode returns verbatim.
func (r *Registry) Rename(id, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[id]
	if !ok {
		return fmt.Errorf("fleet: no node %s", id)
	}
	if conflictID, ok := r.nameConflictLocked(name, id); ok {
		return fmt.Errorf("name %q is already used by node %s", name, ShortNodeID(conflictID))
	}
	prev := n.Name
	n.Name = name
	if err := r.saveLocked(); err != nil {
		n.Name = prev
		return err
	}
	return nil
}

// ShortNodeID returns id truncated to 8 characters, for log lines and errors.
func ShortNodeID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// DuplicateNames summarizes ("name (id1, id2)") names shared case-insensitively
// by 2+ nodes, for a one-time startup warning: a registry from before names were
// unique is loaded as-is, never auto-renamed.
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
	c.DependsOn = append([]string(nil), n.DependsOn...)
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
