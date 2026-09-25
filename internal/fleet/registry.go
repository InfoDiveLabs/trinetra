package fleet

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
)

// Node is one enrolled fleet member as the master sees it.
type Node struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Tags         []string `json:"tags,omitempty"`
	Self         bool     `json:"self,omitempty"`
	PubKey       string   `json:"pubkey,omitempty"`
	CertSerial   string   `json:"cert_serial,omitempty"`
	CertNotAfter int64    `json:"cert_not_after,omitempty"`
	Revoked      bool     `json:"revoked,omitempty"`
	Version      string   `json:"version,omitempty"`
	Joined       int64    `json:"joined,omitempty"`
	LastSeen     int64    `json:"last_seen,omitempty"`
	RemoteAddr   string   `json:"remote_addr,omitempty"`
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

// Add registers a new node; the ID must be unused.
func (r *Registry) Add(n Node) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[n.ID]; ok {
		return fmt.Errorf("fleet: node %s already registered", n.ID)
	}
	r.nodes[n.ID] = &n
	if err := r.saveLocked(); err != nil {
		delete(r.nodes, n.ID)
		return err
	}
	return nil
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
