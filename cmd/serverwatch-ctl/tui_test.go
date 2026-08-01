package main

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// keyRunes builds a tea.KeyMsg for a single printable character, the same
// shape the real terminal reader produces for an ordinary keystroke.
func keyRunes(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// keyType builds a tea.KeyMsg for a non-rune key (enter, esc, up, down, ...).
func keyType(t tea.KeyType) tea.KeyMsg {
	return tea.KeyMsg{Type: t}
}

// typeString feeds each rune of s into m as individual keystrokes, mirroring
// how a real terminal delivers typed text one KeyMsg at a time, and returns
// the resulting model. Any tea.Cmd returned along the way is discarded: none
// of the wizard's text steps issue a command worth observing per keystroke
// (only the transition-triggering "enter" does, and callers press that
// separately so its Cmd isn't lost).
func typeString(t *testing.T, m tea.Model, s string) tea.Model {
	t.Helper()
	for _, r := range s {
		m, _ = m.Update(keyRunes(r))
	}
	return m
}

// runCmd executes a tea.Cmd synchronously and returns the tea.Msg it
// produces, standing in for the runtime loop a real tea.Program would run
// the Cmd on. Bubble Tea Cmds are plain `func() tea.Msg` values, so this is
// just a direct call -- no fake terminal or goroutine needed.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("runCmd: nil command")
	}
	return cmd()
}

// TestHomeKeyS enters the web setup wizard on the mode-selection screen.
func TestHomeKeyS(t *testing.T) {
	m := newModel(&fakeAPI{})
	var mm tea.Model = m
	mm, _ = mm.Update(keyRunes('s'))
	got := mm.(model)
	if got.step != stepSetupWeb {
		t.Fatalf("step = %v, want stepSetupWeb", got.step)
	}
	if got.wiz != webSetupMode {
		t.Fatalf("wiz = %v, want webSetupMode", got.wiz)
	}
}

// TestHomeKeyQQuits asserts 'q' on Home issues tea.Quit.
func TestHomeKeyQQuits(t *testing.T) {
	m := newModel(&fakeAPI{})
	var mm tea.Model = m
	mm, cmd := mm.Update(keyRunes('q'))
	if cmd == nil {
		t.Fatal("expected a quit command, got nil")
	}
	if !mm.(model).quitting {
		t.Error("quitting = false, want true")
	}
}

// TestWizardProxyModeSkipsDomain drives: press 's' (enter wizard), select
// "proxy" (already the cursor default, so just enter), type a listen
// address, enter -- and asserts the wizard lands directly on the confirm
// screen without visiting the domain/rp_id/origin steps, per needsDomain's
// contract for proxy mode.
func TestWizardProxyModeSkipsDomain(t *testing.T) {
	var mm tea.Model = newModel(&fakeAPI{})
	mm, _ = mm.Update(keyRunes('s'))
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept "proxy" (cursor index 0)

	got := mm.(model)
	if got.ans.Mode != "proxy" {
		t.Fatalf("ans.Mode = %q, want proxy", got.ans.Mode)
	}
	if got.wiz != webSetupListen {
		t.Fatalf("wiz = %v, want webSetupListen", got.wiz)
	}
	if got.listenIn.Value() != "127.0.0.1:8088" {
		t.Errorf("default listen = %q, want 127.0.0.1:8088", got.listenIn.Value())
	}

	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept the default listen address
	got = mm.(model)
	if got.wiz != webSetupConfirm {
		t.Fatalf("wiz = %v, want webSetupConfirm (proxy mode skips domain/rp_id/origin)", got.wiz)
	}
	if got.ans.Listen != "127.0.0.1:8088" {
		t.Errorf("ans.Listen = %q, want 127.0.0.1:8088", got.ans.Listen)
	}
	if got.ans.RPID != "" || got.ans.Origin != "" {
		t.Errorf("proxy mode should leave rp_id/origin blank, got rp_id=%q origin=%q", got.ans.RPID, got.ans.Origin)
	}
}

