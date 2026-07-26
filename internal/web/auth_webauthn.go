//go:build web

package web

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"serverwatch/internal/config"
)

// enrollSessionCookie names the cookie beginRegistration/finishRegistration
// use to correlate the two halves of a registration ceremony; see
// regCeremonyData's doc for what's stashed under it.
const enrollSessionCookie = "sw_enroll"

// loginCeremonyCookie is enrollSessionCookie's counterpart for the login
// (assertion) ceremony beginLogin/finishLogin run.
const loginCeremonyCookie = "sw_login"

// newRandomID returns a fresh, unguessable identifier of n random bytes,
// URL-safe base64-encoded (so it drops cleanly into a cookie value or a
// WebAuthn user handle without escaping) — the same construction
// security.go's newNonce uses for the CSP nonce.
func newRandomID(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("web: generate random id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// newUserID mints a new WebAuthn user handle: 32 random bytes (well above
// the spec's recommended entropy), matching webauthn.User.WebAuthnID's doc
// that this value should be "completely random" rather than derived from
// the account name.
func newUserID() (string, error) {
	return newRandomID(32)
}

// webAuthnConfig builds a *webauthn.WebAuthn configured for the current
// request: the operator-configured web.rp_id/web.origin when set (required
// in autocert/manual mode, see internal/config and validateOrigin), or —
// when left empty, as proxy mode permits — derived from this request's own
// resolved origin (requestOriginFromContext, issue #59's withRequestOrigin
// middleware).
//
// This is what makes ceremony origin validation (this task's requirement 4)
// actually bite: go-webauthn's ParsedCredentialCreationData.Verify rejects
// any ceremony whose browser-reported clientData.origin isn't exactly one
// of Config.RPOrigins, so a request whose attestation claims a different
// origin than the one this server considers authoritative for itself — the
// configured origin, or in proxy mode the origin derived from the trusted
// reverse proxy's forwarded headers — is rejected regardless of what the
// client sends.
func webAuthnConfig(cfg *config.Config, r *http.Request) (*webauthn.WebAuthn, error) {
	rpID, origin := cfg.Web.RPID, cfg.Web.Origin
	if rpID == "" || origin == "" {
		origin = requestOriginFromContext(r)
		if origin == "" {
			// No withRequestOrigin middleware ran (e.g. a handler invoked
			// directly in a test) — fall back to the request's own
			// Host/TLS state rather than leaving origin empty.
			origin = requestOrigin(r, false)
		}
		u, err := url.Parse(origin)
		if err != nil {
			return nil, fmt.Errorf("web: derive rp_id from origin %q: %w", origin, err)
		}
		rpID = u.Hostname()
	}
	return webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: "serverwatch",
		RPOrigins:     []string{origin},
	})
}

// ceremonyTTL bounds how long a stashed WebAuthn ceremony (registration or
// login) is retained/valid: it mirrors the enroll/login ceremony cookies'
// MaxAge (5 min) so a stashed Session never outlives the cookie that
// references it.
const ceremonyTTL = 5 * time.Minute

// regCeremonyData is what beginRegistration JSON-encodes into a ceremony
// Session's Data field (session.go) and finishRegistration decodes back out:
// the go-webauthn SessionData the ceremony needs to verify the attestation,
// plus the pending *User being enrolled (not yet persisted —
// finishRegistration's store.Put is the first time it's written).
//
// TODO(#61) resolved: this replaces the temporary in-memory ceremonyStash
// (see git history) with the real, file-backed SessionStore — both
// registration and login ceremonies (beginLogin/finishLogin, below) now
// stash their SessionData the same way, keyed by a Session.ID set in a
// short-lived, ceremony-scoped cookie.
type regCeremonyData struct {
	User     *User                 `json:"user"`
	WebAuthn *webauthn.SessionData `json:"webauthn"`
}

