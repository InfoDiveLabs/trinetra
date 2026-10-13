package web

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// --- virtual authenticator: builds a real.

// cosePublicKeyCBOR returns a syntactically-valid COSE_Key CBOR encoding of a fresh P-256
// EC public key (kty=EC2, crv=P-256, alg=ES256).
func cosePublicKeyCBOR(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	x := priv.X.Bytes()
	y := priv.Y.Bytes()
	// P-256 coordinates must be exactly 32 bytes; left-pad if Bytes() dropped leading zeroes.
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

// authenticatorData builds a raw authenticatorData structure (§6.1) with the
// attested-credential-data flag set.
func authenticatorData(t *testing.T, rpID string, credID, pubKeyCBOR []byte) []byte {
	t.Helper()
	rpIDHash := sha256.Sum256([]byte(rpID))

	var buf bytes.Buffer
	buf.Write(rpIDHash[:])
	buf.WriteByte(0x41) // flags: UP (0x01) | AT (0x40)
	if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	buf.Write(make([]byte, 16)) // AAGUID: all-zero is valid (self-issued authenticator)
	idLen := make([]byte, 2)
	binary.BigEndian.PutUint16(idLen, uint16(len(credID)))
	buf.Write(idLen)
	buf.Write(credID)
	buf.Write(pubKeyCBOR)
	return buf.Bytes()
}

// attestationObjectCBOR wraps rawAuthData into a "none"-format attestation object.
func attestationObjectCBOR(t *testing.T, rawAuthData []byte) []byte {
	t.Helper()
	obj := map[string]interface{}{
		"fmt":      "none",
		"attStmt":  map[string]interface{}{},
		"authData": rawAuthData,
	}
	b, err := cbor.Marshal(obj)
	if err != nil {
		t.Fatalf("cbor marshal attestation object: %v", err)
	}
	return b
}

// b64url is the shorthand this file uses throughout: WebAuthn wire format
// is base64.RawURLEncoding (no padding) for every byte-string field.
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// creationResponseBody assembles the full JSON body a browser's
// navigator.credentials.create() would POST to /enroll/finish.
func creationResponseBody(t *testing.T, challenge, origin, rpID string) (body []byte, credID []byte) {
	t.Helper()
	credID = make([]byte, 16)
	if _, err := rand.Read(credID); err != nil {
		t.Fatalf("generate credential id: %v", err)
	}

	authData := authenticatorData(t, rpID, credID, cosePublicKeyCBOR(t))
	attObj := attestationObjectCBOR(t, authData)

	clientData := map[string]string{
		"type":      "webauthn.create",
		"challenge": challenge,
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
	body, err = json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal creation response: %v", err)
	}
	return body, credID
}

// testWebAuthn returns a *webauthn.WebAuthn configured for rpID/origin, used directly.
func testWebAuthn(t *testing.T, rpID, origin string) *webauthn.WebAuthn {
	t.Helper()
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: "trinetra test",
		RPOrigins:     []string{origin},
	})
	if err != nil {
		t.Fatalf("webauthn.New: %v", err)
	}
	return wa
}

const (
	testRPID   = "monitor.example.com"
	testOrigin = "https://monitor.example.com"
)

func mustNewUserID(t *testing.T) string {
	t.Helper()
	id, err := newUserID()
	if err != nil {
		t.Fatalf("newUserID: %v", err)
	}
	return id
}

// cookieFrom extracts the named cookie from a recorder's Set-Cookie
// headers, failing the test if it's absent.
func cookieFrom(t *testing.T, rr *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	resp := rr.Result()
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("cookie %q not set; got %v", name, resp.Cookies())
	return nil
}