// TestWizardAutocertDerivesRPIDAndOrigin is the scenario the task spec calls
// out explicitly: selecting autocert then entering a domain produces the
// expected config to apply. It drives mode -> listen -> domain and asserts
// rp_id/origin were derived from the typed domain (deriveRPIDOrigin), then
// walks through to confirm and checks applyWebSetup's resulting
// *config.Config against a fake API's captured ApplyConfig call.
func TestWizardAutocertDerivesRPIDAndOrigin(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	var mm tea.Model = newModel(api)

	mm, _ = mm.Update(keyRunes('s'))         // enter wizard, cursor on "proxy"
	mm, _ = mm.Update(keyType(tea.KeyDown))  // move to "autocert"
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // select autocert

	got := mm.(model)
	if got.ans.Mode != "autocert" {
		t.Fatalf("ans.Mode = %q, want autocert", got.ans.Mode)
	}
	if got.wiz != webSetupListen {
		t.Fatalf("wiz = %v, want webSetupListen", got.wiz)
	}
	if got.listenIn.Value() != "0.0.0.0:8443" {
		t.Errorf("autocert default listen = %q, want 0.0.0.0:8443", got.listenIn.Value())
	}

	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept default listen
	got = mm.(model)
	if got.wiz != webSetupDomain {
		t.Fatalf("wiz = %v, want webSetupDomain", got.wiz)
	}

	mm = typeString(t, mm, "example.com")
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // commit domain

	got = mm.(model)
	if got.ans.Domain != "example.com" {
		t.Fatalf("ans.Domain = %q, want example.com", got.ans.Domain)
	}
	if got.wiz != webSetupRPID {
		t.Fatalf("wiz = %v, want webSetupRPID", got.wiz)
	}
	if got.rpidIn.Value() != "example.com" {
		t.Errorf("derived rp_id = %q, want example.com", got.rpidIn.Value())
	}
	if got.ans.Origin != "https://example.com" {
		t.Errorf("derived origin = %q, want https://example.com", got.ans.Origin)
	}

	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept derived rp_id
	got = mm.(model)
	if got.wiz != webSetupOrigin {
		t.Fatalf("wiz = %v, want webSetupOrigin", got.wiz)
	}
	if got.originIn.Value() != "https://example.com" {
		t.Errorf("origin input = %q, want https://example.com", got.originIn.Value())
	}

	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept derived origin
	got = mm.(model)
	if got.wiz != webSetupConfirm {
		t.Fatalf("wiz = %v, want webSetupConfirm", got.wiz)
	}

	// Confirm and apply.
	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	got = mm.(model)
	if !got.applying {
		t.Fatal("applying = false after confirming, want true")
	}
	if cmd == nil {
		t.Fatal("expected applyWebSetupCmd, got nil Cmd")
	}
	msg := runCmd(t, cmd)
	applied, ok := msg.(webSetupAppliedMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want webSetupAppliedMsg", msg)
	}
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}

	if api.applyN != 1 {
		t.Fatalf("ApplyConfig called %d times, want 1", api.applyN)
	}
	if api.applied == nil {
		t.Fatal("ApplyConfig was called with a nil config")
	}
	if !api.applied.Web.Enabled {
		t.Error("applied config: web.enabled = false, want true")
	}
	if api.applied.Web.Mode != "autocert" {
		t.Errorf("applied config: web.mode = %q, want autocert", api.applied.Web.Mode)
	}
	if api.applied.Web.Listen != "0.0.0.0:8443" {
		t.Errorf("applied config: web.listen = %q, want 0.0.0.0:8443", api.applied.Web.Listen)
	}
	if api.applied.Web.RPID != "example.com" {
		t.Errorf("applied config: web.rp_id = %q, want example.com", api.applied.Web.RPID)
	}
	if api.applied.Web.Origin != "https://example.com" {
		t.Errorf("applied config: web.origin = %q, want https://example.com", api.applied.Web.Origin)
	}
	if api.applied.Web.AutocertDomains != "example.com" {
		t.Errorf("applied config: web.autocert_domains = %q, want example.com", api.applied.Web.AutocertDomains)
	}

	// Feed the msg back through Update, as tea.Program's runtime loop would.
	mm, _ = mm.Update(applied)
	got = mm.(model)
	if got.wiz != webSetupResult {
		t.Fatalf("wiz after applied msg = %v, want webSetupResult", got.wiz)
	}
	if got.applying {
		t.Error("applying should be false once the result msg lands")
	}
	if got.applyErr != nil {
		t.Errorf("applyErr = %v, want nil", got.applyErr)
	}
}

