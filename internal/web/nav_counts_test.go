//go:build web

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"serverwatch/internal/config"
)

// TestNavBadgesRenderRealCounts pins Part 2's core obligation: the sidebar
// nav's Alerts/Channels/Users/Monitoring badges must reflect real per-request
// counts (Deps.AlertStatePath's active map, len(Cfg().Channels), the user
// store's List(), and the live snapshot's container count) rather than the
// mockup's hardcoded demo values (220/2/5/3).
func TestNavBadgesRenderRealCounts(t *testing.T) {
	d := enrollTestDeps(t)
	d.AlertStatePath = writeAlertState(t, d.StateDir, `{"active":{"disk:/":{"since":1,"reason":"x"},"cpu":{"since":2,"reason":"y"}}}`)
	d.Cfg = func() *config.Config {
		cfg := config.Default()
		cfg.Channels = []config.ChannelConfig{
			{Name: "tg", Type: "telegram", Enabled: true},
			{Name: "mail", Type: "email", Enabled: true},
			{Name: "hook", Type: "webhook", Enabled: true},
		}
		return cfg
	}
	d.Snapshot = func() DashboardView { return DashboardView{ContainersTotal: 9} }

	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	viewer := &User{ID: mustNewUserID(t), Name: "viewer-user", Role: RoleViewer, Created: 1}
	if err := users.Put(viewer); err != nil {
		t.Fatalf("seed viewer: %v", err)
	}

	h := newHandler(d)
	// seedSignedInRequest itself seeds one more (admin) user for the
	// session, so the store ends up with exactly 2: viewer above + this one.
	req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	for _, want := range []string{
		`Alerts<span class="ct">2</span>`,
		`Channels<span class="ct">3</span>`,
		`Users<span class="ct">2</span>`,
		`Monitoring<span class="ct">9</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("nav body missing real badge %q:\n%s", want, body)
		}
	}

	if strings.Contains(body, `Monitoring<span class="ct">220</span>`) {
		t.Errorf("nav still renders the mockup's hardcoded Monitoring 220 demo badge:\n%s", body)
	}
}

// TestNavBadgeHiddenWhenZero pins the "no stale numbers" rule: a 0 (or
// unknown/missing) count must render NO badge at all, not a literal "0".
func TestNavBadgeHiddenWhenZero(t *testing.T) {
	d := enrollTestDeps(t)
	// No AlertStatePath set (zero value "") -> 0 active alerts.
	// Default config -> zero channels.
	// No users seeded -> zero users.
	// Zero-value Snapshot -> zero containers.
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	// seedSignedInRequest seeds exactly one (admin) user for the session —
	// nothing else is added, so the store ends up with exactly 1.
	req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	for _, label := range []string{"Alerts", "Channels", "Monitoring"} {
		if strings.Contains(body, label+`<span class="ct">`) {
			t.Errorf("nav still renders a badge next to %q with a zero count:\n%s", label, body)
		}
	}
	// Users has exactly one seeded account (the signed-in admin) — its
	// badge must show "1", not be hidden and not show "0".
	if !strings.Contains(body, `Users<span class="ct">1</span>`) {
		t.Errorf("nav Users badge should show 1 for the single seeded user:\n%s", body)
	}
}

// TestNavCountsAlertStateMissingFileIsZeroNoPanic pins the defensive-decode
// requirement: an AlertStatePath naming a file that doesn't exist (or holds
// garbage) must count as 0 active alerts, never panic the page.
func TestNavCountsAlertStateMissingFileIsZeroNoPanic(t *testing.T) {
	d := enrollTestDeps(t)
	d.AlertStatePath = d.StateDir + "/does-not-exist.json"

	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200 (no panic), body: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `Alerts<span class="ct">`) {
		t.Errorf("nav should show no Alerts badge when AlertStatePath is missing:\n%s", rr.Body.String())
	}
}
