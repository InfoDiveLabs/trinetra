package web

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/webauthn"
)

// --- login (assertion) virtual authenticator fixtures. These extend
// auth_webauthn_test.go's registration fixtures: a login assertion must be
// signed with the SAME private key whose public half was registered, so --
// unlike cosePublicKeyCBOR, which discards its key -- these fixtures keep the
// *ecdsa.PrivateKey around across a register-then-login test.

// cosePublicKeyCBORForKey is cosePublicKeyCBOR (auth_webauthn_test.go) with
// the EC key supplied rather than freshly generated, so the same key can
// later sign a login assertion.
func cosePublicKeyCBORForKey(t *testing.T, priv *ecdsa.PrivateKey) []byte {
	t.Helper()
	x := priv.X.Bytes()
	y := priv.Y.Bytes()
	x = append(make([]byte, 32-len(x)), x...)
	y = append(make([]byte, 32-len(y)), y...)

	coseKey := map[int]interface{}{
		1:  2,  // kty: EC2
		3:  -7, // alg: ES256
		-1: 1,  // crv: P-256
		-2: x,
		-3: y,
	}
	b, err := cbor.Marshal(coseKey)
	if err != nil {
		t.Fatalf("cbor marshal COSE key: %v", err)
	}
	return b
}

// registerVirtualCredentialDirect runs a full begin->finish registration
// ceremony (beginRegistration/finishRegistration, auth_webauthn.go) for u
// against a freshly generated EC key pair, using the same "none"-attestation
// shape auth_webauthn_test.go's creationResponseBody builds but pointed at
// THIS key rather than a throwaway one -- so the private key can go on to
// sign a login assertion in the same test. Returns the credential ID and
// private key.
func registerVirtualCredentialDirect(t *testing.T, wa *webauthn.WebAuthn, store UserStore, ceremonies SessionStore, u *User, origin, rpID string) (credID []byte, priv *ecdsa.PrivateKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}

	beginReq := httptest.NewRequest(http.MethodPost, "/enroll/begin", nil)
	beginRR := httptest.NewRecorder()
	creation, err := beginRegistration(beginRR, beginReq, wa, u, false, ceremonies)
	if err != nil {
		t.Fatalf("beginRegistration: %v", err)
	}

	credID = make([]byte, 16)
	if _, err := rand.Read(credID); err != nil {
		t.Fatalf("generate credential id: %v", err)
	}
	authData := authenticatorData(t, rpID, credID, cosePublicKeyCBORForKey(t, priv))
	attObj := attestationObjectCBOR(t, authData)
	clientData := map[string]string{
		"type":      "webauthn.create",
		"challenge": creation.Response.Challenge.String(),
		"origin":    origin,
	}
	clientDataJSON, err := json.Marshal(clientData)
	if err != nil {
		t.Fatalf("marshal clientDataJSON: %v", err)
	}
	resp := map[string]interface{}{
		"id":    b64url(credID),
		"rawId": b64url(credID),
		"type":  "public-key",
		"response": map[string]interface{}{
			"clientDataJSON":    b64url(clientDataJSON),
			"attestationObject": b64url(attObj),
		},
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal creation response: %v", err)
	}

	finishReq := httptest.NewRequest(http.MethodPost, "/enroll/finish", bytes.NewReader(body))
	finishReq.AddCookie(cookieFrom(t, beginRR, enrollSessionCookie))
	finishRR := httptest.NewRecorder()
	if err := finishRegistration(finishRR, finishReq, wa, store, ceremonies); err != nil {
		t.Fatalf("finishRegistration: %v", err)
	}
	return credID, priv
}

