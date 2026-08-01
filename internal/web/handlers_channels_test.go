//go:build web

package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"serverwatch/internal/config"
)

// undeliverableTelegram simulates buildNotifier's rule that a telegram channel
// with no resolvable chat id cannot deliver. It is what daemon_web.go wires
// Deps.ValidateChannel to in production (via the real buildNotifier).
func undeliverableTelegram(cc config.ChannelConfig, _ *config.Config) error {
	if cc.Type == "telegram" && cc.Settings["chat_id"] == "" {
		return fmt.Errorf("telegram channel %q: chat_id not configured", cc.Name)
	}
	return nil
}

// TestChannelsAddRejectsUndeliverableEnabledChannel is the #79 guard: the web
// editor must not silently create an enabled channel that will be dropped at
// delivery. A telegram channel with a token but no chat id (the trap the old
// "auto-captured on first message" placeholder invited) is rejected with 400,
// not persisted, and Reload is never called.
func TestChannelsAddRejectsUndeliverableEnabledChannel(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	d.ValidateChannel = undeliverableTelegram
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	form := url.Values{
		"name":           {"Phone"},
		"type":           {"telegram"},
		"enabled":        {"1"},
		"settings.token": {"123:abc"},
		// chat_id deliberately omitted
	}
	rr := postForm(h, "/channels", form, cookie, csrf)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (undeliverable channel rejected); body: %s", rr.Code, rr.Body.String())
	}
	if *reloadCalled {
		t.Fatal("Reload must not be called when the channel is rejected")
	}
	if _, ok := (*cfg).GetChannel("Phone"); ok {
		t.Fatal("an undeliverable channel must not be persisted")
	}
}

// TestChannelsAddAllowsDeliverableTelegramChannel: with a chat id supplied the
// same channel validates and is created.
func TestChannelsAddAllowsDeliverableTelegramChannel(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	d.ValidateChannel = undeliverableTelegram
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	form := url.Values{
		"name":             {"Phone"},
		"type":             {"telegram"},
		"enabled":          {"1"},
		"settings.token":   {"123:abc"},
		"settings.chat_id": {"555"},
	}
	rr := postForm(h, "/channels", form, cookie, csrf)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if !*reloadCalled {
		t.Fatal("Reload should be called for a deliverable channel")
	}
	cc, ok := (*cfg).GetChannel("Phone")
	if !ok || cc.Settings["chat_id"] != "555" {
		t.Fatalf("channel not persisted correctly: ok=%v cc=%+v", ok, cc)
	}
}

// TestChannelsAddDisabledChannelSkipsDeliverabilityCheck: a disabled channel
// is a draft and need not be deliverable yet, so it saves without validation.
func TestChannelsAddDisabledChannelSkipsDeliverabilityCheck(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	d.ValidateChannel = func(config.ChannelConfig, *config.Config) error {
		return fmt.Errorf("validation must not run for a disabled channel")
	}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	form := url.Values{
		"name":           {"Draft"},
		"type":           {"telegram"},
		"settings.token": {"123:abc"},
		// enabled omitted => disabled; chat_id omitted
	}
	rr := postForm(h, "/channels", form, cookie, csrf)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (disabled draft allowed); body: %s", rr.Code, rr.Body.String())
	}
	if _, ok := (*cfg).GetChannel("Draft"); !ok {
		t.Fatal("disabled draft channel should be saved")
	}
}

// TestChannelsUpdateRejectsUndeliverableEnabledChannel: the same guard applies
// when editing an existing channel into an undeliverable enabled state.
func TestChannelsUpdateRejectsUndeliverableEnabledChannel(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	d.ValidateChannel = undeliverableTelegram
	// seed an existing (disabled) telegram channel to edit
	c := *(*cfg)
	c.AddChannel(config.ChannelConfig{Name: "Phone", Type: "telegram", Settings: map[string]string{"token": "123:abc"}})
	*cfg = &c
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	form := url.Values{
		"name":           {"Phone"},
		"type":           {"telegram"},
		"enabled":        {"1"},
		"settings.token": {"123:abc"},
		// still no chat id
	}
	rr := postForm(h, "/channels/Phone/update", form, cookie, csrf)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on enabling an undeliverable channel; body: %s", rr.Code, rr.Body.String())
	}
}

