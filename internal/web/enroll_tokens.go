package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// EnrollToken is a single admin-issued, single-use, expiring invitation to
// enroll a new web UI account. /enroll?token=... (routes.go's
// enrollPageHandler/enrollBeginHandler) redeems it via tokenStore.Redeem,
// and the Role it carries is what the resulting NEW account is created with
// (see resolveEnrollRole, below). Task 7's user management page is what
// actually issues these (tokenStore.Issue); this task only wires the store
// and the /enroll gate that consumes them.
type EnrollToken struct {
	// Token is the opaque, unguessable value carried in the /enroll?token=
	// query string and the /enroll/begin request body's "token" field.
	Token string `json:"token"`
	// Role is the account role a successful Redeem of this token grants.
	Role Role `json:"role"`
	// Expires is the Unix seconds timestamp after which Redeem refuses this
	// token even if it was never used.
	Expires int64 `json:"expires"`
	// Used marks a token permanently spent: Redeem sets this on its first
	// (successful) call and refuses every call thereafter, regardless of
	// Expires -- a token is single-use even within its TTL.
	Used bool `json:"used"`
}

// tokenStore is a file-backed (<StateDir>/enroll_tokens.json) enrollment
// token store, structured the same way as jsonUserStore/jsonSessionStore: no
// in-memory cache, every method reloads from disk under the shared per-path
// lock (fileStoreMutex, users.go) so Issue/Redeem's read-modify-write can't
// race a concurrent goroutine within this process -- including one holding a
// DIFFERENT tokenStore instance over the same file, which the handlers create
// per request (a second OS process editing the file concurrently is out of
// scope, same caveat as the other two stores).
type tokenStore struct {
	path string
	// now overrides the store's clock; nil (the production default) means
	// time.Now. Tests set this directly to drive expiry deterministically,
	// mirroring jsonSessionStore's own `now` field.
	now func() time.Time
}

// newTokenStore returns a tokenStore rooted at <stateDir>/enroll_tokens.json.
// Like newUserStore/newSessionStore, this touches no disk until an
// operation is actually performed.
func newTokenStore(stateDir string) *tokenStore {
	return &tokenStore{path: filepath.Join(stateDir, "enroll_tokens.json")}
}

// clock returns the store's time source, defaulting to time.Now when s.now
// is unset.
func (s *tokenStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// loadLocked reads and parses the store file, returning (nil, nil) if it
// doesn't exist yet (a fresh install/StateDir, or one where no token has
// ever been issued). Callers must hold the store's fileStoreMutex.
func (s *tokenStore) loadLocked() ([]*EnrollToken, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("web: read %s: %w", s.path, err)
	}
	var toks []*EnrollToken
	if err := json.Unmarshal(b, &toks); err != nil {
		return nil, fmt.Errorf("web: parse %s: %w", s.path, err)
	}
	return toks, nil
}

