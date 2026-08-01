package main

import (
	"fmt"
	"strings"

	"serverwatch/internal/config"
)

// webSetupStep enumerates the guided "set up the web UI" wizard's screens,
// in the order the user walks through them. It is a sub-state of the top
// level tui step (stepSetupWeb) rather than a value on that same enum, so
// the wizard's own progression is independent of, and doesn't clutter, the
// top level home/setup switch in tui.go.
type webSetupStep int

const (
	webSetupMode webSetupStep = iota
	webSetupListen
	webSetupDomain
	webSetupRPID
	webSetupOrigin
	webSetupConfirm
	webSetupResult
)

// webModeChoices are the three internal/web serving modes a user can pick
// (see internal/web/serving.go and config.validWebModes), in the order they
// are offered: proxy first because it is the documented default and needs
// the least input (no domain/TLS material of its own).
var webModeChoices = []string{"proxy", "autocert", "manual"}

// webSetupAnswers accumulates the wizard's field-by-field input. It mirrors
// config.Config.Web's shape closely enough that applyWebSetup can turn it
// into config.Set calls almost one for one; keeping it a separate plain
// struct (rather than editing a *config.Config in place as the user types)
// means a half-finished wizard never touches the config that Home is
// showing, and the whole struct is trivially comparable in tests.
type webSetupAnswers struct {
	Mode   string
	Listen string
	Domain string
	RPID   string
	Origin string
}

// deriveWebDefaults fills in listen/rp_id/origin defaults once the user has
// picked a mode: proxy keeps the existing local-only listen address and
// leaves rp_id/origin blank (internal/web derives them per-request from the
// trusted reverse proxy's forwarded headers in that mode -- see
// validateOrigin's doc in internal/web/serving.go); autocert/manual bind a
// public 0.0.0.0:443-style address and need a fixed public hostname, so
// domain (once entered) seeds rp_id verbatim and origin as "https://" +
// domain, the same pairing internal/web's validateOrigin requires (rp_id
// must equal origin's host).
func deriveWebDefaults(mode string) (listen string) {
	switch mode {
	case "autocert", "manual":
		return "0.0.0.0:8443"
	default:
		return "127.0.0.1:8088"
	}
}

// deriveRPIDOrigin derives rp_id/origin defaults from a domain the user
// just typed, for the autocert/manual steps. Both fall back to empty when
// domain is empty so an editor field simply starts blank rather than
// showing a stale suggestion.
func deriveRPIDOrigin(domain string) (rpid, origin string) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return "", ""
	}
	return domain, "https://" + domain
}

// needsDomain reports whether mode requires the domain/rp_id/origin/
// autocert_domains screens at all: proxy mode's rp_id/origin are optional
// (see deriveWebDefaults), so the wizard skips straight from the listen
// address to the confirm screen for it, matching internal/web/serving.go's
// validateOrigin which only requires rp_id/origin in non-proxy modes.
func needsDomain(mode string) bool {
	return mode == "autocert" || mode == "manual"
}

// applyWebSetup applies ans onto cfg via config.Config.Set, the same
// validated setter `serverwatch-ctl config set`/the web config page use, so
// the wizard gets exactly the same validation (web.listen host:port shape,
// web.mode allowlist) for free instead of duplicating it. It returns the
// first validation error encountered (nothing is applied, and nothing to
// undo, since cfg is the caller's fetched-fresh copy -- see setupApplyCmd).
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
	return nil
}

// webSetupSummary renders ans as the confirm screen's review text.
func webSetupSummary(ans webSetupAnswers) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mode:    %s\n", ans.Mode)
	fmt.Fprintf(&b, "listen:  %s\n", ans.Listen)
	if needsDomain(ans.Mode) {
		fmt.Fprintf(&b, "domain:  %s\n", ans.Domain)
	}
	fmt.Fprintf(&b, "rp_id:   %s\n", valueOrDash(ans.RPID))
	fmt.Fprintf(&b, "origin:  %s\n", valueOrDash(ans.Origin))
	return b.String()
}

func valueOrDash(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
