package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestTokenStoreConcurrentIssueNoLostUpdate is the shared-per-path-lock
// regression pin for enrollment tokens: like sessions, each handler builds a
// fresh tokenStore per request (newTokenStore), so concurrent Issue calls
// through separate instances on the same file must all persist -- a
// per-instance mutex would let two Issues load→append→save the whole file,
// last-writer-wins, silently dropping a token (and colliding on the shared
// .tmp path). With fileStoreMutex they serialize and every token redeems.
func TestTokenStoreConcurrentIssueNoLostUpdate(t *testing.T) {
	dir := t.TempDir()
	const n = 8

	var wg sync.WaitGroup
	start := make(chan struct{})
	toks := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			toks[i] = newTokenStore(dir).Issue(RoleViewer, time.Hour)
		}(i)
	}
	close(start)
	wg.Wait()

	redeemer := newTokenStore(dir)
	for i, tok := range toks {
		if tok == "" {
			t.Fatalf("Issue[%d] returned an empty token", i)
		}
		if _, err := redeemer.Redeem(tok); err != nil {
			t.Errorf("token %d was lost or corrupted -- Redeem: %v", i, err)
		}
	}
}

// TestTokenStoreConcurrentRedeemSingleUse pins single-use enforcement under
// concurrency: many goroutines racing to redeem the SAME token (each via its
// own tokenStore instance) must yield exactly one success -- the shared
// per-path lock serializes the load→mark-used→save so no two callers can both
// observe it unused and both consume it.
func TestTokenStoreConcurrentRedeemSingleUse(t *testing.T) {
	dir := t.TempDir()
	tok := newTokenStore(dir).Issue(RoleAdmin, time.Hour)
	if tok == "" {
		t.Fatal("Issue returned an empty token")
	}
	const n = 8

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = newTokenStore(dir).Redeem(tok)
		}(i)
	}
	close(start)
	wg.Wait()

	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Errorf("concurrent Redeem successes = %d, want exactly 1 (single-use)", successes)
	}
}

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
// token supplied resolves to a bootstrap attempt (role deferred to finish,
// not decided here -- see the TOCTOU fix), not a rejection.
func TestResolveEnrollRoleBootstrapsFirstUserAsAdmin(t *testing.T) {
	dir := t.TempDir()
	tokens := newTokenStore(dir)
	users := newUserStore(dir)

	role, bootstrap, err := resolveEnrollRole(tokens, users, "")
	if err != nil {
		t.Fatalf("resolveEnrollRole(bootstrap): %v", err)
	}
	if !bootstrap {
		t.Error("resolveEnrollRole(empty store, no token) bootstrap = false, want true")
	}
	// Role is intentionally NOT decided at begin for a bootstrap attempt;
	// CreateFirstAdmin forces RoleAdmin atomically at finish time.
	if role != "" {
		t.Errorf("bootstrap role = %q, want \"\" (decided at finish)", role)
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

	if _, _, err := resolveEnrollRole(tokens, users, ""); err == nil {
		t.Fatal("resolveEnrollRole(no token, users exist) = nil error, want rejection (enrollment closed)")
	}
}

// TestResolveEnrollRoleFailsClosedOnUnreadableStore pins the #105 secondary
// hardening: an UNREADABLE user store (corrupt/partially-written users.json,
// I/O error) must NOT be treated as a genuine empty first-run store. If it
// were, an attacker could tokenlessly bootstrap an admin during any window
// the store is unreadable. resolveEnrollRole must fail closed -- return an
// error, never bootstrap=true -- rather than trust List() swallowing the read
// error into an empty slice.
func TestResolveEnrollRoleFailsClosedOnUnreadableStore(t *testing.T) {
	dir := t.TempDir()
	tokens := newTokenStore(dir)
	users := newUserStore(dir)

	// Corrupt the users store so it exists but cannot be read as valid JSON.
	// (An absent file is the genuine first-run case and must still bootstrap;
	// a present-but-unreadable file is the takeover window we close here.)
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt store: %v", err)
	}

	role, bootstrap, err := resolveEnrollRole(tokens, users, "")
	if err == nil {
		t.Fatal("resolveEnrollRole(unreadable store, no token) = nil error, want fail-closed rejection")
	}
	if bootstrap {
		t.Error("resolveEnrollRole(unreadable store) bootstrap = true, want false (no tokenless bootstrap on unreadable store)")
	}
	if role != "" {
		t.Errorf("role = %q, want \"\" on fail-closed", role)
	}
}

