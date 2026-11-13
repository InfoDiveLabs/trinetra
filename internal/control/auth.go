package control

import (
	"crypto/subtle"
	"strings"
	"sync"
)

// Grant is what a token may do.
type Grant struct {
	Name     string
	PluginID string
	Full     bool
	Methods  map[string]bool
}

// Allows reports whether method may be called under g.
func (g Grant) Allows(method string) bool {
	if g.Full {
		return !strings.HasPrefix(method, "Plugin.")
	}
	return g.Methods[method]
}

// Authorizer maps a presented token to its grant.
type Authorizer interface {
	Authorize(token string) (Grant, bool)
}

// Tokens is a concurrency-safe Authorizer; lookup compares every entry in constant time.
type Tokens struct {
	mu sync.RWMutex
	m  map[string]Grant
}

// NewTokens returns an empty token table.
func NewTokens() *Tokens { return &Tokens{m: map[string]Grant{}} }

// Set registers token with grant g, replacing any previous grant for it.
func (t *Tokens) Set(token string, g Grant) {
	t.mu.Lock()
	t.m[token] = g
	t.mu.Unlock()
}

// Remove revokes token.
func (t *Tokens) Remove(token string) {
	t.mu.Lock()
	delete(t.m, token)
	t.mu.Unlock()
}

// RemoveName revokes every token whose grant carries name.
func (t *Tokens) RemoveName(name string) {
	t.mu.Lock()
	for tok, g := range t.m {
		if g.Name == name {
			delete(t.m, tok)
		}
	}
	t.mu.Unlock()
}

// Authorize implements Authorizer.
func (t *Tokens) Authorize(token string) (Grant, bool) {
	if token == "" {
		return Grant{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	var found Grant
	ok := 0
	for tok, g := range t.m {
		eq := subtle.ConstantTimeCompare([]byte(tok), []byte(token))
		if eq == 1 {
			found = g
		}
		ok |= eq
	}
	return found, ok == 1
}

// FullAccess is the grant held by the daemon's launch token (ctl, web, CLI).
func FullAccess(name string) Grant { return Grant{Name: name, Full: true} }

// singleToken is the Authorizer behind Serve: one full-access token, or none required when empty.
type singleToken string

func (s singleToken) Authorize(token string) (Grant, bool) {
	if s == "" {
		return FullAccess("launch"), true
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(s)) == 1 {
		return FullAccess("launch"), true
	}
	return Grant{}, false
}
