package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// enrollSessionCookie names the cookie beginRegistration/finishRegistration use to
// correlate the two halves of a registration ceremony.
const enrollSessionCookie = "sw_enroll"

// loginCeremonyCookie is enrollSessionCookie's counterpart for the login
// (assertion) ceremony beginLogin/finishLogin run.
const loginCeremonyCookie = "sw_login"

// newRandomID returns a fresh, unguessable identifier of n random bytes, URL-safe
// base64-encoded.
func newRandomID(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("web: generate random id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// newUserID mints a new WebAuthn user handle: 32 random bytes (well above the spec's
// recommended entropy).
func newUserID() (string, error) {
	return newRandomID(32)
}

// webAuthnConfig builds a *webauthn.WebAuthn configured for the current
// request: the operator-configured web.rp_id/web.origin when set (required
// in autocert/manual mode, see internal/config and validateOrigin), or --
// when left empty, as proxy mode permits -- derived from this request's own
// resolved origin (requestOriginFromContext, issue #59's withRequestOrigin
// middleware).
//
// This is what makes ceremony origin validation (this task's requirement 4)
// actually bite: go-webauthn's ParsedCredentialCreationData.Verify rejects
// any ceremony whose browser-reported clientData.origin isn't exactly one
// of Config.RPOrigins, so a request whose attestation claims a different
// origin than the one this server considers authoritative for itself -- the
// configured origin, or in proxy mode the origin derived from the trusted
// reverse proxy's forwarded headers -- is rejected regardless of what the
// client sends.
func webAuthnConfig(cfg *config.Config, r *http.Request) (*webauthn.WebAuthn, error) {
	rpID, origin := cfg.Web.RPID, cfg.Web.Origin
	if rpID == "" || origin == "" {
		origin = requestOriginFromContext(r)
		if origin == "" {
			// No withRequestOrigin middleware ran (e.g. a handler invoked directly in a test) --
			// fall back to the request's own Host/TLS state rather than leaving origin empty.
			origin = requestOrigin(r, false)
		}
		u, err := url.Parse(origin)
		if err != nil {
			return nil, fmt.Errorf("web: derive rp_id from origin %q: %w", origin, err)
		}
		rpID = u.Hostname()
	}
	requireRK := false
	return webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: "Trinetra",
		RPOrigins:     []string{origin},
		// Ask for a discoverable credential so the default sign-in (no name) can list it; the
		// spec default.
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementPreferred,
			RequireResidentKey: &requireRK,
			UserVerification:   protocol.VerificationPreferred,
		},
		AttestationPreference: protocol.PreferNoAttestation,
	})
}

// ceremonyTTL bounds how long a stashed WebAuthn ceremony (registration or login) is
// retained/valid: it mirrors the enroll/login ceremony cookies' MaxAge.
const ceremonyTTL = 5 * time.Minute

// regCeremonyData is what beginRegistration JSON-encodes into a ceremony Session's Data
// field (session.go) and finishRegistration decodes back out.
type regCeremonyData struct {
	User     *User                 `json:"user"`
	WebAuthn *webauthn.SessionData `json:"webauthn"`
	// Bootstrap marks a tokenless first-run enrollment whose admin-or-refuse decision must be
	// made atomically at finish time.
	Bootstrap bool `json:"bootstrap,omitempty"`
}

// beginRegistration starts a WebAuthn registration ceremony for u: it asks wa for a fresh
// challenge/options (creation).
func beginRegistration(w http.ResponseWriter, r *http.Request, wa *webauthn.WebAuthn, u *User, bootstrap bool, ceremonies SessionStore) (*protocol.CredentialCreation, error) {
	creation, sessionData, err := wa.BeginRegistration(u)
	if err != nil {
		return nil, fmt.Errorf("web: begin registration: %w", err)
	}
	data, err := json.Marshal(regCeremonyData{User: u, WebAuthn: sessionData, Bootstrap: bootstrap})
	if err != nil {
		return nil, fmt.Errorf("web: encode registration ceremony: %w", err)
	}
	sess, err := ceremonies.New("", ceremonyTTL)
	if err != nil {
		return nil, err
	}
	sess.Data = data
	if err := ceremonies.Put(sess); err != nil {
		return nil, err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     enrollSessionCookie,
		Value:    sess.ID,
		Path:     "/enroll",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300, // 5 minutes: ample time for a passkey prompt; matches ceremonyTTL.
	})
	return creation, nil
}

