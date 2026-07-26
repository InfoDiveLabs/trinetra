//go:build web

package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// --- HTTP-level bootstrap/token enrollment flow. These drive the full
// /enroll/begin + /enroll/finish surface (using auth_webauthn_test.go's
// virtual-authenticator fixtures for the ceremonies that need to actually
// persist a user to check its resulting Role), pinning that routes.go's
// enrollBeginHandler wiring of resolveEnrollRole actually behaves per the
// design doc's first-run-bootstrap/enrollment-token rules end-to-end, not
// just at the resolveEnrollRole unit level above.

// TestEnrollBootstrapFirstUserBecomesAdmin drives the full HTTP /enroll
// surface (no token) against a fresh, empty StateDir: the resulting FIRST
// account must be admin, per the design doc's first-run bootstrap rule.
func TestEnrollBootstrapFirstUserBecomesAdmin(t *testing.T) {
	d := enrollTestDeps(t)
	h := newHandler(d)

	beginBody, _ := json.Marshal(map[string]string{"name": "first-admin"})
	beginReq := httptest.NewRequest(http.MethodPost, "/enroll/begin", bytes.NewReader(beginBody))
	beginRR := httptest.NewRecorder()
	h.ServeHTTP(beginRR, beginReq)
	if beginRR.Code != http.StatusOK {
		t.Fatalf("POST /enroll/begin (bootstrap) status = %d, want 200, body: %s", beginRR.Code, beginRR.Body.String())
	}

	var beginResp struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(beginRR.Body.Bytes(), &beginResp); err != nil {
		t.Fatalf("decode begin response: %v", err)
	}
	cookie := cookieFrom(t, beginRR, enrollSessionCookie)

	finishBody, _ := creationResponseBody(t, beginResp.PublicKey.Challenge, "http://example.com", "example.com")
	finishReq := httptest.NewRequest(http.MethodPost, "/enroll/finish", bytes.NewReader(finishBody))
	finishReq.AddCookie(cookie)
	finishRR := httptest.NewRecorder()
	h.ServeHTTP(finishRR, finishReq)
	if finishRR.Code != http.StatusNoContent {
		t.Fatalf("POST /enroll/finish (bootstrap) status = %d, want 204, body: %s", finishRR.Code, finishRR.Body.String())
	}

	store := newUserStore(d.StateDir)
	u, ok := store.ByName("first-admin")
	if !ok {
		t.Fatal("bootstrap user not found after enroll")
	}
	if u.Role != RoleAdmin {
		t.Errorf("bootstrap user role = %q, want %q", u.Role, RoleAdmin)
	}
}

// TestEnrollWithoutTokenClosedAfterBootstrap pins the flip side: once a
// first user exists, a second tokenless /enroll/begin must be refused
// outright — no ceremony cookie set, no account created.
func TestEnrollWithoutTokenClosedAfterBootstrap(t *testing.T) {
	d := enrollTestDeps(t)
	store := newUserStore(d.StateDir)
	if err := store.Put(&User{ID: mustNewUserID(t), Name: "admin", Role: RoleAdmin, Created: 1}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	h := newHandler(d)
	beginBody, _ := json.Marshal(map[string]string{"name": "second-user"})
	req := httptest.NewRequest(http.MethodPost, "/enroll/begin", bytes.NewReader(beginBody))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST /enroll/begin (no token, post-bootstrap) status = %d, want 403, body: %s", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == enrollSessionCookie && c.Value != "" {
			t.Errorf("refused enrollment set a ceremony cookie %q", c.Value)
		}
	}
	if _, ok := store.ByName("second-user"); ok {
		t.Error("refused enrollment created an account anyway")
	}
}

