package pluginhost

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type entry struct {
	token   string
	granted []plugin.Permission
	dev     bool
}

// Registrar mints and revokes the per-plugin tokens accepted on the host socket.
type Registrar struct {
	tokens *control.Tokens
	mu     sync.Mutex
	byID   map[string]entry
}

// NewRegistrar returns a Registrar that installs its grants into t.
func NewRegistrar(t *control.Tokens) *Registrar {
	return &Registrar{tokens: t, byID: map[string]entry{}}
}

// Register mints a 32-byte hex token scoped to the methods granted allows, replacing any previous token for id.
func (r *Registrar) Register(id string, granted []plugin.Permission, dev bool) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.byID[id]; ok {
		r.tokens.Remove(old.token)
	}
	r.tokens.Set(tok, control.Grant{Name: "plugin:" + id, PluginID: id, Methods: plugin.AllowedMethods(granted)})
	r.byID[id] = entry{token: tok, granted: append([]plugin.Permission(nil), granted...), dev: dev}
	return tok, nil
}

// Unregister revokes id's token.
func (r *Registrar) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.byID[id]; ok {
		r.tokens.Remove(old.token)
		delete(r.byID, id)
	}
}

// Granted returns id's permissions, whether it is a dev session, and whether id is registered.
func (r *Registrar) Granted(id string) ([]plugin.Permission, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return nil, false, false
	}
	return append([]plugin.Permission(nil), e.granted...), e.dev, true
}

// Token returns id's current token.
func (r *Registrar) Token(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	return e.token, ok
}
