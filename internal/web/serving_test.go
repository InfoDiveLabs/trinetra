package web

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"serverwatch/internal/config"
)

// webCfg builds a *config.Config with just the web.* fields serving.go cares
// about set directly (bypassing Config.Set's own enum validation, so these
// tests exercise validateOrigin's checks in isolation rather than
// internal/config's).
func webCfg(mode, rpID, origin, autocertDomains, tlsCert, tlsKey string) *config.Config {
	c := config.Default()
	c.Web.Mode = mode
	c.Web.RPID = rpID
	c.Web.Origin = origin
	c.Web.AutocertDomains = autocertDomains
	c.Web.TLSCert = tlsCert
	c.Web.TLSKey = tlsKey
	return c
}

// TestValidateOriginAllowsEmptyInProxyMode pins the proxy-mode exemption: an
// operator fronting the daemon with a local reverse proxy (Cloudflare
// Tunnel/nginx/Caddy) need not set rp_id/origin at all -- proxy mode derives
// them per-request instead (see requestOrigin).
func TestValidateOriginAllowsEmptyInProxyMode(t *testing.T) {
	cfg := webCfg("proxy", "", "", "", "", "")
	if err := validateOrigin(cfg); err != nil {
		t.Fatalf("validateOrigin(proxy, empty rp_id/origin) = %v, want nil", err)
	}
}

// TestValidateOriginAcceptsConsistentConfig pins the happy path for both
// TLS-terminating modes: rp_id equal to origin's host, plus that mode's
// other required fields present.
func TestValidateOriginAcceptsConsistentConfig(t *testing.T) {
	cases := []*config.Config{
		webCfg("manual", "monitor.example.com", "https://monitor.example.com", "", "/etc/serverwatch/tls.crt", "/etc/serverwatch/tls.key"),
		webCfg("autocert", "monitor.example.com", "https://monitor.example.com", "monitor.example.com", "", ""),
		webCfg("proxy", "monitor.example.com", "https://monitor.example.com", "", "", ""),
	}
	for _, cfg := range cases {
		if err := validateOrigin(cfg); err != nil {
			t.Errorf("validateOrigin(%+v) = %v, want nil", cfg.Web, err)
		}
	}
}

// TestValidateOriginRejectsMismatchedHost pins the core passkey-security
// invariant from the design doc: "rp_id/origin must match the public
// hostname in every mode, validated at startup -- refuse to start on
// mismatch."
func TestValidateOriginRejectsMismatchedHost(t *testing.T) {
	cfg := webCfg("manual", "monitor.example.com", "https://other.example.com", "", "/tls.crt", "/tls.key")
	if err := validateOrigin(cfg); err == nil {
		t.Fatal("validateOrigin(mismatched rp_id/origin host) = nil, want error")
	}
}

// TestValidateOriginRejectsEmptyInNonProxyModes covers both autocert and
// manual: unlike proxy mode, both require rp_id and origin to be set
// explicitly since there's no per-request reverse-proxy signal to derive
// them from.
func TestValidateOriginRejectsEmptyInNonProxyModes(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
	}{
		{"autocert empty rp_id", webCfg("autocert", "", "https://monitor.example.com", "monitor.example.com", "", "")},
		{"autocert empty origin", webCfg("autocert", "monitor.example.com", "", "monitor.example.com", "", "")},
		{"manual empty rp_id", webCfg("manual", "", "https://monitor.example.com", "", "/tls.crt", "/tls.key")},
		{"manual empty origin", webCfg("manual", "monitor.example.com", "", "", "/tls.crt", "/tls.key")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateOrigin(tc.cfg); err == nil {
				t.Fatalf("validateOrigin(%+v) = nil, want error", tc.cfg.Web)
			}
		})
	}
}

// TestValidateOriginRequiresAutocertDomains pins autocert mode's other
// prerequisite: HostPolicy needs at least one domain to whitelist.
func TestValidateOriginRequiresAutocertDomains(t *testing.T) {
	cfg := webCfg("autocert", "monitor.example.com", "https://monitor.example.com", "", "", "")
	if err := validateOrigin(cfg); err == nil {
		t.Fatal("validateOrigin(autocert, empty autocert_domains) = nil, want error")
	}
}