// TestEnrollWithValidTokenAssignsTokenRole pins the invited path: a valid
// token's Role wins over any default, even post-bootstrap.
func TestEnrollWithValidTokenAssignsTokenRole(t *testing.T) {
	d := enrollTestDeps(t)
	store := newUserStore(d.StateDir)
	if err := store.Put(&User{ID: mustNewUserID(t), Name: "admin", Role: RoleAdmin, Created: 1}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	tok := newTokenStore(d.StateDir).Issue(RoleViewer, time.Hour)
	if tok == "" {
		t.Fatal("Issue returned an empty token")
	}

	h := newHandler(d)
	beginBody, _ := json.Marshal(map[string]string{"name": "invited-viewer", "token": tok})
	beginReq := httptest.NewRequest(http.MethodPost, "/enroll/begin", bytes.NewReader(beginBody))
	beginRR := httptest.NewRecorder()
	h.ServeHTTP(beginRR, beginReq)
	if beginRR.Code != http.StatusOK {
		t.Fatalf("POST /enroll/begin (valid token) status = %d, want 200, body: %s", beginRR.Code, beginRR.Body.String())
	}

	var beginResp struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(beginRR.Body.Bytes(), &beginResp); err != nil {
		t.Fatalf("decode begin response: %v", err)
	}
	cookie := cookieFrom(t, beginRR, enrollSessionCookie)

	finishBody, _ := creationResponseBody(t, beginResp.PublicKey.Challenge, "http://example.com", "example.com")
	finishReq := httptest.NewRequest(http.MethodPost, "/enroll/finish", bytes.NewReader(finishBody))
	finishReq.AddCookie(cookie)
	finishRR := httptest.NewRecorder()
	h.ServeHTTP(finishRR, finishReq)
	if finishRR.Code != http.StatusNoContent {
		t.Fatalf("POST /enroll/finish (valid token) status = %d, want 204, body: %s", finishRR.Code, finishRR.Body.String())
	}

	u, ok := store.ByName("invited-viewer")
	if !ok {
		t.Fatal("invited user not found after enroll")
	}
	if u.Role != RoleViewer {
		t.Errorf("invited user role = %q, want %q", u.Role, RoleViewer)
	}
}

// TestEnrollTokenIsSingleUseAtHTTPLayer pins that a token consumed by one
// successful /enroll/begin cannot be reused by a second one.
func TestEnrollTokenIsSingleUseAtHTTPLayer(t *testing.T) {
	d := enrollTestDeps(t)
	store := newUserStore(d.StateDir)
	if err := store.Put(&User{ID: mustNewUserID(t), Name: "admin", Role: RoleAdmin, Created: 1}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	tok := newTokenStore(d.StateDir).Issue(RoleViewer, time.Hour)

	h := newHandler(d)
	firstBody, _ := json.Marshal(map[string]string{"name": "first-invitee", "token": tok})
	firstReq := httptest.NewRequest(http.MethodPost, "/enroll/begin", bytes.NewReader(firstBody))
	firstRR := httptest.NewRecorder()
	h.ServeHTTP(firstRR, firstReq)
	if firstRR.Code != http.StatusOK {
		t.Fatalf("first POST /enroll/begin with token status = %d, want 200, body: %s", firstRR.Code, firstRR.Body.String())
	}

	secondBody, _ := json.Marshal(map[string]string{"name": "second-invitee", "token": tok})
	secondReq := httptest.NewRequest(http.MethodPost, "/enroll/begin", bytes.NewReader(secondBody))
	secondRR := httptest.NewRecorder()
	h.ServeHTTP(secondRR, secondReq)
	if secondRR.Code != http.StatusForbidden {
		t.Fatalf("second POST /enroll/begin reusing the same token status = %d, want 403, body: %s", secondRR.Code, secondRR.Body.String())
	}
}

// TestEnrollExpiredTokenRejected pins that an already-expired token is
// refused at /enroll/begin the same way an unknown one would be.
func TestEnrollExpiredTokenRejected(t *testing.T) {
	d := enrollTestDeps(t)
	store := newUserStore(d.StateDir)
	if err := store.Put(&User{ID: mustNewUserID(t), Name: "admin", Role: RoleAdmin, Created: 1}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	tok := newTokenStore(d.StateDir).Issue(RoleViewer, -time.Minute) // already expired

	h := newHandler(d)
	body, _ := json.Marshal(map[string]string{"name": "too-late", "token": tok})
	req := httptest.NewRequest(http.MethodPost, "/enroll/begin", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST /enroll/begin with expired token status = %d, want 403, body: %s", rr.Code, rr.Body.String())
	}
}