// assertionResponseBody builds the full JSON body a browser's
// navigator.credentials.get() would POST to /login/finish: a real
// ECDSA-P256/ES256 signature over authenticatorData||sha256(clientDataJSON),
// matching what go-webauthn's protocol.ParsedCredentialAssertionData.Verify
// checks (see protocol/assertion.go, webauthncose.EC2PublicKeyData.Verify).
func assertionResponseBody(t *testing.T, priv *ecdsa.PrivateKey, challenge, origin, rpID string, credID, userHandle []byte, counter uint32) []byte {
	t.Helper()
	rpIDHash := sha256.Sum256([]byte(rpID))
	var authData bytes.Buffer
	authData.Write(rpIDHash[:])
	authData.WriteByte(0x01) // flags: UP only (no attested credential data on an assertion).
	if err := binary.Write(&authData, binary.BigEndian, counter); err != nil {
		t.Fatalf("write counter: %v", err)
	}

	clientData := map[string]string{
		"type":      "webauthn.get",
		"challenge": challenge,
		"origin":    origin,
	}
	clientDataJSON, err := json.Marshal(clientData)
	if err != nil {
		t.Fatalf("marshal clientDataJSON: %v", err)
	}
	clientDataHash := sha256.Sum256(clientDataJSON)

	sigData := append(append([]byte{}, authData.Bytes()...), clientDataHash[:]...)
	digest := sha256.Sum256(sigData)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}

	resp := map[string]interface{}{
		"id":    b64url(credID),
		"rawId": b64url(credID),
		"type":  "public-key",
		"response": map[string]interface{}{
			"clientDataJSON":    b64url(clientDataJSON),
			"authenticatorData": b64url(authData.Bytes()),
			"signature":         b64url(sig),
			"userHandle":        b64url(userHandle),
		},
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal assertion response: %v", err)
	}
	return body
}

// TestLoginRoundTripIssuesSessionCookie is the core positive pin:
// begin -> (virtual authenticator assertion) -> finish for a credential
// registered via the registration ceremony must issue a signed-in sw_session
// cookie, and the credential's stored SignCount must advance to the
// asserted counter.
func TestLoginRoundTripIssuesSessionCookie(t *testing.T) {
	wa := testWebAuthn(t, testRPID, testOrigin)
	store := newUserStore(t.TempDir())
	ceremonies := newCeremonyStore(t.TempDir())
	sessions := newSessionStore(t.TempDir())

	u := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleViewer}
	credID, priv := registerVirtualCredentialDirect(t, wa, store, ceremonies, u, testOrigin, testRPID)

	beginReq := httptest.NewRequest(http.MethodPost, "/login/begin", nil)
	beginRR := httptest.NewRecorder()
	assertion, err := beginLogin(beginRR, beginReq, wa, ceremonies)
	if err != nil {
		t.Fatalf("beginLogin: %v", err)
	}

	// The registration fixture's authenticatorData (auth_webauthn_test.go's
	// authenticatorData helper) hardcodes counter=1, so the stored SignCount
	// after registration is already 1 -- the login assertion's counter must
	// exceed that to count as a legitimate (non-regressed) advance.
	loginBody := assertionResponseBody(t, priv, assertion.Response.Challenge.String(), testOrigin, testRPID, credID, []byte(u.ID), 2)
	finishReq := httptest.NewRequest(http.MethodPost, "/login/finish", bytes.NewReader(loginBody))
	finishReq.AddCookie(cookieFrom(t, beginRR, loginCeremonyCookie))
	finishRR := httptest.NewRecorder()

	if err := finishLogin(finishRR, finishReq, wa, store, ceremonies, sessions, time.Hour); err != nil {
		t.Fatalf("finishLogin: %v", err)
	}

	sessCookie := cookieFrom(t, finishRR, sessionCookieName)
	if sessCookie.Value == "" {
		t.Fatal("sw_session cookie value is empty")
	}
	if !sessCookie.HttpOnly {
		t.Error("sw_session cookie must be HttpOnly")
	}
	sess, ok := sessions.Get(sessCookie.Value)
	if !ok {
		t.Fatal("issued session not found in store")
	}
	if sess.UserID != u.ID {
		t.Errorf("session UserID = %q, want %q", sess.UserID, u.ID)
	}
	if sess.CSRF == "" {
		t.Error("issued session has an empty CSRF token")
	}

	got, ok := store.Get(u.ID)
	if !ok {
		t.Fatal("user not found after login")
	}
	if len(got.Credentials) != 1 || got.Credentials[0].SignCount != 2 {
		t.Errorf("stored credential SignCount = %+v, want SignCount=2", got.Credentials)
	}
}