// TestValidateOriginRequiresTLSCertAndKey pins manual mode's other
// prerequisite: http.Server.ServeTLS needs both a cert and a key file.
func TestValidateOriginRequiresTLSCertAndKey(t *testing.T) {
	cfg := webCfg("manual", "monitor.example.com", "https://monitor.example.com", "", "", "")
	if err := validateOrigin(cfg); err == nil {
		t.Fatal("validateOrigin(manual, empty tls_cert/tls_key) = nil, want error")
	}
}

// TestValidateOriginRejectsUnknownMode is a defensive check: validateOrigin
// is also reachable with a *config.Config built directly (not through
// Config.Set's own web.mode enum validation), so it must not silently treat
// an unrecognized mode as one of the three known ones.
func TestValidateOriginRejectsUnknownMode(t *testing.T) {
	cfg := webCfg("bogus", "monitor.example.com", "https://monitor.example.com", "", "", "")
	if err := validateOrigin(cfg); err == nil {
		t.Fatal("validateOrigin(unknown mode) = nil, want error")
	}
}

// TestRequestOriginTrustsForwardedHeadersOnlyWhenAllowed pins the proxy-mode
// contract: X-Forwarded-Proto/Host are only honored when trustForwarded is
// true (the caller's job to gate on the listener being loopback-bound, see
// isLoopbackAddr), otherwise requestOrigin must fall back to the request's
// own Host/TLS state so an untrusted client can't spoof its apparent origin.
func TestRequestOriginTrustsForwardedHeadersOnlyWhenAllowed(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "127.0.0.1:8088"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "monitor.example.com")

	if got, want := requestOrigin(r, true), "https://monitor.example.com"; got != want {
		t.Errorf("requestOrigin(trusted) = %q, want %q", got, want)
	}
	if got, want := requestOrigin(r, false), "http://127.0.0.1:8088"; got != want {
		t.Errorf("requestOrigin(untrusted) = %q, want %q", got, want)
	}
}

// TestIsLoopbackAddr pins the gate requestOrigin's trustForwarded relies on:
// only a listen address actually bound to loopback (where only same-host
// processes, i.e. a local reverse proxy, can reach it) is trusted to have
// its forwarded headers honored.
func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8088", true},
		{"localhost:8088", true},
		{"[::1]:8088", true},
		{"0.0.0.0:8088", false},
		{"192.168.1.5:8088", false},
		{":8088", false},
	}
	for _, tc := range tests {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestSecurityHeadersSetsCSPNonceContentTypeReferrer pins securityHeaders'
// always-on headers: a Content-Security-Policy scoped to self plus a
// per-response nonce for the single htmx boot script, X-Content-Type-
// Options, and Referrer-Policy. HSTS is covered separately (TLS-only, see
// TestSecurityHeadersHSTSOnlyOverTLS) since httptest requests aren't TLS by
// default.
func TestSecurityHeadersSetsCSPNonceContentTypeReferrer(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	csp := rr.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy header missing")
	}
	if !strings.Contains(csp, "script-src 'self' 'nonce-") {
		t.Errorf("CSP %q missing script-src 'self' 'nonce-...'", csp)
	}
	// script-src must stay STRICT (nonce-based, never unsafe-inline) -- that's
	// the directive that actually matters for XSS. style-src, by contrast,
	// deliberately allows 'unsafe-inline' because the ported mockup uses
	// inline style="…" pervasively (see securityHeaders' comment).
	scriptDir, styleDir := cspDirective(csp, "script-src"), cspDirective(csp, "style-src")
	if strings.Contains(scriptDir, "unsafe-inline") {
		t.Errorf("script-src %q must not contain unsafe-inline", scriptDir)
	}
	if !strings.Contains(styleDir, "unsafe-inline") {
		t.Errorf("style-src %q must contain 'unsafe-inline' for the mockup's inline styles", styleDir)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rr.Header().Get("Referrer-Policy"); got == "" {
		t.Error("Referrer-Policy header missing")
	}
}

// cspDirective returns the single CSP directive (e.g. "script-src") from a
// full policy string, or "" if absent -- lets a test assert on one directive
// without a false match from another directive's value.
func cspDirective(csp, name string) string {
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, name+" ") || d == name {
			return d
		}
	}
	return ""
}

