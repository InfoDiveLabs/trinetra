package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// TestSidebarShowsSignedInUserNotHardcodedName is the #80 guard: the sidebar footer must
// render the actual signed-in user's name (here "admin-user" from seedSignedInRequest).
func TestSidebarShowsSignedInUserNotHardcodedName(t *testing.T) {
	d, _, _ := configTestDeps(t)
	// Pin a fixed server.name so the brand subtitle (#101) is deterministic and doesn't render
	// the dev machine's hostname.
	cfg := config.Default()
	cfg.Name = "test-host"
	d.Cfg = func() *config.Config { return cfg }
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/config")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "admin-user") {
		t.Errorf("sidebar should show the signed-in user's name 'admin-user':\n%s", body)
	}
	for _, placeholder := range []string{"Suraj", "Aditi"} {
		if strings.Contains(body, placeholder) {
			t.Errorf("sidebar must not render the hardcoded placeholder %q", placeholder)
		}
	}
}

// TestSidebarNavLinksCarryTitleAndLabelSpan pins U10b (2026-09-25 UI audit): the icon-rail
// breakpoint.
func TestSidebarNavLinksCarryTitleAndLabelSpan(t *testing.T) {
	d, _, _ := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/config")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `title="Dashboard" aria-label="Dashboard"`) {
		t.Errorf("sidebar Dashboard link missing title/aria-label:\n%s", body)
	}
	if !strings.Contains(body, `<span class="lb">Dashboard</span>`) {
		t.Errorf("sidebar Dashboard link missing its .lb label span:\n%s", body)
	}
}

func TestFirstInitial(t *testing.T) {
	cases := map[string]string{"admin-user": "A", "bob": "B", "": ""}
	for in, want := range cases {
		if got := firstInitial(in); got != want {
			t.Errorf("firstInitial(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSidebarMinRoleResponder(t *testing.T) {
	saved := navItems
	defer func() { navItems = saved }()
	navItems = []navEntry{
		{NavItem: NavItem{Href: "/resp", Label: "RespOnly"}, MinRole: RoleResponder},
		{NavItem: NavItem{Href: "/adm", Label: "AdmOnly"}, AdminOnly: true},
	}
	has := func(role, label string) bool {
		for _, n := range navForRole(role, NavCounts{}, nodeScope{Self: true}, "solo") {
			if n.Label == label {
				return true
			}
		}
		return false
	}
	if !has("responder", "RespOnly") || has("responder", "AdmOnly") {
		t.Error("responder should see MinRole item but not AdminOnly")
	}
	if has("viewer", "RespOnly") || has("viewer", "AdmOnly") {
		t.Error("viewer should see neither")
	}
	if !has("admin", "RespOnly") || !has("admin", "AdmOnly") {
		t.Error("admin should see both")
	}
}
