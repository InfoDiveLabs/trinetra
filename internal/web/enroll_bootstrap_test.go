package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestLocalOnly(t *testing.T) {
	for _, tc := range []struct {
		origin, listen string
		want           bool
	}{
		{"", "127.0.0.1:8088", true},
		{"", "localhost:8088", true},
		{"", "[::1]:8088", true},
		{"", "0.0.0.0:8088", false},
		{"", ":8088", false},
		{"http://localhost:8088", "0.0.0.0:8088", false},
		{"http://localhost:8088", "127.0.0.1:8088", true},
		{"http://localhost:8088", ":8088", false},
		{"https://status.example.com", "127.0.0.1:8088", false},
		{"https://192.168.1.10", "127.0.0.1:8088", false},
		{"http://127.0.0.5:9000", "127.0.0.1:8088", true},
		{"http://localhost:8088", "[::]:8088", false},
	} {
		c := config.Default()
		c.Web.Origin, c.Web.Listen = tc.origin, tc.listen
		if got := localOnly(c); got != tc.want {
			t.Errorf("origin %q listen %q: %v, want %v", tc.origin, tc.listen, got, tc.want)
		}
	}
}

func TestBootstrapRefusedWhenNotLocal(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := resolveEnrollRole(newTokenStore(dir), newUserStore(dir), "", false); !errors.Is(err, errSetupLinkRequired) {
		t.Fatalf("err = %v, want errSetupLinkRequired", err)
	}
	if _, boot, err := resolveEnrollRole(newTokenStore(dir), newUserStore(dir), "", true); err != nil || !boot {
		t.Fatalf("local: boot=%v err=%v", boot, err)
	}
	tok := newTokenStore(dir).Issue(RoleAdmin, time.Hour)
	if role, boot, err := resolveEnrollRole(newTokenStore(dir), newUserStore(dir), tok, false); err != nil || boot || role != RoleAdmin {
		t.Fatalf("invite on non-local UI: role=%s boot=%v err=%v", role, boot, err)
	}
}

func TestEnrollPageExplainsSetupLink(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Web.Origin = "https://status.example.com"
	rr := httptest.NewRecorder()
	newHandler(d).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/enroll", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "sudo trinetra web users invite --role admin") {
		t.Fatalf("enroll page: %d %s", rr.Code, rr.Body.String())
	}
	(*cfg).Web.Origin = ""
	(*cfg).Web.Listen = "127.0.0.1:8088"
	rr = httptest.NewRecorder()
	newHandler(d).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/enroll", nil))
	if strings.Contains(rr.Body.String(), "sudo trinetra web users invite") {
		t.Fatal("local-only UI should allow first sign-up without a link")
	}
}
