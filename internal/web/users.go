package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Role is a web UI account's access level. requireRole (middleware.go) enforces it at the
// route level; resolveEnrollRole.
type Role string

const (
	RoleAdmin     Role = "admin"
	RoleResponder Role = "responder" // posts status-page updates, acks alerts; no config/users/channels (issue #157)
	RoleViewer    Role = "viewer"
)

// roleRank orders roles for requireRole: viewer < responder < admin; an
// unknown or empty role ranks 0 and satisfies nothing.
func roleRank(r Role) int {
	switch r {
	case RoleViewer:
		return 1
	case RoleResponder:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

func validRole(r Role) bool { return roleRank(r) > 0 }

// Credential is one registered passkey, in the flattened shape the design doc's "Users
// store" section specifies.
type Credential struct {
	// ID is the credential ID the authenticator generated, used to look the
	// credential up again during login (Task 5/#61).
	ID []byte `json:"id"`
	// PublicKey is the COSE-encoded public key bytes go-webauthn extracted from the
	// attestation object; the private key never leaves the authenticator.
	PublicKey []byte `json:"publicKey"`
	// SignCount is the authenticator's signature counter at registration time (usually 0);
	// login ceremonies (#61) update and persist it to detect cloned authenticators.
	SignCount uint32 `json:"signCount"`
	// Transports hints at how the browser may reach this authenticator again (e.g. "internal",
	// "usb", "hybrid"), used to skip transport probing on a later login ceremony.
	Transports []string `json:"transports,omitempty"`
}

// User is a web UI account.
type User struct {
	// ID is this user's WebAuthn user handle: an opaque, random identifier (see newUserID),
	// never the display Name.
	ID string `json:"id"`
	// Name is the human-palatable account name (display name and username are the same value
	// here; the mockup/design doc doesn't distinguish them for this app).
	Name string `json:"name"`
	// Role is this account's access level (RoleAdmin/RoleResponder/RoleViewer, ranked viewer <
	// responder < admin), assigned by resolveEnrollRole (enroll_tokens.go) at enrollment time.
	Role Role `json:"role"`
	// Created is the Unix seconds timestamp the account was first enrolled.
	Created int64 `json:"created"`
	// Credentials holds every passkey this user has registered.
	Credentials []Credential `json:"credentials,omitempty"`
}

// WebAuthnID returns u.ID as raw bytes: the WebAuthn user handle.
func (u *User) WebAuthnID() []byte { return []byte(u.ID) }

// WebAuthnName satisfies webauthn.User; see User.Name's doc.
func (u *User) WebAuthnName() string { return u.Name }

// WebAuthnDisplayName satisfies webauthn.User; this app doesn't distinguish
// a separate display name from the account name.
func (u *User) WebAuthnDisplayName() string { return u.Name }

// WebAuthnIcon satisfies webauthn.User.
func (u *User) WebAuthnIcon() string { return "" }

// WebAuthnCredentials adapts u.Credentials (this package's flattened storage shape) into
// the []webauthn.Credential shape go-webauthn's login ceremony.
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

// var _ webauthn.User = (*User)(nil) pins the interface implementation at compile time: if
// a go-webauthn upgrade adds/changes a User method, the trinetra-web build.
var _ webauthn.User = (*User)(nil)

// transportsFromStrings converts the stored string transport hints back into go-webauthn's
// protocol.AuthenticatorTransport, the type webauthn.Credential.Transport expects.
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

// transportsToStrings is transportsFromStrings' inverse.
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

// UserStore is how the web package persists/looks up accounts.
type UserStore interface {
	Get(id string) (*User, bool)
	ByName(name string) (*User, bool)
	Put(u *User) error
	List() []*User
	// IsEmpty reports whether the store holds zero accounts, distinguishing a genuinely empty
	// store (absent file -> true, nil) from one that exists but cannot be read.
	IsEmpty() (bool, error)
	Delete(id string) error
	// CreateFirstAdmin atomically persists u as the very first account (role forced to
	// RoleAdmin) IFF the store is still empty, else fails.
	CreateFirstAdmin(u *User) error
	// SetRoleUnlessLastAdmin sets user id's role, but refuses (errLastAdmin) to demote the
	// sole remaining admin -- the load, the last-admin check.
	SetRoleUnlessLastAdmin(id string, role Role) error
	// RemoveUnlessLastAdmin deletes user id, but refuses (errLastAdmin) to remove the sole
	// remaining admin.
	RemoveUnlessLastAdmin(id string) error
	// RevokeCredentialUnlessLastAdmin removes credential credID from user id's Credentials,
	// but refuses.
	RevokeCredentialUnlessLastAdmin(id, credID string) error
}

// errLastAdmin/errUserNotFound/errLastAdminCredential/errCredentialNotFound are the
// sentinel errors the atomic guard methods.
var (
	errLastAdmin           = errors.New("web: refusing to leave the store with no admin")
	errUserNotFound        = errors.New("web: user not found")
	errLastAdminCredential = errors.New("web: refusing to revoke the last admin's last credential")
	errCredentialNotFound  = errors.New("web: credential not found")
)

// countAdmins reports how many of users hold RoleAdmin -- the last-admin
// guard's input, evaluated on the in-lock snapshot the atomic methods hold.
func countAdmins(users []*User) int {
	n := 0
	for _, u := range users {
		if u.Role == RoleAdmin {
			n++
		}
	}
	return n
}

// jsonUserStore is UserStore backed by a single JSON file (<StateDir>/users.json).
type jsonUserStore struct {
	path string
}

// fileStoreMutexes holds one *sync.Mutex per absolute file path, so every file-backed store
// instance.
var (
	fileStoreMutexes   = map[string]*sync.Mutex{}
	fileStoreMutexesMu sync.Mutex
)

// fileStoreMutex returns the process-wide mutex for path (creating it on first use).
// filepath.Abs canonicalizes the key so two spellings of the same path share the lock.
func fileStoreMutex(path string) *sync.Mutex {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	fileStoreMutexesMu.Lock()
	defer fileStoreMutexesMu.Unlock()
	mu, ok := fileStoreMutexes[abs]
	if !ok {
		mu = &sync.Mutex{}
		fileStoreMutexes[abs] = mu
	}
	return mu
}

// newUserStore returns a UserStore rooted at <stateDir>/users.json.
func newUserStore(stateDir string) *jsonUserStore {
	return &jsonUserStore{path: filepath.Join(stateDir, "users.json")}
}

// loadLocked reads and parses the store file, returning (nil, nil) if it
// doesn't exist yet (a fresh install/StateDir). Callers must hold the store's fileStoreMutex.
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

// saveLocked atomically rewrites the store file with users, tightening perms to 0600:
// users.json holds WebAuthn public keys and account metadata, not a secret by itself.
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
	// Explicit Chmod after a 0600 WriteFile is belt-and-suspenders: WriteFile only applies the
	// mode when it CREATES the file.
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
	defer lockStore(s.path)()
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

// ByName returns the first user with the given Name, or (nil, false) if none exists.
func (s *jsonUserStore) ByName(name string) (*User, bool) {
	defer lockStore(s.path)()
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

// Put inserts u, or replaces the existing user with the same ID, and persists the result.
func (s *jsonUserStore) Put(u *User) error {
	defer lockStore(s.path)()
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

// CreateFirstAdmin atomically persists u as the first-ever account, forcing its role to
// RoleAdmin -- but ONLY if the store is still empty.
func (s *jsonUserStore) CreateFirstAdmin(u *User) error {
	defer lockStore(s.path)()
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
	defer lockStore(s.path)()
	users, err := s.loadLocked()
	if err != nil {
		return nil
	}
	return users
}

// IsEmpty reports whether the store holds zero accounts.
func (s *jsonUserStore) IsEmpty() (bool, error) {
	defer lockStore(s.path)()
	users, err := s.loadLocked()
	if err != nil {
		return false, err
	}
	return len(users) == 0, nil
}

// Delete removes the user with the given ID, reporting an error if no such user exists.
func (s *jsonUserStore) Delete(id string) error {
	defer lockStore(s.path)()
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

// SetRoleUnlessLastAdmin sets user id's Role to role, all under a SINGLE fileStoreMutex
// critical section: it loads the current users, and only if demoting id.
func (s *jsonUserStore) SetRoleUnlessLastAdmin(id string, role Role) error {
	defer lockStore(s.path)()
	users, err := s.loadLocked()
	if err != nil {
		return err
	}
	var target *User
	for _, u := range users {
		if u.ID == id {
			target = u
			break
		}
	}
	if target == nil {
		return errUserNotFound
	}
	if target.Role == RoleAdmin && role != RoleAdmin && countAdmins(users) <= 1 {
		return errLastAdmin
	}
	target.Role = role
	return s.saveLocked(users)
}

// RemoveUnlessLastAdmin deletes user id under a SINGLE fileStoreMutex critical section,
// refusing (errLastAdmin) to delete the sole remaining admin.
func (s *jsonUserStore) RemoveUnlessLastAdmin(id string) error {
	defer lockStore(s.path)()
	users, err := s.loadLocked()
	if err != nil {
		return err
	}
	idx := -1
	for i, u := range users {
		if u.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return errUserNotFound
	}
	if users[idx].Role == RoleAdmin && countAdmins(users) <= 1 {
		return errLastAdmin
	}
	users = append(users[:idx], users[idx+1:]...)
	return s.saveLocked(users)
}

// RevokeCredentialUnlessLastAdmin removes credential credID from user id's Credentials
// under a SINGLE fileStoreMutex critical section -- load, last-admin check.
func (s *jsonUserStore) RevokeCredentialUnlessLastAdmin(id, credID string) error {
	defer lockStore(s.path)()
	users, err := s.loadLocked()
	if err != nil {
		return err
	}
	var target *User
	for _, u := range users {
		if u.ID == id {
			target = u
			break
		}
	}
	if target == nil {
		return errUserNotFound
	}
	idx := -1
	for i, c := range target.Credentials {
		if string(c.ID) == credID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return errCredentialNotFound
	}
	if target.Role == RoleAdmin && countAdmins(users) <= 1 && len(target.Credentials) <= 1 {
		return errLastAdminCredential
	}
	target.Credentials = append(target.Credentials[:idx], target.Credentials[idx+1:]...)
	return s.saveLocked(users)
}

// var _ UserStore = (*jsonUserStore)(nil) pins the interface implementation
// at compile time.
var _ UserStore = (*jsonUserStore)(nil)
