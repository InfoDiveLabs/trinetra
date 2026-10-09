package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// sessionCookieName is the cookie a signed-in session lives under.
const sessionCookieName = "sw_session"

// defaultSessionTTL is used whenever cfg.Web.SessionTTL is empty or fails to parse --
// belt-and-suspenders alongside config.Default()/Load(), which already backfill "24h".
const defaultSessionTTL = 24 * time.Hour

// sessionTTL parses cfg.Web.SessionTTL (time.ParseDuration syntax), falling back to
// defaultSessionTTL on a nil cfg, an empty string, or an unparseable/non-positive value.
func sessionTTL(cfg *config.Config) time.Duration {
	if cfg == nil {
		return defaultSessionTTL
	}
	d, err := time.ParseDuration(cfg.Web.SessionTTL)
	if err != nil || d <= 0 {
		return defaultSessionTTL
	}
	return d
}

// Session is a server-side session record: either a real signed-in web UI session, or --
// reusing the very same store.
type Session struct {
	// ID is the random, unguessable identifier: the sw_session cookie's value for a signed-in
	// session, or the ceremony cookie's value (sw_enroll/sw_login) for an in-flight ceremony.
	ID string `json:"id"`
	// UserID is the signed-in account's WebAuthn user handle (User.ID).
	UserID string `json:"userID"`
	// Created/Expires are Unix seconds timestamps; Expires is when this record becomes
	// eligible for eviction (Get treats it as absent, GC removes it from disk).
	Created int64 `json:"created"`
	Expires int64 `json:"expires"`
	// CSRF is this session's anti-CSRF token (requireCSRF, middleware.go), minted once by New
	// and constant for the record's lifetime.
	CSRF string `json:"csrf"`
	// Data is an opaque, caller-defined payload carried alongside a record. auth_webauthn.go's
	// registration/login ceremonies JSON-encode a *webauthn.SessionData here.
	Data []byte `json:"data,omitempty"`
}

// SessionStore is how the web package creates/looks up/invalidates session records.
type SessionStore interface {
	// New mints a fresh Session (userID may be "" for a not-yet- authenticated login ceremony)
	// with the given TTL, persists it, and returns it.
	New(userID string, ttl time.Duration) (*Session, error)
	// Get returns the session with the given ID, or (nil, false) if none exists or it has
	// expired.
	Get(id string) (*Session, bool)
	// Put persists a mutated Session (e.g. one whose Data a ceremony handler just filled in)
	// under its existing ID.
	Put(s *Session) error
	// Delete removes the session with the given ID.
	Delete(id string) error
	// GC removes every session whose Expires is <= now (Unix seconds),
	// persisting the result if anything was actually removed.
	GC(now int64)
}

// sessionMaxEntries hard-caps the authenticated-session store's size.
const sessionMaxEntries = 4096

// ceremonyMaxEntries hard-caps the SEPARATE ceremony-placeholder store (newCeremonyStore).
const ceremonyMaxEntries = 1024

// jsonSessionStore is SessionStore backed by a single JSON file (<StateDir>/sessions.json).
type jsonSessionStore struct {
	path string
	// now overrides the store's clock; nil (the production default, see newSessionStore) means
	// time.Now.
	now func() time.Time
	// maxEntries overrides the default sessionMaxEntries cap when non-zero. newCeremonyStore.
	maxEntries int
}

// capEntries returns the store's effective size cap: maxEntries when set,
// else sessionMaxEntries.
func (s *jsonSessionStore) capEntries() int {
	if s.maxEntries > 0 {
		return s.maxEntries
	}
	return sessionMaxEntries
}

// newSessionStore returns the AUTHENTICATED-session store, rooted at
// <stateDir>/sessions.json (cap sessionMaxEntries).
func newSessionStore(stateDir string) *jsonSessionStore {
	return &jsonSessionStore{path: filepath.Join(stateDir, "sessions.json")}
}

// newCeremonyStore returns the SEPARATE, independently-bounded store for in-flight WebAuthn
// ceremony placeholders (registration and login), rooted at <stateDir>/ceremonies.json.
func newCeremonyStore(stateDir string) *jsonSessionStore {
	return &jsonSessionStore{path: filepath.Join(stateDir, "ceremonies.json"), maxEntries: ceremonyMaxEntries}
}