// finishRegistration completes the ceremony started by a prior beginRegistration call: it
// reads enrollSessionCookie off r to find the stashed SessionData/pending User.
func finishRegistration(w http.ResponseWriter, r *http.Request, wa *webauthn.WebAuthn, store UserStore, ceremonies SessionStore) error {
	cookie, err := r.Cookie(enrollSessionCookie)
	if err != nil {
		return fmt.Errorf("web: missing or expired enrollment session: %w", err)
	}
	sess, ok := ceremonies.Get(cookie.Value)
	if !ok {
		return fmt.Errorf("web: enrollment session not found (expired or already used)")
	}
	_ = ceremonies.Delete(cookie.Value) // one-shot: a cookie value is only ever valid for a single finishRegistration call.
	// Clear the cookie regardless of outcome below: it's single-use either way.
	http.SetCookie(w, &http.Cookie{
		Name:     enrollSessionCookie,
		Value:    "",
		Path:     "/enroll",
		MaxAge:   -1,
		HttpOnly: true,
	})

	var data regCeremonyData
	if err := json.Unmarshal(sess.Data, &data); err != nil {
		return fmt.Errorf("web: decode enrollment session: %w", err)
	}

	cred, err := wa.FinishRegistration(data.User, *data.WebAuthn, r)
	if err != nil {
		return fmt.Errorf("web: finish registration: %w", err)
	}

	data.User.Credentials = append(data.User.Credentials, Credential{
		ID:         cred.ID,
		PublicKey:  cred.PublicKey,
		SignCount:  cred.Authenticator.SignCount,
		Transports: transportsToStrings(cred.Transport),
	})
	// A tokenless first-run enrollment (Bootstrap) must decide "am I the first account, and
	// therefore admin?" atomically with the write.
	if data.Bootstrap {
		if err := store.CreateFirstAdmin(data.User); err != nil {
			return fmt.Errorf("web: persist bootstrap admin: %w", err)
		}
		return nil
	}
	if err := store.Put(data.User); err != nil {
		return fmt.Errorf("web: persist user: %w", err)
	}
	return nil
}

// beginLogin starts a WebAuthn login (assertion) ceremony using client-side discoverable
// ("resident key") credentials: unlike beginRegistration.
func beginLogin(w http.ResponseWriter, r *http.Request, wa *webauthn.WebAuthn, ceremonies SessionStore) (*protocol.CredentialAssertion, error) {
	assertion, sessionData, err := wa.BeginDiscoverableLogin()
	if err != nil {
		return nil, fmt.Errorf("web: begin login: %w", err)
	}
	return assertion, stashLoginCeremony(w, r, sessionData, ceremonies)
}

// beginLoginFor starts a sign-in for the account called name, listing its credentials (with
// their transports) so keys that are not discoverable still work.
func beginLoginFor(w http.ResponseWriter, r *http.Request, wa *webauthn.WebAuthn, ceremonies SessionStore, users UserStore, name string) (*protocol.CredentialAssertion, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return beginLogin(w, r, wa, ceremonies)
	}
	var target webauthn.User = decoyLoginUser(name)
	for _, u := range users.List() {
		if strings.EqualFold(u.Name, name) && len(u.Credentials) > 0 {
			target = u
			break
		}
	}
	assertion, sessionData, err := wa.BeginLogin(target)
	if err != nil {
		return nil, fmt.Errorf("web: begin login: %w", err)
	}
	return assertion, stashLoginCeremony(w, r, sessionData, ceremonies)
}

// decoySecret keys decoy credential ids; it changes per process, so a decoy
// cannot be recomputed offline and told apart from a real credential id.
var decoySecret = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}()

type decoyUser struct{ id, credID []byte }

func decoyLoginUser(name string) decoyUser {
	mac := hmac.New(sha256.New, decoySecret)
	mac.Write([]byte(strings.ToLower(name)))
	sum := mac.Sum(nil)
	return decoyUser{id: append([]byte("decoy:"), sum[:8]...), credID: sum}
}