// TestCeremonyFloodDoesNotStarveValidLogin is the availability pin for the
// separate ceremony store (session.go): an unauthenticated flood of ceremony
// begins fills only the bounded CEREMONY store, and must never prevent a user
// presenting a VALID passkey from minting an authenticated session -- the
// bug that would exist if ceremony placeholders and real sessions shared one
// capped store.
func TestCeremonyFloodDoesNotStarveValidLogin(t *testing.T) {
	wa := testWebAuthn(t, testRPID, testOrigin)
	store := newUserStore(t.TempDir())
	// Small ceremony cap so the flood is a handful of cheap writes rather
	// than ceremonyMaxEntries of them; the authenticated-session store is a
	// distinct instance (distinct file, distinct cap).
	const ceremonyCap = 8
	ceremonies := &jsonSessionStore{path: t.TempDir() + "/ceremonies.json", maxEntries: ceremonyCap}
	sessions := newSessionStore(t.TempDir())

	u := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleViewer}
	credID, priv := registerVirtualCredentialDirect(t, wa, store, ceremonies, u, testOrigin, testRPID)

	// A legitimate login ceremony begins BEFORE the flood, so its placeholder
	// is already stashed when the attacker starts hammering /login/begin.
	beginReq := httptest.NewRequest(http.MethodPost, "/login/begin", nil)
	beginRR := httptest.NewRecorder()
	assertion, err := beginLogin(beginRR, beginReq, wa, ceremonies)
	if err != nil {
		t.Fatalf("beginLogin: %v", err)
	}
	loginCookie := cookieFrom(t, beginRR, loginCeremonyCookie)

	// Flood the ceremony store to its cap. Once full, further ceremony begins
	// are refused (that part is fine -- it only rate-limits new ceremonies).
	flooded := false
	for i := 0; i < ceremonyCap*2; i++ {
		if _, err := ceremonies.New("", ceremonyTTL); err != nil {
			flooded = true
			break
		}
	}
	if !flooded {
		t.Fatal("flood never reached the ceremony cap; test setup is wrong")
	}
	if _, err := ceremonies.New("", ceremonyTTL); err == nil {
		t.Fatal("ceremony store is not actually full after the flood")
	}

	// The valid, already-begun login must STILL finish and issue a session:
	// finishLogin mints it in the authenticated-session store, which the
	// ceremony flood cannot touch.
	loginBody := assertionResponseBody(t, priv, assertion.Response.Challenge.String(), testOrigin, testRPID, credID, []byte(u.ID), 2)
	finishReq := httptest.NewRequest(http.MethodPost, "/login/finish", bytes.NewReader(loginBody))
	finishReq.AddCookie(loginCookie)
	finishRR := httptest.NewRecorder()
	if err := finishLogin(finishRR, finishReq, wa, store, ceremonies, sessions, time.Hour); err != nil {
		t.Fatalf("finishLogin under ceremony flood = %v, want success (flood must not starve a valid login)", err)
	}
	sessCookie := cookieFrom(t, finishRR, sessionCookieName)
	if _, ok := sessions.Get(sessCookie.Value); !ok {
		t.Fatal("valid login issued no usable session despite the ceremony flood")
	}
}

// TestLoginRejectsSignCountRegression pins clone detection: a second login
// whose assertion counter does not exceed the credential's last stored
// SignCount must be rejected outright (no session issued), and the stored
// SignCount must be left exactly as the first, legitimate login left it --
// an attacker replaying a cloned authenticator's earlier counter value must
// not get to consume it.
func TestLoginRejectsSignCountRegression(t *testing.T) {
	wa := testWebAuthn(t, testRPID, testOrigin)
	store := newUserStore(t.TempDir())
	ceremonies := newCeremonyStore(t.TempDir())
	sessions := newSessionStore(t.TempDir())

	u := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleViewer}
	credID, priv := registerVirtualCredentialDirect(t, wa, store, ceremonies, u, testOrigin, testRPID)

	// First, legitimate login: counter advances 1 (registration) -> 5.
	doLogin := func(counter uint32) error {
		beginReq := httptest.NewRequest(http.MethodPost, "/login/begin", nil)
		beginRR := httptest.NewRecorder()
		assertion, err := beginLogin(beginRR, beginReq, wa, ceremonies)
		if err != nil {
			t.Fatalf("beginLogin: %v", err)
		}
		body := assertionResponseBody(t, priv, assertion.Response.Challenge.String(), testOrigin, testRPID, credID, []byte(u.ID), counter)
		finishReq := httptest.NewRequest(http.MethodPost, "/login/finish", bytes.NewReader(body))
		finishReq.AddCookie(cookieFrom(t, beginRR, loginCeremonyCookie))
		finishRR := httptest.NewRecorder()
		return finishLogin(finishRR, finishReq, wa, store, ceremonies, sessions, time.Hour)
	}

	if err := doLogin(5); err != nil {
		t.Fatalf("first (legitimate) login: %v", err)
	}
	got, _ := store.Get(u.ID)
	if got.Credentials[0].SignCount != 5 {
		t.Fatalf("SignCount after first login = %d, want 5", got.Credentials[0].SignCount)
	}

	// Second login replays the SAME counter value (5 <= 5): a cloned
	// authenticator's textbook signature -- must be rejected.
	if err := doLogin(5); err == nil {
		t.Fatal("login with non-advancing signCount = nil error, want rejection")
	}

	// The stored SignCount must be untouched by the rejected attempt.
	got, _ = store.Get(u.ID)
	if got.Credentials[0].SignCount != 5 {
		t.Errorf("SignCount after rejected replay = %d, want unchanged 5", got.Credentials[0].SignCount)
	}

	// A lower counter (regression proper) is rejected the same way.
	if err := doLogin(3); err == nil {
		t.Fatal("login with lower signCount = nil error, want rejection")
	}
}