// clock returns the store's time source, defaulting to time.Now when s.now is unset.
func (s *jsonSessionStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// loadLocked reads and parses the store file, returning (nil, nil) if it doesn't exist yet.
func (s *jsonSessionStore) loadLocked() ([]*Session, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("web: read %s: %w", s.path, err)
	}
	var sessions []*Session
	if err := json.Unmarshal(b, &sessions); err != nil {
		return nil, fmt.Errorf("web: parse %s: %w", s.path, err)
	}
	return sessions, nil
}

// saveLocked atomically rewrites the store file with sessions, tightening perms to 0600 --
// sessions.json holds session IDs, CSRF tokens, and in-flight WebAuthn ceremony data.
func (s *jsonSessionStore) saveLocked(sessions []*Session) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("web: create %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return fmt.Errorf("web: encode sessions: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("web: write %s: %w", tmp, err)
	}
	// Belt-and-suspenders: WriteFile only applies the mode when it CREATES the file.
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

// evictExpired returns sessions with every record whose Expires is <= now
// dropped, preserving order otherwise.
func evictExpired(sessions []*Session, now int64) []*Session {
	kept := sessions[:0]
	for _, sess := range sessions {
		if sess.Expires > now {
			kept = append(kept, sess)
		}
	}
	return kept
}

// New mints a fresh Session, evicting already-expired records first (see sessionMaxEntries'
// doc) and refusing.
func (s *jsonSessionStore) New(userID string, ttl time.Duration) (*Session, error) {
	id, err := newRandomID(24)
	if err != nil {
		return nil, err
	}
	csrf, err := newRandomID(24)
	if err != nil {
		return nil, err
	}
	now := s.clock()
	sess := &Session{
		ID:      id,
		UserID:  userID,
		Created: now.Unix(),
		Expires: now.Add(ttl).Unix(),
		CSRF:    csrf,
	}

	mu := fileStoreMutex(s.path)
	mu.Lock()
	defer mu.Unlock()
	sessions, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	sessions = evictExpired(sessions, now.Unix())
	if len(sessions) >= s.capEntries() {
		return nil, fmt.Errorf("web: too many active sessions; try again shortly")
	}
	sessions = append(sessions, sess)
	if err := s.saveLocked(sessions); err != nil {
		return nil, err
	}
	return sess, nil
}

// Get returns the session with the given ID, or (nil, false) if absent or
// expired per the store's clock.
func (s *jsonSessionStore) Get(id string) (*Session, bool) {
	mu := fileStoreMutex(s.path)
	mu.Lock()
	defer mu.Unlock()
	sessions, err := s.loadLocked()
	if err != nil {
		return nil, false
	}
	now := s.clock().Unix()
	for _, sess := range sessions {
		if sess.ID == id {
			if sess.Expires <= now {
				return nil, false
			}
			return sess, true
		}
	}
	return nil, false
}

// Put persists sess, replacing any existing record with the same ID or
// appending it if none matches.
func (s *jsonSessionStore) Put(sess *Session) error {
	mu := fileStoreMutex(s.path)
	mu.Lock()
	defer mu.Unlock()
	sessions, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i, existing := range sessions {
		if existing.ID == sess.ID {
			sessions[i] = sess
			return s.saveLocked(sessions)
		}
	}
	sessions = append(sessions, sess)
	return s.saveLocked(sessions)
}

// Delete removes the session with the given ID.
func (s *jsonSessionStore) Delete(id string) error {
	mu := fileStoreMutex(s.path)
	mu.Lock()
	defer mu.Unlock()
	sessions, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i, sess := range sessions {
		if sess.ID == id {
			sessions = append(sessions[:i], sessions[i+1:]...)
			return s.saveLocked(sessions)
		}
	}
	return nil
}

// GC removes every session whose Expires is <= now, saving only if that
// actually dropped something.
func (s *jsonSessionStore) GC(now int64) {
	mu := fileStoreMutex(s.path)
	mu.Lock()
	defer mu.Unlock()
	sessions, err := s.loadLocked()
	if err != nil {
		return
	}
	kept := evictExpired(sessions, now)
	if len(kept) != len(sessions) {
		_ = s.saveLocked(kept)
	}
}

// startGC runs GC on a ticker every interval until the returned stop func is called.
func (s *jsonSessionStore) startGC(interval time.Duration) (stop func()) {
	done := make(chan struct{})
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.GC(time.Now().Unix())
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

// var _ SessionStore = (*jsonSessionStore)(nil) pins the interface
// implementation at compile time.
var _ SessionStore = (*jsonSessionStore)(nil)
