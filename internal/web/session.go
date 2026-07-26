//go:build web

package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"serverwatch/internal/config"
)

// sessionCookieName is the cookie a signed-in session lives under (Task
// 5/#61's login ceremony sets it; see session.go's setSessionCookie /
// middleware.go's sessionMiddleware, which reads it back on every request).
const sessionCookieName = "sw_session"

// defaultSessionTTL is used whenever cfg.Web.SessionTTL is empty or fails to
// parse — belt-and-suspenders alongside config.Default()/Load(), which
// already backfill "24h" (internal/config's Config.Default), for any caller
// that builds a bare *config.Config{} directly (e.g. a test).
const defaultSessionTTL = 24 * time.Hour

// sessionTTL parses cfg.Web.SessionTTL (time.ParseDuration syntax), falling
// back to defaultSessionTTL on a nil cfg, an empty string, or an
// unparseable/non-positive value.
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

// Session is a server-side session record: either a real signed-in web UI
// session, or — reusing the very same store — a short-lived placeholder
// stashing an in-flight WebAuthn ceremony's data between its /begin and
// /finish requests (see auth_webauthn.go's beginRegistration/beginLogin,
// which replaced Task 4's temporary in-memory ceremonyStash with this store;
// TODO(#61), now resolved).
type Session struct {
	// ID is the random, unguessable identifier: the sw_session cookie's
	// value for a signed-in session, or the ceremony cookie's value
	// (sw_enroll/sw_login) for an in-flight ceremony.
	ID string `json:"id"`
	// UserID is the signed-in account's WebAuthn user handle (User.ID).
	// Empty for a login ceremony (the user isn't known until the assertion's
	// userHandle resolves it — see auth_webauthn.go's finishLogin).
	UserID string `json:"userID"`
	// Created/Expires are Unix seconds timestamps; Expires is when this
	// record becomes eligible for eviction (Get treats it as absent, GC
	// removes it from disk).
	Created int64 `json:"created"`
	Expires int64 `json:"expires"`
	// CSRF is this session's anti-CSRF token (requireCSRF, middleware.go),
	// minted once by New and constant for the record's lifetime. A ceremony
	// placeholder has one too (harmless — nothing ever checks it) since New
	// always mints it.
	CSRF string `json:"csrf"`
	// Data is an opaque, caller-defined payload carried alongside a record.
	// auth_webauthn.go's registration/login ceremonies JSON-encode a
	// *webauthn.SessionData here (plus, for registration, the not-yet-
	// persisted pending *User) between /begin and /finish; an ordinary
	// signed-in session leaves it nil.
	Data []byte `json:"data,omitempty"`
}

// SessionStore is how the web package creates/looks up/invalidates session
// records — both real signed-in sessions and the short-lived WebAuthn
// ceremony placeholders described on Session.Data. The only implementation
// is jsonSessionStore, below.
type SessionStore interface {
	// New mints a fresh Session (userID may be "" for a not-yet-
	// authenticated login ceremony) with the given TTL, persists it, and
	// returns it.
	New(userID string, ttl time.Duration) (*Session, error)
	// Get returns the session with the given ID, or (nil, false) if none
	// exists or it has expired (an expired record is treated as absent, not
	// actively removed here — GC does that).
	Get(id string) (*Session, bool)
	// Put persists a mutated Session (e.g. one whose Data a ceremony
	// handler just filled in) under its existing ID, replacing any prior
	// record with that ID or inserting it if absent.
	Put(s *Session) error
	// Delete removes the session with the given ID. Deleting an ID that
	// isn't present is a no-op, not an error — logout and one-shot ceremony
	// consumption may harmlessly race a GC sweep or a repeat call.
	Delete(id string) error
	// GC removes every session whose Expires is <= now (Unix seconds),
	// persisting the result if anything was actually removed.
	GC(now int64)
}

// sessionMaxEntries hard-caps the authenticated-session store's size. New
// refuses to mint another record once the store (after evicting anything
// already expired) is at this cap; the number of users actually signed in at
// once sits far below it.
//
// CRITICAL: unauthenticated WebAuthn ceremony placeholders do NOT count
// against this cap — they live in a SEPARATE store instance
// (newCeremonyStore, bounded by ceremonyMaxEntries) precisely so that a
// pre-auth flood of /login/begin or /enroll/begin can never fill this store
// and cause finishLogin's post-assertion sessions.New to refuse a user
// presenting a valid passkey (an availability bug). The two stores share
// this type but nothing else — separate files, separate caps.
const sessionMaxEntries = 4096