// TestLoginRejectsExpiredCeremonySession pins that a login ceremony whose
// stashed Session has expired (the sw_login cookie references an ID the
// store now treats as absent) is rejected -- the assertion-side counterpart
// of finishRegistration's "enrollment session not found" behavior.
func TestLoginRejectsExpiredCeremonySession(t *testing.T) {
	wa := testWebAuthn(t, testRPID, testOrigin)
	store := newUserStore(t.TempDir())
	now := time.Unix(1_700_000_000, 0)
	// Clock-controlled ceremony store so the ceremony placeholder can be
	// aged past ceremonyTTL; the authenticated-session store is separate and
	// never reached (finishLogin fails at the ceremony Get).
	ceremonies := &jsonSessionStore{path: t.TempDir() + "/ceremonies.json", now: func() time.Time { return now }, maxEntries: ceremonyMaxEntries}
	sessions := newSessionStore(t.TempDir())

	u := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleViewer}
	credID, priv := registerVirtualCredentialDirect(t, wa, store, ceremonies, u, testOrigin, testRPID)

	beginReq := httptest.NewRequest(http.MethodPost, "/login/begin", nil)
	beginRR := httptest.NewRecorder()
	assertion, err := beginLogin(beginRR, beginReq, wa, ceremonies)
	if err != nil {
		t.Fatalf("beginLogin: %v", err)
	}
	cookie := cookieFrom(t, beginRR, loginCeremonyCookie)

	// Advance well past ceremonyTTL before finishing.
	now = now.Add(ceremonyTTL + time.Minute)

	body := assertionResponseBody(t, priv, assertion.Response.Challenge.String(), testOrigin, testRPID, credID, []byte(u.ID), 2)
	finishReq := httptest.NewRequest(http.MethodPost, "/login/finish", bytes.NewReader(body))
	finishReq.AddCookie(cookie)
	finishRR := httptest.NewRecorder()

	if err := finishLogin(finishRR, finishReq, wa, store, ceremonies, sessions, time.Hour); err == nil {
		t.Fatal("finishLogin with expired ceremony session = nil error, want rejection")
	}
	for _, c := range finishRR.Result().Cookies() {
		if c.Name == sessionCookieName {
			t.Error("finishLogin issued a sw_session cookie despite rejection")
		}
	}
}