func (d decoyUser) WebAuthnID() []byte          { return d.id }
func (d decoyUser) WebAuthnName() string        { return "" }
func (d decoyUser) WebAuthnDisplayName() string { return "" }
func (d decoyUser) WebAuthnIcon() string        { return "" }
func (d decoyUser) WebAuthnCredentials() []webauthn.Credential {
	return []webauthn.Credential{{ID: d.credID, Transport: []protocol.AuthenticatorTransport{protocol.Internal, protocol.Hybrid}}}
}

func stashLoginCeremony(w http.ResponseWriter, r *http.Request, sessionData *webauthn.SessionData, ceremonies SessionStore) error {
	data, err := json.Marshal(sessionData)
	if err != nil {
		return fmt.Errorf("web: encode login ceremony: %w", err)
	}
	sess, err := ceremonies.New("", ceremonyTTL)
	if err != nil {
		return err
	}
	sess.Data = data
	if err := ceremonies.Put(sess); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     loginCeremonyCookie,
		Value:    sess.ID,
		Path:     "/login",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300, // 5 minutes: matches ceremonyTTL.
	})
	return nil
}

// finishLogin completes the ceremony beginLogin started: it reads loginCeremonyCookie off r
// to find the stashed SessionData, asks wa to verify r's body.
func finishLogin(w http.ResponseWriter, r *http.Request, wa *webauthn.WebAuthn, users UserStore, ceremonies, sessions SessionStore, ttl time.Duration) error {
	cookie, err := r.Cookie(loginCeremonyCookie)
	if err != nil {
		return fmt.Errorf("web: missing or expired login session: %w", err)
	}
	sess, ok := ceremonies.Get(cookie.Value)
	if !ok {
		return fmt.Errorf("web: login session not found (expired or already used)")
	}
	_ = ceremonies.Delete(cookie.Value) // one-shot, same as the registration ceremony.
	http.SetCookie(w, &http.Cookie{
		Name:     loginCeremonyCookie,
		Value:    "",
		Path:     "/login",
		MaxAge:   -1,
		HttpOnly: true,
	})

	var sessionData webauthn.SessionData
	if err := json.Unmarshal(sess.Data, &sessionData); err != nil {
		return fmt.Errorf("web: decode login session: %w", err)
	}

	parsed, err := protocol.ParseCredentialRequestResponse(r)
	if err != nil {
		return fmt.Errorf("web: finish login: %w", err)
	}
	var matched *User
	var cred *webauthn.Credential
	if len(sessionData.UserID) > 0 {
		// Signed in by name (beginLogin with a name): the credential may be
		// non-discoverable and return no user handle.
		u, ok := users.Get(string(sessionData.UserID))
		if !ok {
			return fmt.Errorf("web: unknown credential user")
		}
		matched = u
		cred, err = wa.ValidateLogin(u, sessionData, parsed)
	} else {
		cred, err = wa.ValidateDiscoverableLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
			u, ok := users.Get(string(userHandle))
			if !ok {
				return nil, fmt.Errorf("web: unknown credential user")
			}
			matched = u
			return u, nil
		}, sessionData, parsed)
	}
	if err != nil {
		return fmt.Errorf("web: finish login: %w", err)
	}
	if matched == nil {
		return fmt.Errorf("web: internal error: no user resolved for login")
	}
	// A counter that is still counting but did not advance is the textbook clone signal.
	newCount := parsed.Response.AuthenticatorData.Counter
	if cred.Authenticator.CloneWarning && newCount != 0 {
		return fmt.Errorf("web: authenticator signature counter did not advance (possible cloned credential); login rejected")
	}
	cred.Authenticator.SignCount = newCount

	found := false
	for i := range matched.Credentials {
		if bytes.Equal(matched.Credentials[i].ID, cred.ID) {
			matched.Credentials[i].SignCount = cred.Authenticator.SignCount
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("web: credential not found on resolved user")
	}
	if err := users.Put(matched); err != nil {
		return fmt.Errorf("web: persist updated sign count: %w", err)
	}

	newSess, err := sessions.New(matched.ID, ttl)
	if err != nil {
		return fmt.Errorf("web: create session: %w", err)
	}
	setSessionCookie(w, r, newSess, ttl)
	return nil
}
