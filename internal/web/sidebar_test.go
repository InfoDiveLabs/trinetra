package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSidebarShowsSignedInUserNotHardcodedName is the #80 guard: the sidebar
// footer must render the actual signed-in user's name (here "admin-user" from
// seedSignedInRequest), never the hardcoded "Suraj"/"Aditi" placeholders.
func TestSidebarShowsSignedInUserNotHardcodedName(t *testing.T) {
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
	if !strings.Contains(body, "admin-user") {
		t.Errorf("sidebar should show the signed-in user's name 'admin-user':\n%s", body)
	}
	for _, placeholder := range []string{"Suraj", "Aditi"} {
		if strings.Contains(body, placeholder) {
			t.Errorf("sidebar must not render the hardcoded placeholder %q", placeholder)
		}
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
