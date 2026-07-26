//go:build web

package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// enrollTestDeps mirrors testDeps (server_test.go) but points StateDir at a
// fresh temp dir, since these tests exercise the real newUserStore-backed
// enroll handlers (unlike testDeps' callers, which never reach /enroll/*).
func enrollTestDeps(t *testing.T) Deps {
	t.Helper()
	d := testDeps(t)
	d.StateDir = t.TempDir()
	return d
}

// TestEnrollPageRendersBareLayout pins GET /enroll: it must render through
// the bare/centered layout (base_bare.html), not the app shell — no
// sidebar/topbar nav, since there's no signed-in session yet — while still
// carrying the ported mockup markup and a CSP nonce on its boot script.
func TestEnrollPageRendersBareLayout(t *testing.T) {
	h := newHandler(enrollTestDeps(t))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/enroll", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /enroll status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Create your passkey") {
		t.Errorf("enroll page missing heading:\n%s", body)
	}
	if strings.Contains(body, `class="side"`) || strings.Contains(body, `class="topbar"`) {
		t.Errorf("enroll page rendered the app shell (nav/topbar), want bare layout:\n%s", body)
	}
	if !strings.Contains(body, `<script src="/assets/app.js" nonce="`) {
		t.Errorf("enroll page missing nonce'd app.js boot script:\n%s", body)
	}
}

// TestEnrollBeginHandlerReturnsCreationOptionsAndCookie pins the HTTP
// surface of enrollBeginHandler: given a JSON {"name":...} body, it must
// respond with WebAuthn creation options (a "publicKey" object carrying a
// challenge and the posted name) and set the ceremony cookie
// finishRegistration needs later.
func TestEnrollBeginHandlerReturnsCreationOptionsAndCookie(t *testing.T) {
	h := newHandler(enrollTestDeps(t))
	body, _ := json.Marshal(map[string]string{"name": "on-call"})
	req := httptest.NewRequest(http.MethodPost, "/enroll/begin", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("POST /enroll/begin status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				Name string `json:"name"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if resp.PublicKey.Challenge == "" {
		t.Error("response publicKey.challenge is empty")
	}
	if resp.PublicKey.User.Name != "on-call" {
		t.Errorf("response publicKey.user.name = %q, want on-call", resp.PublicKey.User.Name)
	}

	found := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == enrollSessionCookie {
			found = true
		}
	}
	if !found {
		t.Errorf("POST /enroll/begin did not set %q cookie", enrollSessionCookie)
	}
}

// TestEnrollBeginHandlerRejectsEmptyName pins basic input validation: a
// blank name must not silently create an anonymous account.
func TestEnrollBeginHandlerRejectsEmptyName(t *testing.T) {
	h := newHandler(enrollTestDeps(t))
	body, _ := json.Marshal(map[string]string{"name": "   "})
	req := httptest.NewRequest(http.MethodPost, "/enroll/begin", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("POST /enroll/begin with blank name status = %d, want 400", rr.Code)
	}
}

// TestEnrollHandlersEndToEndPersistCredential drives the full HTTP surface
// (GET /enroll's cousins POST /enroll/begin + POST /enroll/finish) using the
// same virtual authenticator fixtures auth_webauthn_test.go uses to test the
// ceremony functions directly, pinning that the handlers wire beginRegistration/
// finishRegistration/newUserStore together correctly end-to-end: after a
// successful finish, the user is discoverable by name in the on-disk store
// with a real, non-empty credential public key.
func TestEnrollHandlersEndToEndPersistCredential(t *testing.T) {
	d := enrollTestDeps(t)
	h := newHandler(d)

	beginBody, _ := json.Marshal(map[string]string{"name": "on-call"})
	beginReq := httptest.NewRequest(http.MethodPost, "/enroll/begin", bytes.NewReader(beginBody))
	beginRR := httptest.NewRecorder()
	h.ServeHTTP(beginRR, beginReq)
	if beginRR.Code != http.StatusOK {
		t.Fatalf("POST /enroll/begin status = %d, want 200, body: %s", beginRR.Code, beginRR.Body.String())
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

	// enrollTestDeps/testDeps leave Web.RPID/Origin empty (proxy-mode
	// default, config.Default()), so webAuthnConfig derives both from the
	// request's own Host — httptest.NewRequest defaults that to
	// "example.com" for a path-only target URL, giving "http://example.com".
	finishBody, _ := creationResponseBody(t, beginResp.PublicKey.Challenge, "http://example.com", "example.com")
	finishReq := httptest.NewRequest(http.MethodPost, "/enroll/finish", bytes.NewReader(finishBody))
	finishReq.AddCookie(cookie)
	finishRR := httptest.NewRecorder()
	h.ServeHTTP(finishRR, finishReq)
	if finishRR.Code != http.StatusNoContent {
		t.Fatalf("POST /enroll/finish status = %d, want 204, body: %s", finishRR.Code, finishRR.Body.String())
	}

	store := newUserStore(d.StateDir)
	u, ok := store.ByName("on-call")
	if !ok {
		t.Fatal("store.ByName(on-call) = not found after successful enroll")
	}
	if len(u.Credentials) != 1 {
		t.Fatalf("enrolled user has %d credentials, want 1", len(u.Credentials))
	}
	if len(u.Credentials[0].PublicKey) == 0 {
		t.Error("enrolled credential PublicKey is empty, want non-zero bytes")
	}
}

// TestEnrollBeginHandlerReusesExistingUserByName pins that re-enrolling the
// same name (e.g. registering a second device) appends to the existing
// account rather than creating a duplicate.
func TestEnrollBeginHandlerReusesExistingUserByName(t *testing.T) {
	d := enrollTestDeps(t)
	store := newUserStore(d.StateDir)
	existing := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleAdmin, Created: 1}
	if err := store.Put(existing); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	h := newHandler(d)
	beginBody, _ := json.Marshal(map[string]string{"name": "on-call"})
	req := httptest.NewRequest(http.MethodPost, "/enroll/begin", bytes.NewReader(beginBody))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /enroll/begin status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		PublicKey struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// The creation options' user.id is base64url of the WebAuthn user
	// handle (User.ID); it must match the existing seeded account's ID
	// rather than a freshly minted one, proving ByName's lookup was used.
	if want := b64url([]byte(existing.ID)); resp.PublicKey.User.ID != want {
		t.Errorf("creation options user.id = %q, want %q (existing account's id)", resp.PublicKey.User.ID, want)
	}
}