// TestWizardConfirmCancelReturnsHome asserts 'n' on the confirm screen
// discards the wizard without ever calling ApplyConfig.
func TestWizardConfirmCancelReturnsHome(t *testing.T) {
	api := &fakeAPI{}
	var mm tea.Model = newModel(api)
	mm, _ = mm.Update(keyRunes('s'))
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // proxy
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // default listen -> confirm (proxy skips domain)

	if mm.(model).wiz != webSetupConfirm {
		t.Fatalf("precondition failed: wiz = %v, want webSetupConfirm", mm.(model).wiz)
	}

	mm, cmd := mm.Update(keyRunes('n'))
	if cmd != nil {
		t.Error("cancel should not issue a command")
	}
	if mm.(model).step != stepHome {
		t.Errorf("step = %v, want stepHome after cancel", mm.(model).step)
	}
	if api.applyN != 0 {
		t.Errorf("ApplyConfig called %d times, want 0 (cancelled)", api.applyN)
	}
}

// TestWizardApplyErrorSurfaces asserts a failing ApplyConfig (as the daemon
// would return on a rejected config) reaches the result screen's applyErr
// rather than being silently swallowed.
func TestWizardApplyErrorSurfaces(t *testing.T) {
	wantErr := errors.New("web.rp_id does not match web.origin")
	api := &fakeAPI{cfg: &config.Config{}, applyErr: wantErr}
	var mm tea.Model = newModel(api)
	mm, _ = mm.Update(keyRunes('s'))
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // proxy
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // default listen -> confirm

	mm, cmd := mm.Update(keyType(tea.KeyEnter)) // confirm apply
	msg := runCmd(t, cmd)
	mm, _ = mm.Update(msg)

	got := mm.(model)
	if got.applyErr == nil || got.applyErr.Error() != wantErr.Error() {
		t.Fatalf("applyErr = %v, want %v", got.applyErr, wantErr)
	}
	if got.wiz != webSetupResult {
		t.Fatalf("wiz = %v, want webSetupResult", got.wiz)
	}
}

// TestWizardEscBacksOutOfTextStep asserts esc on a text step (e.g. listen)
// returns to mode selection without losing the ability to pick a different
// mode, rather than exiting the whole wizard.
func TestWizardEscBacksOutOfTextStep(t *testing.T) {
	var mm tea.Model = newModel(&fakeAPI{})
	mm, _ = mm.Update(keyRunes('s'))
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // proxy -> listen step

	if mm.(model).wiz != webSetupListen {
		t.Fatalf("precondition: wiz = %v, want webSetupListen", mm.(model).wiz)
	}
	mm, _ = mm.Update(keyType(tea.KeyEsc))
	got := mm.(model)
	if got.wiz != webSetupMode {
		t.Errorf("wiz after esc = %v, want webSetupMode", got.wiz)
	}
	if got.step != stepSetupWeb {
		t.Errorf("esc on a text step should stay in the wizard, step = %v", got.step)
	}
}

// TestSnapshotMsgUpdatesHome asserts a snapshotMsg (as fetchSnapshotCmd
// produces) lands in the model's home fields.
func TestSnapshotMsgUpdatesHome(t *testing.T) {
	m := newModel(&fakeAPI{})
	got, _ := m.Update(snapshotMsg{view: core.DashboardView{CPU: 12.5}, err: nil})
	gm := got.(model)
	if gm.loading {
		t.Error("loading should be false after a snapshotMsg")
	}
	if gm.snap.CPU != 12.5 {
		t.Errorf("snap.CPU = %v, want 12.5", gm.snap.CPU)
	}
}