// TestChannelsAddRoundTripsToConfig pins the core CRUD obligation: POSTing
// /channels creates a channel that shows up in cfg.Channels with the posted
// fields, is Reload()ed, and is audited.
func TestChannelsAddRoundTripsToConfig(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	form := url.Values{
		"name":          {"Ops email"},
		"type":          {"email"},
		"enabled":       {"1"},
		"min_severity":  {"critical"},
		"settings.host": {"smtp.example.com"},
		"settings.from": {"sw@example.com"},
		"settings.to":   {"ops@example.com"},
	}
	rr := postForm(h, "/channels", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !*reloadCalled {
		t.Fatal("Reload was not called")
	}
	cc, ok := (*cfg).GetChannel("Ops email")
	if !ok {
		t.Fatal("channel was not added")
	}
	if cc.Type != "email" || !cc.Enabled || cc.MinSeverity != "critical" {
		t.Errorf("channel = %+v, want type=email enabled=true min_severity=critical", cc)
	}
	if cc.Settings["host"] != "smtp.example.com" || cc.Settings["from"] != "sw@example.com" || cc.Settings["to"] != "ops@example.com" {
		t.Errorf("settings = %+v, want host/from/to populated", cc.Settings)
	}

	recs := readAuditRecords(t, d.StateDir)
	found := false
	for _, r := range recs {
		if r.Action == "channel.add" && r.Key == "Ops email" {
			found = true
		}
	}
	if !found {
		t.Errorf("no channel.add audit record, got: %+v", recs)
	}
}

// TestChannelsAddDuplicateNameRejected pins that adding a channel whose name
// already exists is rejected rather than silently duplicating/overwriting.
func TestChannelsAddDuplicateNameRejected(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).AddChannel(config.ChannelConfig{Name: "dup", Type: "webhook"})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/channels", url.Values{"name": {"dup"}, "type": {"webhook"}, "settings.url": {"https://x"}}, cookie, csrf)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body: %s", rr.Code, rr.Body.String())
	}
	if len((*cfg).Channels) != 1 {
		t.Errorf("Channels = %+v, want unchanged (still 1)", (*cfg).Channels)
	}
}

// TestChannelsAddBadMinSeverityRejectedWithNoWrite pins the validation
// contract: an invalid min_severity rejects 400 and writes nothing (reusing
// config.validateMinSeverity via SetChannelField).
func TestChannelsAddBadMinSeverityRejectedWithNoWrite(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/channels", url.Values{"name": {"bad"}, "type": {"webhook"}, "min_severity": {"not-a-severity"}, "settings.url": {"https://x"}}, cookie, csrf)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if *reloadCalled {
		t.Error("Reload was called despite invalid min_severity")
	}
	if _, ok := (*cfg).GetChannel("bad"); ok {
		t.Error("channel should not have been written")
	}
}

// TestChannelsUpdateRoundTripsToConfig pins editing an existing channel:
// the posted fields replace the old ones, and an audit record captures the
// old->new summary.
func TestChannelsUpdateRoundTripsToConfig(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram", Enabled: false, MinSeverity: "info"})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	param := channelNameParam("tg")
	form := url.Values{
		"type":             {"telegram"},
		"enabled":          {"1"},
		"min_severity":     {"warning"},
		"settings.token":   {"tok"},
		"settings.chat_id": {"chat"},
	}
	rr := postForm(h, "/channels/"+param+"/update", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	cc, ok := (*cfg).GetChannel("tg")
	if !ok {
		t.Fatal("channel disappeared")
	}
	if !cc.Enabled || cc.MinSeverity != "warning" || cc.Settings["token"] != "tok" {
		t.Errorf("channel = %+v, want enabled=true min_severity=warning settings.token=tok", cc)
	}

	recs := readAuditRecords(t, d.StateDir)
	found := false
	for _, r := range recs {
		if r.Action == "channel.update" && r.Key == "tg" {
			found = true
			if !strings.Contains(r.Old, "enabled=false") || !strings.Contains(r.New, "enabled=true") {
				t.Errorf("audit old/new = %q -> %q, want enabled=false -> enabled=true", r.Old, r.New)
			}
		}
	}
	if !found {
		t.Errorf("no channel.update audit record, got: %+v", recs)
	}
}

// TestChannelsUpdateUnknownChannelNotFound pins that updating a
// never-existed channel 404s rather than silently creating one.
func TestChannelsUpdateUnknownChannelNotFound(t *testing.T) {
	d, _, _ := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/channels/"+channelNameParam("ghost")+"/update", url.Values{"type": {"webhook"}}, cookie, csrf)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
}

// TestChannelsRemoveRoundTrips pins removal: the channel disappears from
// cfg.Channels and an audit record is written.
func TestChannelsRemoveRoundTrips(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).AddChannel(config.ChannelConfig{Name: "gone", Type: "webhook"})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/channels/"+channelNameParam("gone")+"/remove", url.Values{}, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if _, ok := (*cfg).GetChannel("gone"); ok {
		t.Error("channel should have been removed")
	}
	recs := readAuditRecords(t, d.StateDir)
	found := false
	for _, r := range recs {
		if r.Action == "channel.remove" && r.Key == "gone" {
			found = true
		}
	}
	if !found {
		t.Errorf("no channel.remove audit record, got: %+v", recs)
	}
}