// saveLocked atomically rewrites the store file with toks, tightening perms
// to 0600 -- an enrollment token is bearer-equivalent (whoever holds the
// string can mint an account at the role it carries), so this file must
// never be group/world-readable, the same reasoning as sessions.json.
// Atomic (write-temp + rename) so a crash mid-write can never leave a
// truncated/corrupt file behind, mirroring jsonUserStore/jsonSessionStore's
// own saveLocked. Callers must hold the store's fileStoreMutex.
func (s *tokenStore) saveLocked(toks []*EnrollToken) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("web: create %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(toks, "", "  ")
	if err != nil {
		return fmt.Errorf("web: encode enrollment tokens: %w", err)
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

// Issue mints a fresh, single-use enrollment token granting role, valid for
// ttl, and persists it, returning the token string. The design doc's
// interface fixes this signature to return just the string (no error): a
// (rare, e.g. disk-full/permissions) persistence failure is swallowed and
// Issue returns "" instead -- Redeem("") always fails with "unknown
// enrollment token" like any other bogus value, so a caller that somehow
// hits this can't mint a usable token, but nothing unsafe results either.
func (s *tokenStore) Issue(role Role, ttl time.Duration) string {
	tok, err := newRandomID(24)
	if err != nil {
		return ""
	}
	now := s.clock()
	et := &EnrollToken{Token: tok, Role: role, Expires: now.Add(ttl).Unix()}

	mu := fileStoreMutex(s.path)
	mu.Lock()
	defer mu.Unlock()
	toks, err := s.loadLocked()
	if err != nil {
		return ""
	}
	toks = append(toks, et)
	if err := s.saveLocked(toks); err != nil {
		return ""
	}
	return tok
}

// Redeem looks up tok and, if it exists, is unexpired, and hasn't already
// been used, marks it Used and returns its Role. Every other case -- unknown
// token, expired, or already used -- returns an error and leaves the store
// untouched.
func (s *tokenStore) Redeem(tok string) (Role, error) {
	mu := fileStoreMutex(s.path)
	mu.Lock()
	defer mu.Unlock()
	toks, err := s.loadLocked()
	if err != nil {
		return "", err
	}
	now := s.clock().Unix()
	for _, t := range toks {
		if t.Token != tok {
			continue
		}
		if t.Used {
			return "", fmt.Errorf("web: enrollment token already used")
		}
		if t.Expires <= now {
			return "", fmt.Errorf("web: enrollment token expired")
		}
		t.Used = true
		if err := s.saveLocked(toks); err != nil {
			return "", err
		}
		return t.Role, nil
	}
	return "", fmt.Errorf("web: unknown enrollment token")
}

// GC removes every token whose Expires is <= now, whether or not it was ever
// used -- the same eager disk-space-reclaim role as SessionStore.GC,
// decoupled from Redeem's own immediate (lazy) expiry/used check.
func (s *tokenStore) GC(now int64) {
	mu := fileStoreMutex(s.path)
	mu.Lock()
	defer mu.Unlock()
	toks, err := s.loadLocked()
	if err != nil {
		return
	}
	kept := toks[:0]
	for _, t := range toks {
		if t.Expires > now {
			kept = append(kept, t)
		}
	}
	if len(kept) != len(toks) {
		_ = s.saveLocked(kept)
	}
}

// startGC runs GC on a ticker every interval until the returned stop func is
// called, mirroring jsonSessionStore.startGC. server.go's Start wires this
// up alongside the session/ceremony GC tickers.
func (s *tokenStore) startGC(interval time.Duration) (stop func()) {
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

// resolveEnrollRole decides, at /enroll/begin, how a NEW registration should
// proceed:
//
//   - a non-blank token: role is whatever tokens.Redeem(token) grants (Redeem
//     enforces unknown/expired/used rejection, and burns the single-use
//     token now); bootstrap is false. The role is final.
//   - no token, and the user store has no accounts yet: this is a first-run
//     bootstrap ATTEMPT -- bootstrap is true and role is left empty. The
//     account's admin role is NOT decided here: two concurrent tokenless
//     begins would both see an empty store, so the authoritative "0 users ⇒
//     admin, else refuse" decision is deferred to finish time
//     (jsonUserStore.CreateFirstAdmin, under the write lock). This begin-time
//     check is only a fast-fail for the obviously-closed case below.
//   - no token, and at least one account already exists: an error. Open,
//     tokenless enrollment is only ever valid for the very first account;
//     every subsequent one needs an admin-issued invite.
//
// A true bootstrap result is threaded through the ceremony (regCeremonyData.
// Bootstrap) so finishRegistration knows to route through CreateFirstAdmin
// rather than a plain Put.
func resolveEnrollRole(tokens *tokenStore, users UserStore, token string) (role Role, bootstrap bool, err error) {
	if token != "" {
		r, err := tokens.Redeem(token)
		return r, false, err
	}
	if len(users.List()) == 0 {
		return "", true, nil
	}
	return "", false, fmt.Errorf("web: enrollment is closed; an admin-issued invite token is required")
}