// ceremonyMaxEntries hard-caps the SEPARATE ceremony-placeholder store
// (newCeremonyStore) — the bound Task 4's temporary in-memory ceremonyStash
// enforced with its own same-named constant, now applied to the file-backed
// store that replaced it (issue #61). Because /login/begin and /enroll/begin
// are unauthenticated, an attacker can flood them; when this store fills,
// only further ceremony begins are refused — real, authenticated sessions
// (a different store, above) are unaffected. A single interactive ceremony
// is one short-lived (ceremonyTTL) record, so this ceiling sits far above
// any honest concurrency.
const ceremonyMaxEntries = 1024

// jsonSessionStore is SessionStore backed by a single JSON file
// (<StateDir>/sessions.json). Like jsonUserStore, it does not cache parsed
// sessions in memory between calls: every method reloads from disk under the
// shared per-path lock (fileStoreMutex, users.go), so New/Put/Delete/GC's
// read-modify-write can't race a concurrent goroutine within this process —
// including one holding a DIFFERENT jsonSessionStore instance over the same
// file, which the handlers create per request (newSessionStore/
// newCeremonyStore). A second OS process editing the file concurrently is out
// of scope, same as jsonUserStore.
type jsonSessionStore struct {
	path string
	// now overrides the store's clock; nil (the production default, see
	// newSessionStore) means time.Now. Tests set this directly to drive
	// expiry/GC deterministically, mirroring auth_webauthn.go's ceremonyStash.
	now func() time.Time
	// maxEntries overrides the default sessionMaxEntries cap when non-zero.
	// newCeremonyStore sets it to ceremonyMaxEntries so the ceremony-
	// placeholder store is bounded independently of the authenticated-
	// session store; tests also set a small cap to exercise the refusal
	// path without thousands of real (O(n) read-modify-write) disk round
	// trips.
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
// <stateDir>/sessions.json (cap sessionMaxEntries). Like newUserStore, this
// touches no disk until an operation is actually performed.
func newSessionStore(stateDir string) *jsonSessionStore {
	return &jsonSessionStore{path: filepath.Join(stateDir, "sessions.json")}
}

// newCeremonyStore returns the SEPARATE, independently-bounded store for
// in-flight WebAuthn ceremony placeholders (registration and login),
// rooted at <stateDir>/ceremonies.json (cap ceremonyMaxEntries). Keeping
// these out of the authenticated-session store is what stops a pre-auth
// ceremony flood from starving real logins — see sessionMaxEntries' doc.
func newCeremonyStore(stateDir string) *jsonSessionStore {
	return &jsonSessionStore{path: filepath.Join(stateDir, "ceremonies.json"), maxEntries: ceremonyMaxEntries}
}

// clock returns the store's time source, defaulting to time.Now when s.now
// is unset.
func (s *jsonSessionStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// loadLocked reads and parses the store file, returning (nil, nil) if it
// doesn't exist yet. Callers must hold the store's fileStoreMutex.
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

// saveLocked atomically rewrites the store file with sessions, tightening
// perms to 0600 — sessions.json holds session IDs, CSRF tokens, and
// in-flight WebAuthn ceremony data, all of which are bearer-equivalent or
// otherwise sensitive, so unlike users.json this file must never be
// group/world-readable even transiently. Atomic (write-temp + rename) so a
// crash mid-write can't leave a truncated/corrupt file behind, mirroring
// jsonUserStore.saveLocked/internal/config.Config.Save. Callers must hold
// s.mu.
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
	// Belt-and-suspenders: WriteFile only applies the mode when it CREATES
	// the file, so an explicit Chmod re-tightens perms even if a stale tmp
	// from a previous crash already existed with wider ones.
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

// New mints a fresh Session, evicting already-expired records first (see
// sessionMaxEntries' doc) and refusing (error) if the store is still at its
// size cap once that eviction is done.
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

// Delete removes the session with the given ID. Absent is a no-op (see the
// SessionStore doc).
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

// startGC runs GC on a ticker every interval until the returned stop func is
// called. Start (server.go) runs this alongside the bound listener for the
// lifetime of the web server, per this task's "GC on a ticker" requirement.
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
