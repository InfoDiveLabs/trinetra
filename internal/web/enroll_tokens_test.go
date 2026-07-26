//go:build web

package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTokenStoreIssueRedeemRoundTrip pins the core contract: a token Issue
// mints must Redeem back to the same Role exactly once.
func TestTokenStoreIssueRedeemRoundTrip(t *testing.T) {
	store := newTokenStore(t.TempDir())
	tok := store.Issue(RoleViewer, time.Hour)
	if tok == "" {
		t.Fatal("Issue returned an empty token")
	}

	role, err := store.Redeem(tok)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if role != RoleViewer {
		t.Errorf("Redeem role = %q, want %q", role, RoleViewer)
	}
}

// TestTokenStoreRedeemIsSingleUse pins single-use enforcement: a second
// Redeem of the same token must fail even though it hasn't expired.
func TestTokenStoreRedeemIsSingleUse(t *testing.T) {
	store := newTokenStore(t.TempDir())
	tok := store.Issue(RoleAdmin, time.Hour)

	if _, err := store.Redeem(tok); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	if _, err := store.Redeem(tok); err == nil {
		t.Fatal("second Redeem of the same token = nil error, want rejection (single-use)")
	}
}

// TestTokenStoreRedeemUnknownFails pins the "no such token" case: a value
// that was never Issued must be rejected, not silently treated as some
// default role.
func TestTokenStoreRedeemUnknownFails(t *testing.T) {
	store := newTokenStore(t.TempDir())
	if _, err := store.Redeem("not-a-real-token"); err == nil {
		t.Fatal("Redeem(unknown) = nil error, want rejection")
	}
}

// TestTokenStoreRedeemExpiredFails pins expiry: a token whose TTL has
// elapsed must be rejected even though it was never used, using a
// clock-controlled store (mirroring jsonSessionStore's `now` field pattern
// in session_test.go) so the test doesn't need a real sleep.
func TestTokenStoreRedeemExpiredFails(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &tokenStore{path: filepath.Join(t.TempDir(), "enroll_tokens.json"), now: func() time.Time { return now }}

	tok := store.Issue(RoleViewer, time.Minute)
	now = now.Add(2 * time.Minute)

	if _, err := store.Redeem(tok); err == nil {
		t.Fatal("Redeem(expired) = nil error, want rejection")
	}
}

// TestTokenStorePersistsWith0600Perms pins the design doc's security
// checklist expectation: a bearer-equivalent enrollment token file must not
// be group/world-readable, same as sessions.json/users.json.
func TestTokenStorePersistsWith0600Perms(t *testing.T) {
	dir := t.TempDir()
	store := newTokenStore(dir)
	if tok := store.Issue(RoleViewer, time.Hour); tok == "" {
		t.Fatal("Issue returned an empty token")
	}

	fi, err := os.Stat(filepath.Join(dir, "enroll_tokens.json"))
	if err != nil {
		t.Fatalf("stat enroll_tokens.json: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("enroll_tokens.json perms = %o, want 0600", got)
	}
}

// TestTokenStoreGCRemovesExpiredRecords pins that GC actually rewrites the
// file without expired entries (used or not), distinct from Redeem's lazy
// (non-mutating on failure) expiry check.
func TestTokenStoreGCRemovesExpiredRecords(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &tokenStore{path: filepath.Join(t.TempDir(), "enroll_tokens.json"), now: func() time.Time { return now }}

	stale := store.Issue(RoleViewer, time.Second)
	now = now.Add(time.Hour)
	fresh := store.Issue(RoleAdmin, time.Hour)

	store.GC(now.Unix())

	toks, err := store.loadLocked()
	if err != nil {
		t.Fatalf("loadLocked: %v", err)
	}
	for _, tk := range toks {
		if tk.Token == stale {
			t.Errorf("GC left expired token %q on disk", stale)
		}
	}
	found := false
	for _, tk := range toks {
		if tk.Token == fresh {
			found = true
		}
	}
	if !found {
		t.Error("GC removed the still-live token")
	}
}

// TestResolveEnrollRoleBootstrapsFirstUserAsAdmin pins the first-run
// bootstrap rule directly against resolveEnrollRole: an empty store with no
// token supplied resolves to RoleAdmin.
func TestResolveEnrollRoleBootstrapsFirstUserAsAdmin(t *testing.T) {
	dir := t.TempDir()
	tokens := newTokenStore(dir)
	users := newUserStore(dir)

	role, err := resolveEnrollRole(tokens, users, "")
	if err != nil {
		t.Fatalf("resolveEnrollRole(bootstrap): %v", err)
	}
	if role != RoleAdmin {
		t.Errorf("bootstrap role = %q, want %q", role, RoleAdmin)
	}
}

// TestResolveEnrollRoleClosedAfterBootstrap pins the flip side: once a user
// exists, an empty token must be refused rather than silently granted
// RoleViewer.
func TestResolveEnrollRoleClosedAfterBootstrap(t *testing.T) {
	dir := t.TempDir()
	tokens := newTokenStore(dir)
	users := newUserStore(dir)
	if err := users.Put(&User{ID: "u1", Name: "admin", Role: RoleAdmin, Created: 1}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	if _, err := resolveEnrollRole(tokens, users, ""); err == nil {
		t.Fatal("resolveEnrollRole(no token, users exist) = nil error, want rejection (enrollment closed)")
	}
}

// TestResolveEnrollRoleHonorsValidToken pins that a valid token's role wins
// even when the store already has users (the normal post-bootstrap path).
func TestResolveEnrollRoleHonorsValidToken(t *testing.T) {
	dir := t.TempDir()
	tokens := newTokenStore(dir)
	users := newUserStore(dir)
	if err := users.Put(&User{ID: "u1", Name: "admin", Role: RoleAdmin, Created: 1}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	tok := tokens.Issue(RoleViewer, time.Hour)
	role, err := resolveEnrollRole(tokens, users, tok)
	if err != nil {
		t.Fatalf("resolveEnrollRole(valid token): %v", err)
	}
	if role != RoleViewer {
		t.Errorf("role = %q, want %q", role, RoleViewer)
	}
}
