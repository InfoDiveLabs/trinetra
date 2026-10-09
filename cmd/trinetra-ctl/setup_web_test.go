// setup_web_test.go unit-tests setup_web.go's PURE logic (applyWebSetup,
// webSetupSummary, validateManualPath) against plain values, no terminal
// involved. The Bubble Tea glue that drives a user through these screens
// (tui.go) is exercised end to end in tui_test.go.
package main

import (
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// TestApplyWebSetupManualSetsCertAndKey asserts manual mode's tls_cert/ tls_key are applied
// via config.Set alongside the existing mode/listen/ rp_id/origin sets: without this.
func TestApplyWebSetupManualSetsCertAndKey(t *testing.T) {
	cfg := &config.Config{}
	ans := webSetupAnswers{
		Mode:    "manual",
		Listen:  "0.0.0.0:8443",
		RPID:    "example.com",
		Origin:  "https://example.com",
		TLSCert: "/etc/trinetra/tls/cert.pem",
		TLSKey:  "/etc/trinetra/tls/key.pem",
	}
	if err := applyWebSetup(cfg, ans); err != nil {
		t.Fatalf("applyWebSetup() error = %v, want nil", err)
	}
	if cfg.Web.TLSCert != "/etc/trinetra/tls/cert.pem" {
		t.Errorf("Web.TLSCert = %q, want /etc/trinetra/tls/cert.pem", cfg.Web.TLSCert)
	}
	if cfg.Web.TLSKey != "/etc/trinetra/tls/key.pem" {
		t.Errorf("Web.TLSKey = %q, want /etc/trinetra/tls/key.pem", cfg.Web.TLSKey)
	}
	if cfg.Web.Mode != "manual" {
		t.Errorf("Web.Mode = %q, want manual", cfg.Web.Mode)
	}
}

// TestApplyWebSetupProxyLeavesCertAndKeyUnset asserts proxy mode (which never collects
// cert/key) does not set them.
func TestApplyWebSetupProxyLeavesCertAndKeyUnset(t *testing.T) {
	cfg := &config.Config{}
	ans := webSetupAnswers{
		Mode:    "proxy",
		Listen:  "127.0.0.1:8088",
		TLSCert: "/stale/cert.pem",
		TLSKey:  "/stale/key.pem",
	}
	if err := applyWebSetup(cfg, ans); err != nil {
		t.Fatalf("applyWebSetup() error = %v, want nil", err)
	}
	if cfg.Web.TLSCert != "" {
		t.Errorf("Web.TLSCert = %q, want unset for proxy mode", cfg.Web.TLSCert)
	}
	if cfg.Web.TLSKey != "" {
		t.Errorf("Web.TLSKey = %q, want unset for proxy mode", cfg.Web.TLSKey)
	}
}

// TestApplyWebSetupAutocertLeavesCertAndKeyUnset mirrors the proxy case for autocert mode,
// which gets its certificate from Let's Encrypt, not a manually supplied file pair.
func TestApplyWebSetupAutocertLeavesCertAndKeyUnset(t *testing.T) {
	cfg := &config.Config{}
	ans := webSetupAnswers{
		Mode:   "autocert",
		Listen: "0.0.0.0:8443",
		Domain: "example.com",
		RPID:   "example.com",
		Origin: "https://example.com",
	}
	if err := applyWebSetup(cfg, ans); err != nil {
		t.Fatalf("applyWebSetup() error = %v, want nil", err)
	}
	if cfg.Web.TLSCert != "" || cfg.Web.TLSKey != "" {
		t.Errorf("Web.TLSCert/TLSKey = %q/%q, want both unset for autocert mode", cfg.Web.TLSCert, cfg.Web.TLSKey)
	}
}

// TestValidateManualPathRejectsBlank asserts the guard rejects an empty (or
// whitespace-only) path with a message naming the field.
func TestValidateManualPathRejectsBlank(t *testing.T) {
	for _, val := range []string{"", "   "} {
		err := validateManualPath("cert path", val)
		if err == nil {
			t.Fatalf("validateManualPath(%q) error = nil, want a validation error", val)
		}
		if !strings.Contains(err.Error(), "cert path") {
			t.Errorf("validateManualPath(%q) error = %q, want it to name the field", val, err.Error())
		}
	}
}

// TestValidateManualPathAcceptsNonBlank asserts any non-blank path passes: the wizard's
// guard only checks presence, not that the file exists or is readable.
func TestValidateManualPathAcceptsNonBlank(t *testing.T) {
	if err := validateManualPath("cert path", "/etc/trinetra/tls/cert.pem"); err != nil {
		t.Errorf("validateManualPath() error = %v, want nil", err)
	}
}

// TestWebSetupSummaryShowsCertAndKeyForManualMode asserts the confirm
// screen's review text includes the cert/key paths in manual mode.
func TestWebSetupSummaryShowsCertAndKeyForManualMode(t *testing.T) {
	ans := webSetupAnswers{
		Mode:    "manual",
		Listen:  "0.0.0.0:8443",
		Domain:  "example.com",
		RPID:    "example.com",
		Origin:  "https://example.com",
		TLSCert: "/etc/trinetra/tls/cert.pem",
		TLSKey:  "/etc/trinetra/tls/key.pem",
	}
	summary := webSetupSummary(ans)
	if !strings.Contains(summary, "/etc/trinetra/tls/cert.pem") {
		t.Errorf("summary missing cert path:\n%s", summary)
	}
	if !strings.Contains(summary, "/etc/trinetra/tls/key.pem") {
		t.Errorf("summary missing key path:\n%s", summary)
	}
}

// TestWebSetupSummaryOmitsCertAndKeyForOtherModes asserts proxy/autocert
// summaries never mention cert/key lines, even if ans carries stale values.
func TestWebSetupSummaryOmitsCertAndKeyForOtherModes(t *testing.T) {
	ans := webSetupAnswers{Mode: "proxy", Listen: "127.0.0.1:8088", TLSCert: "/stale/cert.pem"}
	summary := webSetupSummary(ans)
	if strings.Contains(summary, "cert:") {
		t.Errorf("proxy mode summary should not mention cert:\n%s", summary)
	}
}