// TestBaseTemplateHasNoInlineEventHandlers pins that the strict script-src
// (no unsafe-inline) can actually hold: base.html must carry no inline
// on*="..." event handlers (they'd be blocked by CSP and silently break),
// and the theme button must instead expose the id app.js binds via
// addEventListener. A regression here (someone re-adding onclick=) would
// break the theme toggle under any CSP-enforcing browser.
func TestBaseTemplateHasNoInlineEventHandlers(t *testing.T) {
	b, err := templatesFS.ReadFile("templates/base.html")
	if err != nil {
		t.Fatalf("read base.html: %v", err)
	}
	src := string(b)
	// Match inline event-handler attributes (onclick=, onchange=, ...) while
	// not tripping on unrelated attrs; the leading space/quote/> boundary
	// avoids matching substrings inside other attribute values.
	re := regexp.MustCompile(`(?i)[\s"'>]on[a-z]+\s*=`)
	if loc := re.FindString(src); loc != "" {
		t.Errorf("base.html contains an inline event handler %q; move it to app.js (strict CSP blocks inline JS)", strings.TrimSpace(loc))
	}
	if !strings.Contains(src, `id="themeBtn"`) {
		t.Error(`base.html theme button missing id="themeBtn" (app.js binds its click via addEventListener)`)
	}
}

// TestAppJSBindsThemeButton pins the other half of the theme-toggle move:
// app.js must wire #themeBtn via addEventListener rather than relying on the
// removed inline onclick.
func TestAppJSBindsThemeButton(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "themeBtn") || !strings.Contains(src, "addEventListener") {
		t.Error("app.js must bind #themeBtn via addEventListener")
	}
}

// TestSecurityHeadersHSTSOnlyOverTLS pins that Strict-Transport-Security is
// only ever sent when the current request actually arrived over TLS
// (autocert/manual modes, where this process itself terminates TLS) -- never
// in proxy mode, where our own server always sees plain HTTP even though a
// front proxy may terminate TLS upstream (sending it there would be an
// incorrect promise about a connection this process didn't make).
func TestSecurityHeadersHSTSOnlyOverTLS(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rr.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS over plain HTTP = %q, want empty", got)
	}

	rrTLS := httptest.NewRecorder()
	reqTLS := httptest.NewRequest(http.MethodGet, "/", nil)
	reqTLS.TLS = &tls.ConnectionState{}
	h.ServeHTTP(rrTLS, reqTLS)
	if got := rrTLS.Header().Get("Strict-Transport-Security"); got == "" {
		t.Error("HSTS over TLS missing")
	}
}

// TestSecurityHeadersNoncePropagatesToTemplate is the end-to-end pin the
// brief calls for: the nonce securityHeaders puts in the CSP header must be
// the exact same value the rendered page's boot script carries, so the
// browser actually executes it under the CSP that names it.
func TestSecurityHeadersNoncePropagatesToTemplate(t *testing.T) {
	// GET /enroll (an anonymous, always-reachable page that renders a
	// nonce'd boot script) rather than GET /, which is now viewer+ and would
	// redirect an anonymous request to /login before rendering any page.
	h := newHandler(testDeps(t))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/enroll", nil))

	csp := rr.Header().Get("Content-Security-Policy")
	start := strings.Index(csp, "'nonce-")
	if start == -1 {
		t.Fatalf("CSP %q missing nonce directive", csp)
	}
	start += len("'nonce-")
	end := strings.Index(csp[start:], "'")
	if end == -1 {
		t.Fatalf("CSP %q malformed nonce directive", csp)
	}
	nonce := csp[start : start+end]
	if nonce == "" {
		t.Fatal("empty nonce in CSP header")
	}

	if !strings.Contains(rr.Body.String(), `nonce="`+nonce+`"`) {
		t.Errorf("page body missing nonce=%q matching CSP header:\n%s", nonce, rr.Body.String())
	}
}
