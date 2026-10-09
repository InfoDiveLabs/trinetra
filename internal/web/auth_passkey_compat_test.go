package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// The default sign-in (no name) only finds discoverable credentials, so
// registration must ask for one; left at the spec default Android and some
// password managers create keys the picker can't list.
func TestRegistrationRequestsDiscoverableCredential(t *testing.T) {
	cfg := config.Default()
	cfg.Web.RPID, cfg.Web.Origin = testRPID, testOrigin
	wa, err := webAuthnConfig(cfg, httptest.NewRequest(http.MethodPost, "/enroll/begin", nil))
	if err != nil {
		t.Fatal(err)
	}
	u := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleViewer}
	creation, err := beginRegistration(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/enroll/begin", nil), wa, u, false, newCeremonyStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	sel := creation.Response.AuthenticatorSelection
	if sel.ResidentKey != protocol.ResidentKeyRequirementPreferred {
		t.Errorf("residentKey = %q, want preferred", sel.ResidentKey)
	}
	if sel.UserVerification != protocol.VerificationPreferred {
		t.Errorf("userVerification = %q, want preferred", sel.UserVerification)
	}
}

// Synced passkeys report a signature counter of 0. A credential that once
// counted and now reports 0 (moved to a syncing provider) must still sign
// in; a counter that goes backwards while still counting is rejected.
func TestLoginAcceptsAuthenticatorThatStoppedCounting(t *testing.T) {
	wa := testWebAuthn(t, testRPID, testOrigin)
	store := newUserStore(t.TempDir())
	ceremonies := newCeremonyStore(t.TempDir())
	sessions := newSessionStore(t.TempDir())
	u := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleViewer}
	credID, priv := registerVirtualCredentialDirect(t, wa, store, ceremonies, u, testOrigin, testRPID)

	beginRR := httptest.NewRecorder()
	assertion, err := beginLogin(beginRR, httptest.NewRequest(http.MethodPost, "/login/begin", nil), wa, ceremonies)
	if err != nil {
		t.Fatal(err)
	}
	body := assertionResponseBody(t, priv, assertion.Response.Challenge.String(), testOrigin, testRPID, credID, []byte(u.ID), 0)
	req := httptest.NewRequest(http.MethodPost, "/login/finish", bytes.NewReader(body))
	req.AddCookie(cookieFrom(t, beginRR, loginCeremonyCookie))
	if err := finishLogin(httptest.NewRecorder(), req, wa, store, ceremonies, sessions, time.Hour); err != nil {
		t.Fatalf("login with counter 0 after 1: %v", err)
	}
}

// Keys created before registration asked for discoverable credentials (or
// by authenticators that ignore it) return no user handle. Signing in by
// name lists them explicitly, with their transports, and verifies against
// that user.
func TestLoginByNameWorksForNonDiscoverableCredential(t *testing.T) {
	wa := testWebAuthn(t, testRPID, testOrigin)
	store := newUserStore(t.TempDir())
	ceremonies := newCeremonyStore(t.TempDir())
	sessions := newSessionStore(t.TempDir())
	u := &User{ID: mustNewUserID(t), Name: "on-call", Role: RoleViewer}
	credID, priv := registerVirtualCredentialDirect(t, wa, store, ceremonies, u, testOrigin, testRPID)

	beginRR := httptest.NewRecorder()
	assertion, err := beginLoginFor(beginRR, httptest.NewRequest(http.MethodPost, "/login/begin", nil), wa, ceremonies, store, "On-Call ")
	if err != nil {
		t.Fatal(err)
	}
	allowed := assertion.Response.AllowedCredentials
	if len(allowed) != 1 || !bytes.Equal(allowed[0].CredentialID, credID) {
		t.Fatalf("allowCredentials = %+v, want the user's credential", allowed)
	}
	body := assertionResponseBody(t, priv, assertion.Response.Challenge.String(), testOrigin, testRPID, credID, nil, 2)
	req := httptest.NewRequest(http.MethodPost, "/login/finish", bytes.NewReader(body))
	req.AddCookie(cookieFrom(t, beginRR, loginCeremonyCookie))
	if err := finishLogin(httptest.NewRecorder(), req, wa, store, ceremonies, sessions, time.Hour); err != nil {
		t.Fatalf("login by name without a user handle: %v", err)
	}
}

// An unknown name gets the same shape of options as a known one, so the
// sign-in form can't be used to find out which accounts exist.
func TestLoginByUnknownNameDoesNotRevealIt(t *testing.T) {
	wa := testWebAuthn(t, testRPID, testOrigin)
	store := newUserStore(t.TempDir())
	ceremonies := newCeremonyStore(t.TempDir())
	a1, err := beginLoginFor(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/login/begin", nil), wa, ceremonies, store, "nobody")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := beginLoginFor(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/login/begin", nil), wa, ceremonies, store, "nobody")
	if len(a1.Response.AllowedCredentials) != 1 {
		t.Fatalf("unknown name got %d allowCredentials, want 1 like a real account", len(a1.Response.AllowedCredentials))
	}
	if !bytes.Equal(a1.Response.AllowedCredentials[0].CredentialID, a2.Response.AllowedCredentials[0].CredentialID) {
		t.Error("the decoy credential must be stable for a name, or repeating the request reveals it")
	}
}