// TestBeginRegistrationReturnsCreationOptionsAndSetsCookie pins the begin-half of the
// ceremony in isolation: it must hand back real PublicKeyCredentialCreationOptions.
func TestBeginRegistrationReturnsCreationOptionsAndSetsCookie(t *testing.T) {
	wa := testWebAuthn(t, testRPID, testOrigin)
	u := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleViewer}
	r := httptest.NewRequest(http.MethodPost, "/enroll/begin", nil)
	rr := httptest.NewRecorder()
	ceremonies := newCeremonyStore(t.TempDir())

	creation, err := beginRegistration(rr, r, wa, u, false, ceremonies)
	if err != nil {
		t.Fatalf("beginRegistration: %v", err)
	}
	if len(creation.Response.Challenge) == 0 {
		t.Error("creation.Response.Challenge is empty")
	}
	if got := creation.Response.User.Name; got != "on-call" {
		t.Errorf("creation.Response.User.Name = %q, want on-call", got)
	}

	cookie := cookieFrom(t, rr, enrollSessionCookie)
	if cookie.Value == "" {
		t.Error("enrollSessionCookie value is empty")
	}
	if !cookie.HttpOnly {
		t.Error("enrollSessionCookie must be HttpOnly")
	}
}

// TestRegistrationRoundTripProducesStoredCredentialWithPublicKey is the full begin ->
// (virtual authenticator) -> finish ceremony.
func TestRegistrationRoundTripProducesStoredCredentialWithPublicKey(t *testing.T) {
	wa := testWebAuthn(t, testRPID, testOrigin)
	store := newUserStore(t.TempDir())
	ceremonies := newCeremonyStore(t.TempDir())

	u := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleViewer}
	beginReq := httptest.NewRequest(http.MethodPost, "/enroll/begin", nil)
	beginRR := httptest.NewRecorder()
	creation, err := beginRegistration(beginRR, beginReq, wa, u, false, ceremonies)
	if err != nil {
		t.Fatalf("beginRegistration: %v", err)
	}

	body, credID := creationResponseBody(t, creation.Response.Challenge.String(), testOrigin, testRPID)

	finishReq := httptest.NewRequest(http.MethodPost, "/enroll/finish", bytes.NewReader(body))
	finishReq.Header.Set("Content-Type", "application/json")
	finishReq.AddCookie(cookieFrom(t, beginRR, enrollSessionCookie))
	finishRR := httptest.NewRecorder()

	if err := finishRegistration(finishRR, finishReq, wa, store, ceremonies); err != nil {
		t.Fatalf("finishRegistration: %v", err)
	}

	got, ok := store.Get(u.ID)
	if !ok {
		t.Fatal("store.Get(u.ID) = not found after successful finishRegistration")
	}
	if len(got.Credentials) != 1 {
		t.Fatalf("stored user has %d credentials, want 1", len(got.Credentials))
	}
	cred := got.Credentials[0]
	if len(cred.PublicKey) == 0 {
		t.Error("stored credential PublicKey is empty, want non-zero bytes")
	}
	if !bytes.Equal(cred.ID, credID) {
		t.Errorf("stored credential ID = %x, want %x", cred.ID, credID)
	}
}

// TestRegistrationRejectsTamperedAttestation pins the negative path: a finish request whose
// clientDataJSON claims a challenge that was never issued.
func TestRegistrationRejectsTamperedAttestation(t *testing.T) {
	wa := testWebAuthn(t, testRPID, testOrigin)
	store := newUserStore(t.TempDir())
	ceremonies := newCeremonyStore(t.TempDir())

	u := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleViewer}
	beginReq := httptest.NewRequest(http.MethodPost, "/enroll/begin", nil)
	beginRR := httptest.NewRecorder()
	if _, err := beginRegistration(beginRR, beginReq, wa, u, false, ceremonies); err != nil {
		t.Fatalf("beginRegistration: %v", err)
	}

	// Use a bogus challenge instead of the one beginRegistration actually
	// issued: the equivalent of an attacker crafting/replaying a response.
	body, _ := creationResponseBody(t, b64url([]byte("not-the-real-challenge-bytes!!!")), testOrigin, testRPID)

	finishReq := httptest.NewRequest(http.MethodPost, "/enroll/finish", bytes.NewReader(body))
	finishReq.AddCookie(cookieFrom(t, beginRR, enrollSessionCookie))
	finishRR := httptest.NewRecorder()

	if err := finishRegistration(finishRR, finishReq, wa, store, ceremonies); err == nil {
		t.Fatal("finishRegistration(tampered challenge) = nil error, want error")
	}
	if _, ok := store.Get(u.ID); ok {
		t.Error("store.Get(u.ID) = found after rejected attestation, want not found (nothing persisted)")
	}
}