// TestWizardManualModeCollectsCertAndKey drives the full manual-mode path
// (mode -> listen -> domain -> rp_id -> origin -> cert -> key -> confirm)
// and asserts the two new steps appear in the right place and their typed
// values land on ans.TLSCert/TLSKey, then confirms and checks ApplyConfig
// received a config with web.tls_cert/web.tls_key set -- the actual bug
// this task closes (manual mode used to apply with no cert/key at all).
func TestWizardManualModeCollectsCertAndKey(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	var mm tea.Model = newModel(api)

	mm, _ = mm.Update(keyRunes('s'))        // enter wizard, cursor on "proxy"
	mm, _ = mm.Update(keyType(tea.KeyDown)) // -> autocert
	mm, _ = mm.Update(keyType(tea.KeyDown)) // -> manual
	mm, _ = mm.Update(keyType(tea.KeyEnter))
	if mm.(model).ans.Mode != "manual" {
		t.Fatalf("ans.Mode = %q, want manual", mm.(model).ans.Mode)
	}

	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept default listen -> domain
	mm = typeString(t, mm, "example.com")
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // commit domain -> rp_id
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept derived rp_id -> origin
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept derived origin

	got := mm.(model)
	if got.wiz != webSetupCert {
		t.Fatalf("wiz after origin = %v, want webSetupCert (manual mode)", got.wiz)
	}

	mm = typeString(t, mm, "/etc/serverwatch/tls/cert.pem")
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // commit cert -> key
	got = mm.(model)
	if got.wiz != webSetupKey {
		t.Fatalf("wiz after cert = %v, want webSetupKey", got.wiz)
	}
	if got.ans.TLSCert != "/etc/serverwatch/tls/cert.pem" {
		t.Errorf("ans.TLSCert = %q, want /etc/serverwatch/tls/cert.pem", got.ans.TLSCert)
	}

	mm = typeString(t, mm, "/etc/serverwatch/tls/key.pem")
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // commit key -> confirm
	got = mm.(model)
	if got.wiz != webSetupConfirm {
		t.Fatalf("wiz after key = %v, want webSetupConfirm", got.wiz)
	}
	if got.ans.TLSKey != "/etc/serverwatch/tls/key.pem" {
		t.Errorf("ans.TLSKey = %q, want /etc/serverwatch/tls/key.pem", got.ans.TLSKey)
	}
	if !strings.Contains(got.setupView(), "/etc/serverwatch/tls/cert.pem") {
		t.Error("confirm screen should show the cert path")
	}

	mm, cmd := mm.Update(keyType(tea.KeyEnter)) // confirm apply
	if cmd == nil {
		t.Fatal("expected applyWebSetupCmd, got nil")
	}
	msg := runCmd(t, cmd)
	applied, ok := msg.(webSetupAppliedMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want webSetupAppliedMsg", msg)
	}
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	if api.applied.Web.TLSCert != "/etc/serverwatch/tls/cert.pem" {
		t.Errorf("applied Web.TLSCert = %q, want /etc/serverwatch/tls/cert.pem", api.applied.Web.TLSCert)
	}
	if api.applied.Web.TLSKey != "/etc/serverwatch/tls/key.pem" {
		t.Errorf("applied Web.TLSKey = %q, want /etc/serverwatch/tls/key.pem", api.applied.Web.TLSKey)
	}
}

// TestWizardAutocertModeSkipsCertAndKey asserts autocert mode (which gets
// its certificate from Let's Encrypt) goes straight from origin to confirm,
// never visiting the manual-only cert/key steps.
func TestWizardAutocertModeSkipsCertAndKey(t *testing.T) {
	var mm tea.Model = newModel(&fakeAPI{cfg: &config.Config{}})
	mm, _ = mm.Update(keyRunes('s'))
	mm, _ = mm.Update(keyType(tea.KeyDown)) // -> autocert
	mm, _ = mm.Update(keyType(tea.KeyEnter))
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // default listen -> domain
	mm = typeString(t, mm, "example.com")
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // commit domain -> rp_id
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept rp_id -> origin
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept origin

	if got := mm.(model).wiz; got != webSetupConfirm {
		t.Fatalf("wiz = %v, want webSetupConfirm (autocert skips cert/key)", got)
	}
}