// TestChannelsTestHandlerCallsTestChannel pins the "send test" wiring: it
// calls Deps.API.TestChannel (task 8; previously Deps.TestChannel directly)
// with the decoded name and renders its result.
func TestChannelsTestHandlerCallsTestChannel(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram"})
	var gotName string
	d.API = fakeAPI{testChannel: func(name string) error {
		gotName = name
		return nil
	}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/channels/"+channelNameParam("tg")+"/test", url.Values{}, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if gotName != "tg" {
		t.Errorf("TestChannel called with %q, want tg", gotName)
	}
	if !strings.Contains(rr.Body.String(), "sent test notification") {
		t.Errorf("body missing success message:\n%s", rr.Body.String())
	}
}

// TestChannelsTestHandlerNilTestChannelDoesNotPanic pins the "not yet
// wired" fallback: a nil Deps.API renders a clear message instead of
// panicking (task 8: the guard moved from checking Deps.TestChannel to
// checking Deps.API, since that's what channelsTestHandler now calls
// through).
func TestChannelsTestHandlerNilTestChannelDoesNotPanic(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram"})
	d.API = nil
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/channels/"+channelNameParam("tg")+"/test", url.Values{}, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "not wired") {
		t.Errorf("body missing not-wired message:\n%s", rr.Body.String())
	}
}

// TestChannelsTestHandlerSurfacesError pins that a failing test-send renders
// the error rather than a generic success message.
func TestChannelsTestHandlerSurfacesError(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram"})
	d.API = fakeAPI{testChannel: func(name string) error { return errBoom }}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/channels/"+channelNameParam("tg")+"/test", url.Values{}, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "test failed") {
		t.Errorf("body missing failure message:\n%s", rr.Body.String())
	}
}

// TestChannelsPageRendersTableAndModals pins GET /channels: it lists
// existing channels and renders an edit modal per channel plus the add
// modal.
func TestChannelsPageRendersTableAndModals(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram", Enabled: true, MinSeverity: "info"})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/channels")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`class="side"`,
		`<b>tg</b>`,
		`id="chanModal-new"`,
		`id="chanModal-` + channelNameParam("tg") + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("channels page missing %q:\n%s", want, body)
		}
	}
}

// TestChannelsRoutesAreAdminGated pins RBAC on /channels, mirroring
// TestConfigRoutesAreAdminGated.
func TestChannelsRoutesAreAdminGated(t *testing.T) {
	d, _, _ := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	viewerReq := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/channels")
	viewerRR := httptest.NewRecorder()
	h.ServeHTTP(viewerRR, viewerReq)
	if viewerRR.Code != http.StatusForbidden {
		t.Errorf("GET /channels as viewer status = %d, want 403", viewerRR.Code)
	}

	anonRR := httptest.NewRecorder()
	h.ServeHTTP(anonRR, httptest.NewRequest(http.MethodGet, "/channels", nil))
	if anonRR.Code != http.StatusFound {
		t.Errorf("GET /channels anon status = %d, want 302", anonRR.Code)
	}
}

// TestChannelsMutationsRequireCSRF pins that every /channels* mutation
// rejects without a valid CSRF token.
func TestChannelsMutationsRequireCSRF(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram"})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)

	for _, target := range []string{
		"/channels",
		"/channels/" + channelNameParam("tg") + "/update",
		"/channels/" + channelNameParam("tg") + "/remove",
		"/channels/" + channelNameParam("tg") + "/test",
	} {
		rr := postForm(h, target, url.Values{"name": {"x"}, "type": {"webhook"}}, cookie, "" /* no CSRF */)
		if rr.Code != http.StatusForbidden {
			t.Errorf("POST %s without CSRF status = %d, want 403", target, rr.Code)
		}
	}
}

// errBoom is a fixed sentinel error for tests that just need "some error".
var errBoom = errTest("boom")

type errTest string

func (e errTest) Error() string { return string(e) }