// TestRegistrationRejectsWrongOrigin is this task's requirement-4 pin.
func TestRegistrationRejectsWrongOrigin(t *testing.T) {
	wa := testWebAuthn(t, testRPID, testOrigin)
	store := newUserStore(t.TempDir())
	ceremonies := newCeremonyStore(t.TempDir())

	u := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleViewer}
	beginReq := httptest.NewRequest(http.MethodPost, "/enroll/begin", nil)
	beginRR := httptest.NewRecorder()
	creation, err := beginRegistration(beginRR, beginReq, wa, u, false, ceremonies)
	if err != nil {
		t.Fatalf("beginRegistration: %v", err)
	}

	wrongOrigin := "https://evil.example.com"
	body, _ := creationResponseBody(t, creation.Response.Challenge.String(), wrongOrigin, testRPID)

	finishReq := httptest.NewRequest(http.MethodPost, "/enroll/finish", bytes.NewReader(body))
	finishReq.AddCookie(cookieFrom(t, beginRR, enrollSessionCookie))
	finishRR := httptest.NewRecorder()

	err = finishRegistration(finishRR, finishReq, wa, store, ceremonies)
	if err == nil {
		t.Fatal("finishRegistration(wrong origin) = nil error, want error")
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Logf("finishRegistration(wrong origin) error = %v (expected to mention origin, but go-webauthn's wording may vary)", err)
	}
	if _, ok := store.Get(u.ID); ok {
		t.Error("store.Get(u.ID) = found after wrong-origin attestation, want not found")
	}
}

// TestWebAuthnConfigUsesConfiguredRPIDOrigin pins webAuthnConfig's primary path: when
// web.rp_id/web.origin are set (autocert/manual mode).
func TestWebAuthnConfigUsesConfiguredRPIDOrigin(t *testing.T) {
	cfg := config.Default()
	cfg.Web.RPID = testRPID
	cfg.Web.Origin = testOrigin

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	wa, err := webAuthnConfig(cfg, r)
	if err != nil {
		t.Fatalf("webAuthnConfig: %v", err)
	}
	if wa.Config.RPID != testRPID {
		t.Errorf("RPID = %q, want %q", wa.Config.RPID, testRPID)
	}
	if len(wa.Config.RPOrigins) != 1 || wa.Config.RPOrigins[0] != testOrigin {
		t.Errorf("RPOrigins = %v, want [%s]", wa.Config.RPOrigins, testOrigin)
	}
}

// TestWebAuthnConfigDerivesFromRequestOriginInProxyMode pins the proxy-mode fallback: with
// web.rp_id/web.origin left empty.
func TestWebAuthnConfigDerivesFromRequestOriginInProxyMode(t *testing.T) {
	cfg := config.Default() // Web.RPID/Origin left empty (proxy-mode default)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), requestOriginCtxKey{}, testOrigin))

	wa, err := webAuthnConfig(cfg, r)
	if err != nil {
		t.Fatalf("webAuthnConfig: %v", err)
	}
	if wa.Config.RPID != testRPID {
		t.Errorf("derived RPID = %q, want %q", wa.Config.RPID, testRPID)
	}
	if len(wa.Config.RPOrigins) != 1 || wa.Config.RPOrigins[0] != testOrigin {
		t.Errorf("derived RPOrigins = %v, want [%s]", wa.Config.RPOrigins, testOrigin)
	}
}

// Expiry/eviction/capacity of the ceremony stash is pinned in session_test.go.