// TestWizardManualModeRejectsEmptyCert asserts pressing enter on the cert
// step with a blank value does not advance to the key step, and shows an
// inline message explaining why, so the wizard cannot be walked through to
// confirm/apply with a cert path that was never actually typed.
func TestWizardManualModeRejectsEmptyCert(t *testing.T) {
	var mm tea.Model = newModel(&fakeAPI{cfg: &config.Config{}})
	mm, _ = mm.Update(keyRunes('s'))
	mm, _ = mm.Update(keyType(tea.KeyDown)) // -> autocert
	mm, _ = mm.Update(keyType(tea.KeyDown)) // -> manual
	mm, _ = mm.Update(keyType(tea.KeyEnter))
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // default listen -> domain
	mm = typeString(t, mm, "example.com")
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // commit domain -> rp_id
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept rp_id -> origin
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept origin -> cert

	if got := mm.(model).wiz; got != webSetupCert {
		t.Fatalf("precondition: wiz = %v, want webSetupCert", got)
	}

	mm, cmd := mm.Update(keyType(tea.KeyEnter)) // enter with a blank cert path
	if cmd != nil {
		t.Error("rejecting a blank cert path should not issue a command")
	}
	got := mm.(model)
	if got.wiz != webSetupCert {
		t.Fatalf("wiz after blank cert = %v, want still webSetupCert (rejected)", got.wiz)
	}
	if got.wizFieldErr == nil {
		t.Fatal("wizFieldErr = nil, want a validation error explaining the blank cert path")
	}
	if !strings.Contains(got.setupView(), "required") {
		t.Errorf("cert step view should show an inline required-field message:\n%s", got.setupView())
	}

	// Typing a value and retrying must succeed.
	mm = typeString(t, mm, "/etc/serverwatch/tls/cert.pem")
	mm, _ = mm.Update(keyType(tea.KeyEnter))
	if got := mm.(model).wiz; got != webSetupKey {
		t.Fatalf("wiz after a valid retry = %v, want webSetupKey", got)
	}
}

// TestWizardManualModeRejectsEmptyKey mirrors
// TestWizardManualModeRejectsEmptyCert for the key step, and additionally
// asserts ApplyConfig is never reachable without ever confirming, so an
// incomplete manual-mode config can never be applied through the wizard.
func TestWizardManualModeRejectsEmptyKey(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	var mm tea.Model = newModel(api)
	mm, _ = mm.Update(keyRunes('s'))
	mm, _ = mm.Update(keyType(tea.KeyDown))
	mm, _ = mm.Update(keyType(tea.KeyDown)) // -> manual
	mm, _ = mm.Update(keyType(tea.KeyEnter))
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // default listen -> domain
	mm = typeString(t, mm, "example.com")
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // -> rp_id
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // -> origin
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // -> cert
	mm = typeString(t, mm, "/etc/serverwatch/tls/cert.pem")
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // commit cert -> key

	if got := mm.(model).wiz; got != webSetupKey {
		t.Fatalf("precondition: wiz = %v, want webSetupKey", got)
	}
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // enter with a blank key path
	got := mm.(model)
	if got.wiz != webSetupKey {
		t.Fatalf("wiz after blank key = %v, want still webSetupKey (rejected)", got.wiz)
	}
	if got.wizFieldErr == nil {
		t.Fatal("wizFieldErr = nil, want a validation error")
	}
	if api.applyN != 0 {
		t.Errorf("ApplyConfig called %d times, want 0 (never reached confirm)", api.applyN)
	}
}

// TestWizardCertStepEscReturnsToMode asserts esc on the cert step follows
// the same rule every other text step does (updateTextKey): back to mode
// selection, not a partial step back.
func TestWizardCertStepEscReturnsToMode(t *testing.T) {
	var mm tea.Model = newModel(&fakeAPI{cfg: &config.Config{}})
	mm, _ = mm.Update(keyRunes('s'))
	mm, _ = mm.Update(keyType(tea.KeyDown))
	mm, _ = mm.Update(keyType(tea.KeyDown)) // -> manual
	mm, _ = mm.Update(keyType(tea.KeyEnter))
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // -> domain
	mm = typeString(t, mm, "example.com")
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // -> rp_id
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // -> origin
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // -> cert

	mm, _ = mm.Update(keyType(tea.KeyEsc))
	if got := mm.(model).wiz; got != webSetupMode {
		t.Errorf("wiz after esc on cert step = %v, want webSetupMode", got)
	}
}
