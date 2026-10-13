package main

import (
	"fmt"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// webSetupStep enumerates the guided "set up the web UI" wizard's screens, in the order the
// user walks through them.
type webSetupStep int

const (
	webSetupMode webSetupStep = iota
	webSetupListen
	webSetupDomain
	webSetupRPID
	webSetupOrigin
	webSetupCert
	webSetupKey
	webSetupConfirm
	webSetupResult
)

// webModeChoices are the three internal/web serving modes a user can pick (see
// internal/web/serving.go and config.validWebModes), in the order they are offered.
var webModeChoices = []string{"proxy", "autocert", "manual"}

// webSetupAnswers accumulates the wizard's field-by-field input.
type webSetupAnswers struct {
	Mode   string
	Listen string
	Domain string
	RPID   string
	Origin string
	// TLSCert/TLSKey are only collected (and only applied) in manual mode: internal/web's
	// manual serving mode reads both as PEM file paths at startup.
	TLSCert string
	TLSKey  string
}

// deriveWebDefaults fills in listen/rp_id/origin defaults once the user has picked a mode:
// proxy keeps the existing local-only listen address and leaves rp_id/origin blank.
func deriveWebDefaults(mode string) (listen string) {
	switch mode {
	case "autocert", "manual":
		return "0.0.0.0:8443"
	default:
		return "127.0.0.1:8088"
	}
}

// deriveRPIDOrigin derives rp_id/origin defaults from a domain the user just typed, for the
// autocert/manual steps.
func deriveRPIDOrigin(domain string) (rpid, origin string) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return "", ""
	}
	return domain, "https://" + domain
}

// validateManualPath rejects a blank manual-mode TLS cert/key path.
func validateManualPath(label, val string) error {
	if strings.TrimSpace(val) == "" {
		return fmt.Errorf("%s is required for manual mode", label)
	}
	return nil
}

// applyWebSetup applies ans onto cfg via config.Config.Set, the same validated setter
// `trinetra-ctl config set`/the web config page use.
func applyWebSetup(cfg *config.Config, ans webSetupAnswers) error {
	if err := cfg.Set("web.enabled", "true"); err != nil {
		return err
	}
	if err := cfg.Set("web.mode", ans.Mode); err != nil {
		return err
	}
	if err := cfg.Set("web.listen", ans.Listen); err != nil {
		return err
	}
	if err := cfg.Set("web.rp_id", ans.RPID); err != nil {
		return err
	}
	if err := cfg.Set("web.origin", ans.Origin); err != nil {
		return err
	}
	if ans.Mode == "autocert" && ans.Domain != "" {
		if err := cfg.Set("web.autocert_domains", ans.Domain); err != nil {
			return err
		}
	}
	if ans.Mode == "manual" {
		if err := cfg.Set("web.tls_cert", ans.TLSCert); err != nil {
			return err
		}
		if err := cfg.Set("web.tls_key", ans.TLSKey); err != nil {
			return err
		}
	}
	return nil
}

// webSetupSummary renders ans as the confirm screen's review text.
func webSetupSummary(ans webSetupAnswers) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mode:    %s\n", ans.Mode)
	fmt.Fprintf(&b, "listen:  %s\n", ans.Listen)
	if ans.Domain != "" {
		fmt.Fprintf(&b, "domain:  %s\n", ans.Domain)
	}
	fmt.Fprintf(&b, "rp_id:   %s\n", valueOrDash(ans.RPID))
	fmt.Fprintf(&b, "origin:  %s\n", valueOrDash(ans.Origin))
	if ans.Mode == "manual" {
		fmt.Fprintf(&b, "cert:    %s\n", valueOrDash(ans.TLSCert))
		fmt.Fprintf(&b, "key:     %s\n", valueOrDash(ans.TLSKey))
	}
	if ans.Mode == "proxy" && (ans.RPID == "" || ans.Origin == "") {
		b.WriteString("\nproxy mode derives rp_id/origin from your reverse proxy's\n")
		b.WriteString("X-Forwarded-Host/Proto headers. If your proxy does not forward\n")
		b.WriteString("them, set them explicitly after setup:\n")
		b.WriteString("  trinetra config set web.rp_id <host>\n")
		b.WriteString("  trinetra config set web.origin https://<host>\n")
	}
	return b.String()
}

func valueOrDash(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