// TestResolveEnrollRoleHonorsValidToken pins that a valid token's role wins
// even when the store already has users (the normal post-bootstrap path),
// and that it is NOT a bootstrap attempt.
func TestResolveEnrollRoleHonorsValidToken(t *testing.T) {
	dir := t.TempDir()
	tokens := newTokenStore(dir)
	users := newUserStore(dir)
	if err := users.Put(&User{ID: "u1", Name: "admin", Role: RoleAdmin, Created: 1}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	tok := tokens.Issue(RoleViewer, time.Hour)
	role, bootstrap, err := resolveEnrollRole(tokens, users, tok)
	if err != nil {
		t.Fatalf("resolveEnrollRole(valid token): %v", err)
	}
	if bootstrap {
		t.Error("resolveEnrollRole(valid token) bootstrap = true, want false")
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
// outright -- no ceremony cookie set, no account created.
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

// enrollBeginRR runs POST /enroll/begin for name (no token) and returns the
// challenge + ceremony cookie, failing the test on a non-200.
func enrollBeginRR(t *testing.T, h http.Handler, name string) (challenge string, cookie *http.Cookie) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/enroll/begin", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /enroll/begin(%q) status = %d, want 200, body: %s", name, rr.Code, rr.Body.String())
	}
	var resp struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode begin(%q): %v", name, err)
	}
	return resp.PublicKey.Challenge, cookieFrom(t, rr, enrollSessionCookie)
}

// enrollFinishRR completes an enrollment for a challenge/cookie pair from
// enrollBeginRR and returns the finish status code.
func enrollFinishRR(t *testing.T, h http.Handler, challenge string, cookie *http.Cookie) int {
	t.Helper()
	body, _ := creationResponseBody(t, challenge, "http://example.com", "example.com")
	req := httptest.NewRequest(http.MethodPost, "/enroll/finish", bytes.NewReader(body))
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

// TestBootstrapRaceYieldsExactlyOneAdmin is the TOCTOU regression pin: two
// tokenless enrollments that BOTH begin against an empty store (each reads
// zero users, each is a bootstrap attempt) must not both become admin. The
// admin-or-refuse decision is made atomically at finish time
// (jsonUserStore.CreateFirstAdmin, under the write lock), so whichever finish
// commits first becomes the sole admin and the second -- now seeing a
// non-empty store -- is rejected. This reproduces the reviewer's live repro
// (two /enroll/begin, then two /enroll/finish) and asserts exactly one
// account exists afterward and it is admin.
func TestBootstrapRaceYieldsExactlyOneAdmin(t *testing.T) {
	d := enrollTestDeps(t)
	h := newHandler(d)

	// Both begins run FIRST, against the still-empty store -- this is the
	// window where the old (begin-time) decision made both admin.
	ch1, cookie1 := enrollBeginRR(t, h, "racer-1")
	ch2, cookie2 := enrollBeginRR(t, h, "racer-2")

	// Now both finishes. The first to commit wins the sole admin slot.
	code1 := enrollFinishRR(t, h, ch1, cookie1)
	code2 := enrollFinishRR(t, h, ch2, cookie2)

	if code1 != http.StatusNoContent {
		t.Fatalf("first bootstrap finish status = %d, want 204", code1)
	}
	if code2 == http.StatusNoContent {
		t.Fatal("second bootstrap finish succeeded (204); want rejection -- a second silent admin is exactly the TOCTOU bug")
	}

	store := newUserStore(d.StateDir)
	all := store.List()
	if len(all) != 1 {
		t.Fatalf("store has %d accounts after the bootstrap race, want exactly 1", len(all))
	}
	if all[0].Role != RoleAdmin {
		t.Errorf("sole bootstrap account role = %q, want %q", all[0].Role, RoleAdmin)
	}
	if all[0].Name != "racer-1" {
		t.Errorf("sole account name = %q, want racer-1 (the finish that won the lock)", all[0].Name)
	}
}
