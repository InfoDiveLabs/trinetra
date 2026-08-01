package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"serverwatch/internal/config"
)

// effectiveWebMode returns cfg.Web.Mode, or "proxy" (the documented default,
// see internal/config.Config.Default) if the config was built directly
// (e.g. a bare *config.Config{} in a test) rather than through Default()/
// Load(), which always backfill it.
func effectiveWebMode(cfg *config.Config) string {
	if cfg.Web.Mode == "" {
		return "proxy"
	}
	return cfg.Web.Mode
}

// validateOrigin fail-fasts at startup (called by Start before binding, see
// server.go) on any config that would let the web server bind with a
// passkey-unsafe rp_id/origin, or with a serving mode missing the fields it
// needs to actually terminate TLS. Per the design doc's security checklist:
// "rp_id/origin must match the public hostname in every mode ... validated
// at startup -- refuse to start on mismatch."
func validateOrigin(cfg *config.Config) error {
	mode := effectiveWebMode(cfg)
	switch mode {
	case "proxy", "autocert", "manual":
	default:
		return fmt.Errorf("web.mode %q invalid: want one of proxy|autocert|manual", cfg.Web.Mode)
	}

	rpID, origin := cfg.Web.RPID, cfg.Web.Origin

	// proxy mode alone may leave rp_id/origin unset: it derives them
	// per-request from the trusted local reverse proxy's forwarded headers
	// instead (see requestOrigin). autocert/manual terminate TLS themselves
	// against a fixed public hostname, so both must be configured explicitly.
	if mode != "proxy" {
		if rpID == "" {
			return fmt.Errorf("web.rp_id must be set in %s mode", mode)
		}
		if origin == "" {
			return fmt.Errorf("web.origin must be set in %s mode", mode)
		}
	}

	// Whenever both are set (required above for non-proxy, optional but
	// still checked for proxy), rp_id must equal origin's host: this is the
	// same host WebAuthn's relying-party validation enforces at ceremony
	// time (Task 4/#60), so a mismatch here would only be caught later, at
	// the worst possible moment (a user's browser rejecting every passkey).
	if rpID != "" && origin != "" {
		u, err := url.Parse(origin)
		if err != nil {
			return fmt.Errorf("web.origin %q invalid: %w", origin, err)
		}
		host := u.Hostname()
		if host == "" {
			// origin had no scheme (bare "host[:port]"); treat the whole
			// thing minus any port as the host to compare against rp_id.
			if h, _, splitErr := net.SplitHostPort(origin); splitErr == nil {
				host = h
			} else {
				host = origin
			}
		}
		if !strings.EqualFold(host, rpID) {
			return fmt.Errorf("web.rp_id %q does not match web.origin %q (host %q)", rpID, origin, host)
		}
	}

	switch mode {
	case "autocert":
		if cfg.Web.AutocertDomains == "" {
			return fmt.Errorf("web.autocert_domains must be set in autocert mode")
		}
	case "manual":
		if cfg.Web.TLSCert == "" || cfg.Web.TLSKey == "" {
			return fmt.Errorf("web.tls_cert and web.tls_key must both be set in manual mode")
		}
	}

	return nil
}

// isLoopbackAddr reports whether addr (a "host:port" listen address, e.g.
// Deps.Listen) is bound to loopback -- the only case in which trusting
// X-Forwarded-Proto/X-Forwarded-Host from an incoming request is safe,
// because only a same-host process (a local reverse proxy: Cloudflare
// Tunnel, nginx, Caddy) can dial loopback directly. A wildcard/public bind
// address could receive those headers from any client on the network, which
// would let it spoof its own apparent origin.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		// "" or ":8088" binds all interfaces, not loopback.
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requestOrigin derives the scheme+host a request effectively arrived as.
// When trustForwarded is true (the listener is loopback-bound, see
// isLoopbackAddr) it honors X-Forwarded-Proto/X-Forwarded-Host, the headers
// a local reverse proxy sets to describe the original client-facing
// request; otherwise it falls back to the request's own TLS state and Host
// header, since an untrusted, directly-reachable listener must not let a
// client spoof its origin via those headers.
func requestOrigin(r *http.Request, trustForwarded bool) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if trustForwarded {
		if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
			scheme = fp
		}
		if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
			host = fh
		}
	}
	return scheme + "://" + host
}

// requestOriginCtxKey is the unexported context key withRequestOrigin
// stores the per-request derived origin under.
type requestOriginCtxKey struct{}

// withRequestOrigin wraps next so every request's derived origin (see
// requestOrigin) is available to downstream handlers via
// requestOriginFromContext -- a future task (WebAuthn ceremonies, #60/#61)
// needs this to validate the browser-reported origin against what the
// server itself considers authoritative in proxy mode.
func withRequestOrigin(trustForwarded bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), requestOriginCtxKey{}, requestOrigin(r, trustForwarded))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestOriginFromContext returns the origin withRequestOrigin stored for
// this request, or "" if none was stored (e.g. a handler invoked directly in
// a test without going through newHandler's middleware chain).
func requestOriginFromContext(r *http.Request) string {
	v, _ := r.Context().Value(requestOriginCtxKey{}).(string)
	return v
}

