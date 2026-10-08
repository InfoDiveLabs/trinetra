package web

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

func publicFixture() *fakeStatusPage {
	return &fakeStatusPage{pub: core.PublicStatus{Schema: 1, Title: "Acme status", Overall: core.OverallPartial,
		Services: []core.PublicService{{Name: "API", Group: "Core", State: core.StateDegraded, History: []core.PublicDay{{Date: "2026-10-08", State: core.StateDegraded}}}},
		Active: []core.PublicIncident{{ID: "i1", Title: "Degraded performance: API", Impact: core.StateDegraded, Services: []string{"API"}, Status: "identified",
			Updates: []core.PublicUpdate{{TS: 1759900000, Status: "identified", Message: "Line one\nLine two"}}}},
	}}
}

func publicDeps(t *testing.T, sp *fakeStatusPage, enabled bool) Deps {
	t.Helper()
	d := statusPageDeps(t, sp)
	c := config.Default()
	c.Public.Enabled = enabled
	d.Cfg = func() *config.Config { return c }
	return d
}

func anonGet(t *testing.T, d Deps, path string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	w := httptest.NewRecorder()
	newHandler(d).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	b, _ := io.ReadAll(w.Result().Body)
	return w, string(b)
}

func TestPublicStatusPageRenders(t *testing.T) {
	w, body := anonGet(t, publicDeps(t, publicFixture(), true), "/")
	if w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	for _, want := range []string{"Acme status", "Partial outage", "API", "Degraded performance: API", "Line one<br>Line two"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("public HTML must be no-store")
	}
}

func TestPublicStatusFallsBackWithoutServices(t *testing.T) {
	sp := &fakeStatusPage{pub: core.PublicStatus{Schema: 1}}
	_, body := anonGet(t, publicDeps(t, sp, true), "/")
	if strings.Contains(body, "All systems operational") {
		t.Fatal("status banner shown with zero services; must keep the metrics-only page")
	}
}

func TestPublicStatusHistoryRenders(t *testing.T) {
	w, body := anonGet(t, publicDeps(t, publicFixture(), true), "/status/history")
	if w.Code != 200 || !strings.Contains(body, "Incident history") || !strings.Contains(body, "Degraded performance: API") {
		t.Fatalf("%d %s", w.Code, body)
	}
}

func TestPublicRoutes404WhenDisabled(t *testing.T) {
	d := publicDeps(t, publicFixture(), false)
	for _, p := range []string{"/status/history", "/status/feed.atom", "/status/api.json"} {
		if w, _ := anonGet(t, d, p); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

func TestPublicJSONAPI(t *testing.T) {
	w, body := anonGet(t, publicDeps(t, publicFixture(), true), "/status/api.json")
	if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(w.Header().Get("Cache-Control"), "max-age=30") {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	var got core.PublicStatus
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Schema != 1 || got.Services[0].Name != "API" {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestPublicAtomFeedIsValidXML(t *testing.T) {
	w, body := anonGet(t, publicDeps(t, publicFixture(), true), "/status/feed.atom")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/atom+xml") {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	var feed struct {
		XMLName xml.Name                     `xml:"feed"`
		Entries []struct{ Title, ID string } `xml:"entry"`
	}
	if err := xml.Unmarshal([]byte(body), &feed); err != nil || len(feed.Entries) != 1 {
		t.Fatalf("%v %+v", err, feed)
	}
}

func TestPublicEscapesHostileMessage(t *testing.T) {
	sp := publicFixture()
	sp.pub.Active[0].Updates[0].Message = "</script><script>alert(1)</script> ]]>   & <b>"
	sp.pub.Active[0].Title = "<img src=x onerror=alert(1)>"
	d := publicDeps(t, sp, true)
	_, html := anonGet(t, d, "/")
	if strings.Contains(html, "<script>alert(1)") || strings.Contains(html, "<img src=x") {
		t.Fatal("HTML not escaped")
	}
	_, atom := anonGet(t, d, "/status/feed.atom")
	var v struct {
		XMLName xml.Name `xml:"feed"`
	}
	if err := xml.Unmarshal([]byte(atom), &v); err != nil {
		t.Fatalf("hostile text broke the Atom XML: %v", err)
	}
	_, js := anonGet(t, d, "/status/api.json")
	if strings.Contains(js, "</script>") {
		t.Fatal("JSON not HTML-safe escaped")
	}
}
