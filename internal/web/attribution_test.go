package web

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// The anonymous pages (public status, history, login) carry a small visible
// attribution footer with followed links and a JSON-LD description of the
// project, so crawlers can attribute the page. Neither names the company.
func TestAnonymousPagesCarryAttribution(t *testing.T) {
	sp := publicFixture()
	d := publicDeps(t, sp, true)
	for _, path := range []string{"/", "/status/history", "/login"} {
		w, body := anonGet(t, d, path)
		if w.Code != 200 {
			t.Fatalf("%s: status %d", path, w.Code)
		}
		for _, href := range []string{attributionProductURL, attributionRepoURL, attributionSiteURL} {
			if !strings.Contains(body, `href="`+href+`"`) {
				t.Errorf("%s: missing footer link %s", path, href)
			}
		}
		if strings.Contains(body, `rel="nofollow`) {
			t.Errorf("%s: attribution links must be followed", path)
		}
		if strings.Contains(strings.ToLower(body), "infodive labs") {
			t.Errorf("%s: page must not name the company", path)
		}
		m := regexp.MustCompile(`(?s)<script type="application/ld\+json"[^>]*>(.*?)</script>`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no JSON-LD block", path)
		}
		var ld map[string]any
		if err := json.Unmarshal([]byte(m[1]), &ld); err != nil {
			t.Fatalf("%s: JSON-LD is not valid JSON: %v\n%s", path, err, m[1])
		}
		if ld["@type"] != "SoftwareApplication" || ld["url"] != attributionProductURL || ld["name"] != "trinetra" {
			t.Errorf("%s: unexpected JSON-LD %v", path, ld)
		}
		same, _ := ld["sameAs"].([]any)
		if len(same) != 2 || same[0] != attributionSiteURL || same[1] != attributionRepoURL {
			t.Errorf("%s: sameAs = %v", path, ld["sameAs"])
		}
	}
}

// The zero-services fallback public page gets the same attribution.
func TestFallbackPublicPageCarriesAttribution(t *testing.T) {
	d := publicDeps(t, &fakeStatusPage{pub: core.PublicStatus{Schema: 1}}, true)
	_, body := anonGet(t, d, "/")
	if !strings.Contains(body, `href="`+attributionProductURL+`"`) || !strings.Contains(body, "application/ld+json") {
		t.Fatal("fallback public page lacks attribution")
	}
}