// listenAndServe binds and serves handler per cfg.Web.Mode, returning a stop
// func that gracefully shuts down whatever it started. Start calls this
// after validateOrigin has already confirmed the mode's required fields are
// present, so the mode-specific branches below don't re-check them.
func listenAndServe(d Deps, handler http.Handler) (stop func(), err error) {
	cfg := d.Cfg()
	switch effectiveWebMode(cfg) {
	case "manual":
		return serveManual(d, handler, cfg.Web.TLSCert, cfg.Web.TLSKey)
	case "autocert":
		return serveAutocert(d, handler, cfg.Web.AutocertDomains)
	default: // "proxy"
		return serveProxy(d, handler)
	}
}

// shutdownServer returns a stop func that gracefully shuts down srv with a
// bounded timeout, shared by all three serving modes below.
func shutdownServer(srv *http.Server) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// serveProxy binds plain HTTP on d.Listen: the mode a local reverse proxy
// (Cloudflare Tunnel/nginx/Caddy) fronts with its own TLS termination.
// Forwarded headers are trusted only when d.Listen is loopback-bound (see
// isLoopbackAddr), so requestOrigin can't be spoofed by a client reaching
// the listener directly.
func serveProxy(d Deps, handler http.Handler) (stop func(), err error) {
	ln, err := net.Listen("tcp", d.Listen)
	if err != nil {
		return nil, fmt.Errorf("web: listen %s: %w", d.Listen, err)
	}
	srv := &http.Server{Handler: withRequestOrigin(isLoopbackAddr(d.Listen), handler)}
	go srv.Serve(ln) //nolint:errcheck // Shutdown below always yields http.ErrServerClosed; nothing else to log yet.
	return shutdownServer(srv), nil
}

// serveManual binds TLS on d.Listen using an operator-provided cert/key
// pair. validateOrigin already confirmed both are non-empty; a missing or
// unreadable file surfaces as ServeTLS's own error, logged the same way
// serveProxy's net.Listen failures would be by the caller (web.Start,
// invoked by the serverwatch-web binary).
func serveManual(d Deps, handler http.Handler, certFile, keyFile string) (stop func(), err error) {
	ln, err := net.Listen("tcp", d.Listen)
	if err != nil {
		return nil, fmt.Errorf("web: listen %s: %w", d.Listen, err)
	}
	srv := &http.Server{Handler: withRequestOrigin(false, handler)}
	go srv.ServeTLS(ln, certFile, keyFile) //nolint:errcheck // Shutdown below always yields http.ErrServerClosed; a bad cert/key pair fails per-connection, nothing else to log yet.
	return shutdownServer(srv), nil
}

// serveAutocert binds TLS on d.Listen using golang.org/x/crypto/acme/
// autocert to automatically obtain and renew a Let's Encrypt certificate
// for domainsCSV (a comma-separated allowlist -- validateOrigin already
// confirmed it's non-empty). autocert.Manager's HTTP-01 challenge handler is
// additionally served on :80, which this mode requires be reachable from
// the public internet (see the design doc's "Serving modes" section).
func serveAutocert(d Deps, handler http.Handler, domainsCSV string) (stop func(), err error) {
	domains := splitCSV(domainsCSV)
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(domains...),
		Cache:      autocert.DirCache(filepath.Join(d.StateDir, "autocert")),
	}

	ln, err := net.Listen("tcp", d.Listen)
	if err != nil {
		return nil, fmt.Errorf("web: listen %s: %w", d.Listen, err)
	}
	srv := &http.Server{
		Handler:   withRequestOrigin(false, handler),
		TLSConfig: m.TLSConfig(),
	}
	go srv.ServeTLS(ln, "", "") //nolint:errcheck // cert/key come from TLSConfig.GetCertificate via m, not files.

	challengeSrv := &http.Server{Addr: ":80", Handler: m.HTTPHandler(nil)}
	go func() {
		// The :80 HTTP-01 challenge listener is required for autocert to
		// obtain/renew certificates; a bind failure (e.g. :80 already taken,
		// or no CAP_NET_BIND_SERVICE) would otherwise silently block issuance
		// with no clue why, so surface it. ErrServerClosed is the normal
		// outcome of the stop func's Shutdown below and isn't worth logging.
		if err := challengeSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "web: autocert HTTP-01 challenge listener on :80 failed:", err)
		}
	}()

	return func() {
		shutdownServer(srv)()
		shutdownServer(challengeSrv)()
	}, nil
}

// splitCSV parses a comma-separated list (web.autocert_domains' storage
// shape, matching internal/config's other comma-separated keys like
// channel include_kinds), trimming whitespace and dropping empty elements.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
