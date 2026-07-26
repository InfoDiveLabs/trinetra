//go:build web

package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Role is a web UI account's access level. requireRole (middleware.go)
// enforces it at the route level; resolveEnrollRole (enroll_tokens.go)
// decides what a newly-enrolled account's Role is (first-run bootstrap or
// an admin-issued enrollment token).
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

// Credential is one registered passkey, in the flattened shape the design
// doc's "Users store" section specifies (users.json: id, name, role,
// created, credentials[]{id, publicKey, signCount, transports}). It mirrors
// the fields of *webauthn.Credential this package actually persists —
// AttestationType/Flags/Authenticator.AAGUID aren't needed after the
// ceremony completes, so they're dropped rather than round-tripped.
type Credential struct {
	// ID is the credential ID the authenticator generated, used to look the
	// credential up again during login (Task 5/#61).
	ID []byte `json:"id"`
	// PublicKey is the COSE-encoded public key bytes go-webauthn extracted
	// from the attestation object; the private key never leaves the
	// authenticator.
	PublicKey []byte `json:"publicKey"`
	// SignCount is the authenticator's signature counter at registration
	// time (usually 0); login ceremonies (#61) update and persist it to
	// detect cloned authenticators (a signCount that goes backwards).
	SignCount uint32 `json:"signCount"`
	// Transports hints at how the browser may reach this authenticator
	// again (e.g. "internal", "usb", "hybrid"), used to skip transport
	// probing on a later login ceremony.
	Transports []string `json:"transports,omitempty"`
}

// User is a web UI account. It implements webauthn.User (see the
// WebAuthn... methods below) so it can be passed directly to
// *webauthn.WebAuthn's registration/login ceremonies.
type User struct {
	// ID is this user's WebAuthn user handle: an opaque, random identifier
	// (see newUserID), never the display Name — go-webauthn's User.WebAuthnID
	// doc warns identity decisions must key off this, not Name.
	ID string `json:"id"`
	// Name is the human-palatable account name (display name and username
	// are the same value here; the mockup/design doc doesn't distinguish
	// them for this app).
	Name string `json:"name"`
	// Role is this account's access level (RoleAdmin/RoleViewer), assigned by
	// resolveEnrollRole (enroll_tokens.go) at enrollment time: first-run
	// bootstrap or an admin-issued enrollment token's Role.
	Role Role `json:"role"`
	// Created is the Unix seconds timestamp the account was first enrolled.
	Created int64 `json:"created"`
	// Credentials holds every passkey this user has registered. A single
	// user may hold more than one (e.g. a phone + a security key), so
	// registration appends rather than replaces.
	Credentials []Credential `json:"credentials,omitempty"`
}

// WebAuthnID returns u.ID as raw bytes: the WebAuthn user handle. Must stay
// stable for the lifetime of the account (it's what BeginRegistration's
// SessionData.UserID and FinishRegistration's equality check key off of).
func (u *User) WebAuthnID() []byte { return []byte(u.ID) }

// WebAuthnName satisfies webauthn.User; see User.Name's doc.
func (u *User) WebAuthnName() string { return u.Name }

// WebAuthnDisplayName satisfies webauthn.User; this app doesn't distinguish
// a separate display name from the account name.
func (u *User) WebAuthnDisplayName() string { return u.Name }

// WebAuthnIcon satisfies webauthn.User. The interface's doc marks this
// deprecated by the spec; go-webauthn still requires the method, so it's a
// permanent blank stub.
func (u *User) WebAuthnIcon() string { return "" }

// WebAuthnCredentials adapts u.Credentials (this package's flattened
// storage shape) into the []webauthn.Credential shape go-webauthn's login
// ceremony (Task 5/#61) needs to match an assertion against. Registration
// doesn't consult this (a brand-new user has none yet), but it's part of
// the webauthn.User interface contract regardless.
func (u *User) WebAuthnCredentials() []webauthn.Credential {
	out := make([]webauthn.Credential, len(u.Credentials))
	for i, c := range u.Credentials {
		out[i] = webauthn.Credential{
			ID:        c.ID,
			PublicKey: c.PublicKey,
			Transport: transportsFromStrings(c.Transports),
			Authenticator: webauthn.Authenticator{
				SignCount: c.SignCount,
			},
		}
	}
	return out
}

// var _ webauthn.User = (*User)(nil) pins the interface implementation at
// compile time: if a go-webauthn upgrade adds/changes a User method, the
// default (!web-tagged-out, but still `-tags web`) build fails loudly here
// instead of failing obscurely inside BeginRegistration.
var _ webauthn.User = (*User)(nil)

// transportsFromStrings converts the stored string transport hints back
// into go-webauthn's protocol.AuthenticatorTransport, the type
// webauthn.Credential.Transport expects.
func transportsFromStrings(ss []string) []protocol.AuthenticatorTransport {
	if len(ss) == 0 {
		return nil
	}
	out := make([]protocol.AuthenticatorTransport, len(ss))
	for i, s := range ss {
		out[i] = protocol.AuthenticatorTransport(s)
	}
	return out
}

// transportsToStrings is transportsFromStrings' inverse: what
// finishRegistration calls to flatten a freshly-verified *webauthn.Credential's
// Transport field into this package's storage shape.
func transportsToStrings(ts []protocol.AuthenticatorTransport) []string {
	if len(ts) == 0 {
		return nil
	}
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}

