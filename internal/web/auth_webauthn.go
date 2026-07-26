//go:build web

package web

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"serverwatch/internal/config"
)

// enrollSessionCookie names the cookie beginRegistration/finishRegistration
// use to correlate the two halves of a registration ceremony. See
// regCeremonies' doc for why this is a cookie-keyed in-memory map rather
// than the real session store (that's Task 5/#61).
const enrollSessionCookie = "sw_enroll"

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

// ceremonyTTL bounds how long a stashed ceremony is retained/valid: it
// mirrors the enrollSessionCookie MaxAge (5 min) so a stash entry never
// outlives the cookie that references it.
const ceremonyTTL = 5 * time.Minute

// ceremonyMaxEntries caps the stash size as a hard backstop against a flood
// of /enroll/begin calls arriving faster than they expire (each ceremony is
// tiny, but the endpoint is unauthenticated). Once the cap is hit, put
// refuses new ceremonies rather than growing without bound; legitimate
// enrollment is a single interactive request, so this ceiling is far above
// any honest concurrency.
const ceremonyMaxEntries = 1024

// regCeremony is what beginRegistration stashes and finishRegistration
// retrieves: the go-webauthn SessionData the ceremony needs to verify the
// attestation, plus the pending *User being enrolled (not yet persisted —
// finishRegistration's store.Put is the first time it's written).
type regCeremony struct {
	user    *User
	session *webauthn.SessionData
	// expires is when this entry becomes eligible for eviction (put-time +
	// ceremonyTTL); take treats an expired entry as absent.
	expires time.Time
}

// ceremonyStash is a TEMPORARY in-memory, single-process stand-in for the
// real session store (Task 5/#61 — TODO(#61): replace this with the
// server-side session store once it lands; this map doesn't survive a
// process restart and isn't shared across multiple web server instances,
// neither of which matters for a single-process daemon serving a
// short-lived registration ceremony but both of which a real session store
// must handle). Keyed by a random id set in enrollSessionCookie, a
// short-lived cookie scoped to /enroll.
//
// Because /enroll/begin is unauthenticated, the stash bounds its own growth:
// every put first evicts expired entries (see ceremonyTTL) and, if still at
// ceremonyMaxEntries, refuses the new ceremony — so an anonymous caller
// looping POST /enroll/begin can't grow this map without limit (pre-auth
// DoS). now is overridable so a test can drive expiry deterministically.
type ceremonyStash struct {
	mu   sync.Mutex
	data map[string]regCeremony
	now  func() time.Time
}

// regCeremonies is the package-level stash beginRegistration/
// finishRegistration share. A package-level var (rather than threading a
// store through Deps) is acceptable here because it's explicitly temporary
// scaffolding removed in #61, not a persisted or user-facing store.
var regCeremonies = &ceremonyStash{data: make(map[string]regCeremony)}

// clock returns the stash's time source, defaulting to time.Now when unset
// (the production package-level regCeremonies leaves now nil).
func (s *ceremonyStash) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// evictExpiredLocked drops every entry whose TTL has elapsed. Callers must
// hold s.mu. O(n) over the map, but n is bounded by ceremonyMaxEntries and
// this only runs on put (a low-frequency, interactive path).
func (s *ceremonyStash) evictExpiredLocked(now time.Time) {
	for id, c := range s.data {
		if !c.expires.After(now) {
			delete(s.data, id)
		}
	}
}

// put stores c under a fresh random id and returns it, first evicting
// expired entries and refusing (error) if the stash is still at its size
// cap — see ceremonyStash's doc.
func (s *ceremonyStash) put(c regCeremony) (string, error) {
	id, err := newRandomID(18)
	if err != nil {
		return "", err
	}
	now := s.clock()
	c.expires = now.Add(ceremonyTTL)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked(now)
	if len(s.data) >= ceremonyMaxEntries {
		return "", fmt.Errorf("web: too many pending enrollments; try again shortly")
	}
	s.data[id] = c
	return id, nil
}

// take retrieves and deletes the ceremony stored under id (one-shot: a
// cookie value is only ever valid for a single finishRegistration call). An
// entry whose TTL has elapsed is treated as absent (and removed).
func (s *ceremonyStash) take(id string) (regCeremony, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.data[id]
	if !ok {
		return regCeremony{}, false
	}
	delete(s.data, id)
	if !c.expires.After(s.clock()) {
		return regCeremony{}, false
	}
	return c, true
}

// beginRegistration starts a WebAuthn registration ceremony for u: it asks
// wa for a fresh challenge/options (creation), stashes the resulting
// SessionData (see ceremonyStash) alongside u, and sets enrollSessionCookie
// on w so the browser echoes the same ceremony id back to
// /enroll/finish. The caller (enrollBeginHandler, routes.go) is responsible
// for JSON-encoding the returned creation options onto the response body.
func beginRegistration(w http.ResponseWriter, r *http.Request, wa *webauthn.WebAuthn, u *User) (*protocol.CredentialCreation, error) {
	creation, session, err := wa.BeginRegistration(u)
	if err != nil {
		return nil, fmt.Errorf("web: begin registration: %w", err)
	}
	id, err := regCeremonies.put(regCeremony{user: u, session: session})
	if err != nil {
		return nil, err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     enrollSessionCookie,
		Value:    id,
		Path:     "/enroll",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300, // 5 minutes: ample time for a passkey prompt, short-lived scaffolding (see ceremonyStash doc).
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
func finishRegistration(w http.ResponseWriter, r *http.Request, wa *webauthn.WebAuthn, store UserStore) error {
	cookie, err := r.Cookie(enrollSessionCookie)
	if err != nil {
		return fmt.Errorf("web: missing or expired enrollment session: %w", err)
	}
	ceremony, ok := regCeremonies.take(cookie.Value)
	if !ok {
		return fmt.Errorf("web: enrollment session not found (expired or already used)")
	}
	// Clear the cookie regardless of outcome below: it's single-use either way.
	http.SetCookie(w, &http.Cookie{
		Name:     enrollSessionCookie,
		Value:    "",
		Path:     "/enroll",
		MaxAge:   -1,
		HttpOnly: true,
	})

	cred, err := wa.FinishRegistration(ceremony.user, *ceremony.session, r)
	if err != nil {
		return fmt.Errorf("web: finish registration: %w", err)
	}

	ceremony.user.Credentials = append(ceremony.user.Credentials, Credential{
		ID:         cred.ID,
		PublicKey:  cred.PublicKey,
		SignCount:  cred.Authenticator.SignCount,
		Transports: transportsToStrings(cred.Transport),
	})
	if err := store.Put(ceremony.user); err != nil {
		return fmt.Errorf("web: persist user: %w", err)
	}
	return nil
}