// beginRegistration starts a WebAuthn registration ceremony for u: it asks
// wa for a fresh challenge/options (creation), stashes the resulting
// SessionData alongside u in a new ceremony Session (sessions, ttl
// ceremonyTTL — see regCeremonyData's doc), and sets enrollSessionCookie on
// w so the browser echoes the same ceremony id back to /enroll/finish. The
// caller (enrollBeginHandler, routes.go) is responsible for JSON-encoding
// the returned creation options onto the response body.
func beginRegistration(w http.ResponseWriter, r *http.Request, wa *webauthn.WebAuthn, u *User, ceremonies SessionStore) (*protocol.CredentialCreation, error) {
	creation, sessionData, err := wa.BeginRegistration(u)
	if err != nil {
		return nil, fmt.Errorf("web: begin registration: %w", err)
	}
	data, err := json.Marshal(regCeremonyData{User: u, WebAuthn: sessionData})
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

// finishRegistration completes the ceremony started by a prior
// beginRegistration call: it reads enrollSessionCookie off r to find the
// stashed SessionData/pending User, asks wa to verify r's body (the
// browser's attestation response) against that session, and — only on a
// successful verification — appends the newly minted *webauthn.Credential
// to the user (flattened into this package's Credential shape) and persists
// via store.Put. A tampered/invalid attestation (wrong origin, wrong
// challenge, corrupted signature, replayed/reused cookie, ...) returns an
// error and nothing is persisted.
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
	if err := store.Put(data.User); err != nil {
		return fmt.Errorf("web: persist user: %w", err)
	}
	return nil
}

// beginLogin starts a WebAuthn login (assertion) ceremony using client-side
// discoverable ("resident key") credentials: unlike beginRegistration, this
// endpoint doesn't know which account is signing in yet — the mockup's
// login page (templates/login.html) has no username field, just a single
// "Continue with passkey" button — so the authenticator itself surfaces
// whichever of the user's stored discoverable credentials matches this RP,
// and finishLogin resolves the account afterward from the assertion's
// userHandle (see DiscoverableUserHandler). The resulting SessionData is
// stashed the same way beginRegistration stashes its own (a ceremony Session
// keyed by loginCeremonyCookie, ttl ceremonyTTL).
func beginLogin(w http.ResponseWriter, r *http.Request, wa *webauthn.WebAuthn, ceremonies SessionStore) (*protocol.CredentialAssertion, error) {
	assertion, sessionData, err := wa.BeginDiscoverableLogin()
	if err != nil {
		return nil, fmt.Errorf("web: begin login: %w", err)
	}
	data, err := json.Marshal(sessionData)
	if err != nil {
		return nil, fmt.Errorf("web: encode login ceremony: %w", err)
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
		Name:     loginCeremonyCookie,
		Value:    sess.ID,
		Path:     "/login",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300, // 5 minutes: matches ceremonyTTL.
	})
	return assertion, nil
}

// finishLogin completes the ceremony beginLogin started: it reads
// loginCeremonyCookie off r to find the stashed SessionData, asks wa to
// verify r's body (the browser's assertion response) — resolving the
// signing-in account from the assertion's userHandle via users.Get — and,
// only on success, issues a new signed-in session cookie (ttl) on w.
//
// Clone detection (this task's signCount requirement): go-webauthn's
// Authenticator.UpdateCounter (called internally by FinishDiscoverableLogin)
// already implements the spec's comparison — it sets CloneWarning when the
// assertion's counter is <= the credential's last stored SignCount, unless
// both are zero (some authenticators never implement a counter and always
// report 0, which is legitimate, not a clone signal). A CloneWarning here
// means the same signature counter value was presented twice, the textbook
// sign of a cloned authenticator, so the login is rejected outright and the
// stored SignCount is left untouched (an attacker's replay must not get to
// "use up" a counter value the legitimate authenticator hasn't reached yet).
// Only on a clean (non-regressed) counter is the credential's stored
// SignCount advanced and persisted.
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

	var matched *User
	cred, err := wa.FinishDiscoverableLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
		u, ok := users.Get(string(userHandle))
		if !ok {
			return nil, fmt.Errorf("web: unknown credential user")
		}
		matched = u
		return u, nil
	}, sessionData, r)
	if err != nil {
		return fmt.Errorf("web: finish login: %w", err)
	}
	if matched == nil {
		return fmt.Errorf("web: internal error: no user resolved for login")
	}
	if cred.Authenticator.CloneWarning {
		return fmt.Errorf("web: authenticator signature counter did not advance (possible cloned credential); login rejected")
	}

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