// TestLoginHTTPHandlersAndLogoutCSRFFlow drives the full HTTP surface (POST
// /login/begin, /login/finish, /logout through newHandler) rather than
// calling beginLogin/finishLogin directly, pinning that routes.go's wiring --
// webAuthnConfig derivation, sessionMiddleware/requireCSRF composition, and
// the sw_session cookie's actual Set-Cookie header -- all work together.
// Covers this task's CSRF requirement end-to-end: missing token -> 403,
// mismatched token -> 403 (session left intact), matching token -> 204
// (session deleted, cookie cleared).
func TestLoginHTTPHandlersAndLogoutCSRFFlow(t *testing.T) {
	d := enrollTestDeps(t)
	h := newHandler(d)

	// enrollTestDeps/testDeps leave Web.RPID/Origin empty (proxy-mode
	// default), so webAuthnConfig derives both from the request's own Host --
	// httptest.NewRequest defaults that to "example.com", same as
	// TestEnrollHandlersEndToEndPersistCredential relies on.
	wa, err := webAuthnConfig(d.Cfg(), httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("webAuthnConfig: %v", err)
	}
	store := newUserStore(d.StateDir)
	ceremonies := newCeremonyStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	u := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleViewer}
	credID, priv := registerVirtualCredentialDirect(t, wa, store, ceremonies, u, "http://example.com", "example.com")

	beginRR := httptest.NewRecorder()
	h.ServeHTTP(beginRR, httptest.NewRequest(http.MethodPost, "/login/begin", nil))
	if beginRR.Code != http.StatusOK {
		t.Fatalf("POST /login/begin status = %d, want 200, body: %s", beginRR.Code, beginRR.Body.String())
	}
	var beginResp struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(beginRR.Body.Bytes(), &beginResp); err != nil {
		t.Fatalf("decode begin response: %v", err)
	}
	ceremonyCookie := cookieFrom(t, beginRR, loginCeremonyCookie)

	loginBody := assertionResponseBody(t, priv, beginResp.PublicKey.Challenge, "http://example.com", "example.com", credID, []byte(u.ID), 2)
	finishReq := httptest.NewRequest(http.MethodPost, "/login/finish", bytes.NewReader(loginBody))
	finishReq.AddCookie(ceremonyCookie)
	finishRR := httptest.NewRecorder()
	h.ServeHTTP(finishRR, finishReq)
	if finishRR.Code != http.StatusNoContent {
		t.Fatalf("POST /login/finish status = %d, want 204, body: %s", finishRR.Code, finishRR.Body.String())
	}
	sessCookie := cookieFrom(t, finishRR, sessionCookieName)

	sess, ok := sessions.Get(sessCookie.Value)
	if !ok {
		t.Fatal("issued session not found in store")
	}

	// Missing CSRF token -> 403, session untouched.
	noToken := httptest.NewRequest(http.MethodPost, "/logout", nil)
	noToken.AddCookie(sessCookie)
	noTokenRR := httptest.NewRecorder()
	h.ServeHTTP(noTokenRR, noToken)
	if noTokenRR.Code != http.StatusForbidden {
		t.Fatalf("POST /logout without CSRF token status = %d, want 403", noTokenRR.Code)
	}

	// Mismatched CSRF token -> 403, session still untouched.
	wrong := httptest.NewRequest(http.MethodPost, "/logout", nil)
	wrong.AddCookie(sessCookie)
	wrong.Header.Set("X-CSRF-Token", "not-the-real-token")
	wrongRR := httptest.NewRecorder()
	h.ServeHTTP(wrongRR, wrong)
	if wrongRR.Code != http.StatusForbidden {
		t.Fatalf("POST /logout with wrong CSRF token status = %d, want 403", wrongRR.Code)
	}
	if _, ok := sessions.Get(sessCookie.Value); !ok {
		t.Fatal("session deleted by a rejected (missing/wrong CSRF) logout attempt")
	}

	// Matching CSRF token -> 204, session deleted, cookie cleared.
	ok2 := httptest.NewRequest(http.MethodPost, "/logout", nil)
	ok2.AddCookie(sessCookie)
	ok2.Header.Set("X-CSRF-Token", sess.CSRF)
	okRR := httptest.NewRecorder()
	h.ServeHTTP(okRR, ok2)
	if okRR.Code != http.StatusNoContent {
		t.Fatalf("POST /logout with valid CSRF token status = %d, want 204, body: %s", okRR.Code, okRR.Body.String())
	}
	if _, stillThere := sessions.Get(sessCookie.Value); stillThere {
		t.Error("session still present after logout")
	}
	cleared := cookieFrom(t, okRR, sessionCookieName)
	if cleared.MaxAge >= 0 {
		t.Errorf("logout's sw_session Set-Cookie MaxAge = %d, want negative (expire immediately)", cleared.MaxAge)
	}
}

// TestLogoutRejectsExpiredSession pins "expired session -> unauthenticated"
// at the HTTP layer: a sw_session cookie naming an already-expired record
// (SessionStore.Get treats it as absent) must be rejected by requireCSRF --
// which sees no session in context at all -- even though the caller supplies
// that session's own (otherwise-correct) CSRF token.
func TestLogoutRejectsExpiredSession(t *testing.T) {
	d := enrollTestDeps(t)
	h := newHandler(d)
	sessions := newSessionStore(d.StateDir)

	sess, err := sessions.New("user-1", -time.Second) // already expired
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	req.Header.Set("X-CSRF-Token", sess.CSRF)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST /logout with expired session status = %d, want 403 (unauthenticated)", rr.Code)
	}
}