// UserStore is how the web package persists/looks up accounts. The only
// implementation today is jsonUserStore (below); it's an interface so a
// future task can swap backends (or a test can fake one) without touching
// callers.
type UserStore interface {
	Get(id string) (*User, bool)
	ByName(name string) (*User, bool)
	Put(u *User) error
	List() []*User
	Delete(id string) error
	// CreateFirstAdmin atomically persists u as the very first account (role
	// forced to RoleAdmin) IFF the store is still empty, else fails — the
	// first-run bootstrap decision made under the same lock as the write.
	CreateFirstAdmin(u *User) error
}

// jsonUserStore is UserStore backed by a single JSON file
// (<StateDir>/users.json). It intentionally does not cache the parsed
// users in memory between calls: every method reloads from disk under
// s.mu, so concurrent goroutines within this process always see the latest
// persisted state and Put/Delete's read-modify-write can't race each other
// (a second process editing the file concurrently is out of scope — nothing
// else in this daemon does that). Given the low request volume of an
// enrollment/login ceremony, the extra disk I/O per call is not a concern.
type jsonUserStore struct {
	mu   sync.Mutex
	path string
}

// newUserStore returns a UserStore rooted at <stateDir>/users.json. This
// does not touch disk (no file is created, no error is possible) until an
// operation is actually performed, so it is always safe to construct even
// when stateDir doesn't exist yet or is "" (e.g. a test/handler that never
// reaches an auth route).
func newUserStore(stateDir string) *jsonUserStore {
	return &jsonUserStore{path: filepath.Join(stateDir, "users.json")}
}

// loadLocked reads and parses the store file, returning (nil, nil) if it
// doesn't exist yet (a fresh install/StateDir). Callers must hold s.mu.
func (s *jsonUserStore) loadLocked() ([]*User, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("web: read %s: %w", s.path, err)
	}
	var users []*User
	if err := json.Unmarshal(b, &users); err != nil {
		return nil, fmt.Errorf("web: parse %s: %w", s.path, err)
	}
	return users, nil
}

// saveLocked atomically rewrites the store file with users, tightening
// perms to 0600: users.json holds WebAuthn public keys and account
// metadata, not a secret by itself, but there's no reason to leave it
// group/world-readable either. Atomic (write-temp + rename) so a crash
// mid-write can never leave a truncated/corrupt file behind, mirroring
// internal/config.Config.Save's approach. Callers must hold s.mu.
func (s *jsonUserStore) saveLocked(users []*User) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("web: create %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return fmt.Errorf("web: encode users: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("web: write %s: %w", tmp, err)
	}
	// Explicit Chmod after a 0600 WriteFile is belt-and-suspenders: WriteFile
	// only applies the mode when it CREATES the file, so on the (rare) path
	// where a stale tmp from a previous crash already exists with wider
	// perms, this tightens it back to 0600. 0600 has no group/world bits to
	// widen, so it can never loosen perms.
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("web: chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("web: rename %s -> %s: %w", tmp, s.path, err)
	}
	return nil
}

// Get returns the user with the given WebAuthn user handle (User.ID), or
// (nil, false) if none exists or the store can't be read.
func (s *jsonUserStore) Get(id string) (*User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return nil, false
	}
	for _, u := range users {
		if u.ID == id {
			return u, true
		}
	}
	return nil, false
}

// ByName returns the first user with the given Name, or (nil, false) if
// none exists. TODO(#62): this store does not (yet) enforce Name uniqueness
// on Put; enrollBeginHandler uses ByName only to REJECT a duplicate-name
// enrollment (never to attach to an existing account), so the takeover risk
// is closed regardless, but Task 6's user-management/Put path should add a
// uniqueness constraint so two accounts can't share a name in the first
// place.
func (s *jsonUserStore) ByName(name string) (*User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return nil, false
	}
	for _, u := range users {
		if u.Name == name {
			return u, true
		}
	}
	return nil, false
}

// Put inserts u, or replaces the existing user with the same ID, and
// persists the result.
func (s *jsonUserStore) Put(u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i, existing := range users {
		if existing.ID == u.ID {
			users[i] = u
			return s.saveLocked(users)
		}
	}
	users = append(users, u)
	return s.saveLocked(users)
}

// CreateFirstAdmin atomically persists u as the first-ever account, forcing
// its role to RoleAdmin — but ONLY if the store is still empty; otherwise it
// returns an error and writes nothing. Both the emptiness check and the
// append happen under the SAME s.mu, which is what closes the first-run
// bootstrap TOCTOU: an earlier design decided "0 users ⇒ admin" at
// /enroll/begin (before the WebAuthn round-trip) and only wrote the account
// at /enroll/finish, so two tokenless enrollments started before either
// finished both observed an empty store and both became admin. Deciding it
// here, at finish, under the write lock means whichever finish acquires the
// lock first becomes the sole admin and every later one sees a non-empty
// store and is rejected (tokenless enrollment is closed the moment one
// account exists).
func (s *jsonUserStore) CreateFirstAdmin(u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return err
	}
	if len(users) != 0 {
		return fmt.Errorf("web: enrollment is closed; the first account already exists")
	}
	u.Role = RoleAdmin
	users = append(users, u)
	return s.saveLocked(users)
}

// List returns every stored user, or nil if the store is empty/unreadable.
func (s *jsonUserStore) List() []*User {
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return nil
	}
	return users
}

// Delete removes the user with the given ID, reporting an error if no such
// user exists.
func (s *jsonUserStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i, u := range users {
		if u.ID == id {
			users = append(users[:i], users[i+1:]...)
			return s.saveLocked(users)
		}
	}
	return fmt.Errorf("web: user %q not found", id)
}

// var _ UserStore = (*jsonUserStore)(nil) pins the interface implementation
// at compile time.
var _ UserStore = (*jsonUserStore)(nil)
