package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func statusNamed(id string) plugin.Status {
	st := demoStatus()
	st.ID, st.Manifest.ID, st.Manifest.Name = id, id, "Plugin "+id
	return st
}

func offer(id string, slot plugin.Slot, slotID string, role plugin.Role) plugin.SlotOffer {
	return plugin.SlotOffer{PluginID: id, PluginName: "Plugin " + id, Slot: slot, SlotID: slotID, Title: "Title " + slotID, MinRole: role}
}

func fragment(t *testing.T, e *pluginEnv, role Role, target string) (int, string) {
	t.Helper()
	rr := e.get(role, target)
	return rr.Code, rr.Body.String()
}

func TestBrokenSlotDoesNotBreakPage(t *testing.T) {
	m := newFakeManager(statusNamed("perr"), statusNamed("ptime"), statusNamed("pbad"), statusNamed("pgood"))
	m.offers = []plugin.SlotOffer{
		offer("perr", plugin.SlotDashboardPanel, "p", plugin.RoleViewer),
		offer("ptime", plugin.SlotDashboardPanel, "p", plugin.RoleViewer),
		offer("pbad", plugin.SlotDashboardPanel, "p", plugin.RoleViewer),
		offer("pgood", plugin.SlotDashboardPanel, "p", plugin.RoleViewer),
		offer("pgood", plugin.SlotNavPage, "x", plugin.RoleViewer),
	}
	m.delay["ptime/p"] = 3 * time.Second
	m.renderFn = func(r plugin.UIRequest) (plugin.RenderResult, error) {
		switch r.PluginID {
		case "perr":
			return plugin.RenderResult{}, errors.New("kaboom")
		case "pbad":
			return plugin.RenderResult{Widgets: []plugin.Widget{{Kind: plugin.WidgetStat}}}, nil
		}
		return plugin.RenderResult{Widgets: []plugin.Widget{statW("GOODVALUE")}}, nil
	}
	e := newPluginEnv(t, m)
	cookie := seedSignedInRequest(t, e.users, e.sessions, RoleViewer, http.MethodGet, "/").Cookies()[0]
	do := func(target string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		e.h.ServeHTTP(rr, req)
		return rr.Code, rr.Body.String()
	}

	rr := e.postRaw(cookie, "/dashboard/panels", map[string][]string{
		"csrf_token": {csrfFor(t, e, cookie)},
		"panel":      {"perr/p", "ptime/p", "pbad/p", "pgood/p"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save panels: %d %s", rr.Code, rr.Body.String())
	}

	code, page := do("/")
	if code != http.StatusOK || strings.Count(page, `hx-trigger="load"`) < 4 {
		t.Fatalf("dashboard shell: %d, placeholders=%d", code, strings.Count(page, `hx-trigger="load"`))
	}
	if len(m.renders) != 0 {
		t.Fatalf("page shell called a plugin synchronously: %d renders", len(m.renders))
	}
	code, page = do("/p/pgood/x")
	if code != http.StatusOK || !strings.Contains(page, `hx-trigger="load"`) || len(m.renders) != 0 {
		t.Fatalf("plugin page shell: %d renders=%d", code, len(m.renders))
	}

	want := map[string]string{"perr": "failed", "ptime": "timed out", "pbad": "returned an invalid view"}
	start := time.Now()
	for id, class := range want {
		code, body := do("/plugins/slot?slot=dashboard.panel&plugin=" + id + "&slot_id=p")
		if code != http.StatusOK || !strings.Contains(body, "plugin-error") || !strings.Contains(body, class) {
			t.Errorf("%s: %d %s", id, code, body)
		}
		if strings.Contains(body, "kaboom") {
			t.Errorf("%s leaked error text", id)
		}
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("fragments took %v", time.Since(start))
	}
	code, body := do("/plugins/slot?slot=dashboard.panel&plugin=pgood&slot_id=p")
	if code != http.StatusOK || !strings.Contains(body, "GOODVALUE") || strings.Contains(body, "plugin-error") {
		t.Errorf("good panel: %d %s", code, body)
	}
}

func csrfFor(t *testing.T, e *pluginEnv, c *http.Cookie) string {
	t.Helper()
	s, ok := e.sessions.Get(c.Value)
	if !ok {
		t.Fatal("no session")
	}
	return s.CSRF
}

func TestNavPageRoleFiltered(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.offers = []plugin.SlotOffer{
		offer("demo", plugin.SlotNavPage, "main", plugin.RoleViewer),
		offer("demo", plugin.SlotNavPage, "secret", plugin.RoleAdmin),
	}
	e := newPluginEnv(t, m)
	_, body := fragment(t, e, RoleViewer, "/monitoring")
	if !strings.Contains(body, `href="/p/demo/main"`) || strings.Contains(body, "/p/demo/secret") || !strings.Contains(body, "Plugins") {
		t.Errorf("viewer nav wrong")
	}
	_, body = fragment(t, e, RoleAdmin, "/monitoring")
	if !strings.Contains(body, `href="/p/demo/secret"`) {
		t.Errorf("admin nav missing admin page")
	}
	if code, _ := fragment(t, e, RoleViewer, "/p/demo/secret"); code != http.StatusNotFound {
		t.Errorf("viewer on admin page = %d", code)
	}
	if code, body := fragment(t, e, RoleViewer, "/p/demo/main"); code != http.StatusOK || !strings.Contains(body, "Title main") {
		t.Errorf("page = %d", code)
	}
	if code, _ := fragment(t, e, RoleViewer, "/p/demo/nope"); code != http.StatusNotFound {
		t.Errorf("unknown page = %d", code)
	}
}

func TestDashboardPanelPickerPerUser(t *testing.T) {
	m := newFakeManager(statusNamed("a"), statusNamed("b"))
	m.offers = []plugin.SlotOffer{
		offer("a", plugin.SlotDashboardPanel, "one", plugin.RoleViewer),
		offer("b", plugin.SlotDashboardPanel, "two", plugin.RoleViewer),
	}
	e := newPluginEnv(t, m)
	body := e.get(RoleViewer, "/dashboard/panels").Body.String()
	if !strings.Contains(body, `value="a/one"`) || !strings.Contains(body, `value="b/two"`) {
		t.Fatalf("picker missing offers:\n%s", body)
	}
	rr := e.post(RoleViewer, "/dashboard/panels", map[string][]string{"panel": {"a/one", "evil/x"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save = %d", rr.Code)
	}
	_, viewerDash := fragment(t, e, RoleViewer, "/")
	if !strings.Contains(viewerDash, "plugin=a") || strings.Contains(viewerDash, "plugin=b") || strings.Contains(viewerDash, "evil") {
		t.Errorf("viewer dashboard panels wrong")
	}
	_, adminDash := fragment(t, e, RoleAdmin, "/")
	if strings.Contains(adminDash, "plugin=a&amp;slot_id=one") || strings.Contains(adminDash, "slot_id=one") {
		t.Errorf("admin sees the viewer's panels")
	}
	p := filepath.Join(e.d.StateDir, "plugin_panels.json")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("store file: %v %v", err, st)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "viewer-user") || !strings.Contains(string(raw), "a/one") {
		t.Errorf("store content: %s", raw)
	}
	if rr := e.get(RoleViewer, "/dashboard/panels"); !strings.Contains(rr.Body.String(), "checked") {
		t.Error("saved choice not checked")
	}
}

func TestActionRequiresConfirmForMutating(t *testing.T) {
	m := newFakeManager(demoStatus())
	e := newPluginEnv(t, m)
	base := map[string][]string{"plugin": {"demo"}, "action": {"restart"}}
	rr := e.post(RoleAdmin, "/plugins/action", base)
	if rr.Code != http.StatusBadRequest || len(m.actions) != 0 {
		t.Fatalf("unconfirmed mutating: %d actions=%d", rr.Code, len(m.actions))
	}
	base["confirmed"] = []string{"1"}
	base["param.svc"] = []string{"nginx"}
	m.actionFn = func(plugin.UIActionRequest) (plugin.ActionResult, error) {
		return plugin.ActionResult{OK: true, Message: "restarted <b>x</b>", Widgets: []plugin.Widget{statW("RESULTVAL")}}, nil
	}
	rr = e.post(RoleAdmin, "/plugins/action", base)
	if rr.Code != http.StatusOK || len(m.actions) != 1 {
		t.Fatalf("confirmed: %d actions=%d", rr.Code, len(m.actions))
	}
	got := m.actions[0].Request
	if !got.Confirmed || got.Action != "restart" || got.Params["svc"] != "nginx" || got.User.Role != plugin.RoleAdmin {
		t.Errorf("request: %+v", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "restarted &lt;b&gt;x&lt;/b&gt;") || !strings.Contains(body, "RESULTVAL") {
		t.Errorf("result page: %s", body)
	}
	if rr := e.post(RoleResponder, "/plugins/action", base); rr.Code != http.StatusForbidden {
		t.Errorf("responder on admin action = %d", rr.Code)
	}
	if rr := e.post(RoleViewer, "/plugins/action", map[string][]string{"plugin": {"demo"}, "action": {"ping"}}); rr.Code != http.StatusOK {
		t.Errorf("viewer ping = %d", rr.Code)
	}
	if rr := e.post(RoleAdmin, "/plugins/action", map[string][]string{"plugin": {"demo"}, "action": {"nope"}}); rr.Code != http.StatusNotFound {
		t.Errorf("undeclared action = %d", rr.Code)
	}
}

func TestActionAudited(t *testing.T) {
	m := newFakeManager(demoStatus())
	e := newPluginEnv(t, m)
	e.post(RoleAdmin, "/plugins/action", map[string][]string{"plugin": {"demo"}, "action": {"ping"}})
	if strings.Contains(readAuditActions(t, e.d.StateDir), "plugin.action") {
		t.Error("non-mutating action audited")
	}
	e.post(RoleAdmin, "/plugins/action", map[string][]string{"plugin": {"demo"}, "action": {"restart"}, "confirmed": {"1"}, "container": {"shop-web"}})
	if a := readAuditActions(t, e.d.StateDir); !strings.Contains(a, "plugin.action") || !strings.Contains(a, "demo") || !strings.Contains(a, "container shop-web") {
		t.Errorf("audit: %s", a)
	}
}

func TestActionErrorShowsGenericMessage(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.actionFn = func(plugin.UIActionRequest) (plugin.ActionResult, error) {
		return plugin.ActionResult{}, errors.New("LEAKY-INTERNAL")
	}
	e := newPluginEnv(t, m)
	rr := e.post(RoleViewer, "/plugins/action", map[string][]string{"plugin": {"demo"}, "action": {"ping"}})
	if strings.Contains(rr.Body.String(), "LEAKY-INTERNAL") {
		t.Error("plugin error text leaked")
	}
}

func TestFileDownloadHeaders(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.files["demo/report.csv"] = []byte("a,b\n1,2\n")
	e := newPluginEnv(t, m)
	rr := e.get(RoleViewer, "/plugins/demo/files/report.csv")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	h := rr.Header()
	if h.Get("Content-Disposition") != `attachment; filename="report.csv"` || h.Get("Content-Type") != "text/csv" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", h)
	}
	if rr.Body.String() != "a,b\n1,2\n" {
		t.Errorf("body %q", rr.Body.String())
	}
	if rr := e.get(RoleViewer, "/plugins/demo/files/missing.csv"); rr.Code != http.StatusNotFound {
		t.Errorf("missing = %d", rr.Code)
	}
	if rr := e.get(RoleViewer, "/plugins/demo/files/..%2Fx"); rr.Code == http.StatusOK {
		t.Errorf("traversal = %d", rr.Code)
	}
}

func TestCSPHeaderUnchanged(t *testing.T) {
	e := newPluginEnv(t, newFakeManager(demoStatus()))
	rr := e.get(RoleViewer, "/integrations")
	csp := rr.Header().Get("Content-Security-Policy")
	m := regexp.MustCompile(`'nonce-[A-Za-z0-9_-]+'`).ReplaceAllString(csp, "'nonce-N'")
	want := "default-src 'self'; script-src 'self' 'nonce-N'; style-src 'self' 'unsafe-inline'; img-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'"
	if m != want {
		t.Errorf("CSP changed:\n got %s\nwant %s", m, want)
	}
	rr = e.get(RoleViewer, "/plugins/slot?slot=dashboard.panel")
	if rr.Header().Get("Content-Security-Policy") == "" {
		t.Error("fragment lacks CSP")
	}
}

func TestContainerDrawerUsesQueryEncoding(t *testing.T) {
	b, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i, j := strings.Index(s, "// ---- plugins: slot fragments"), strings.Index(s, "// ---- end plugins ----")
	if i < 0 || j < i {
		t.Fatal("plugin block markers missing")
	}
	block := s[i:j]
	if strings.Count(block, "encodeURIComponent") < 3 {
		t.Error("plugin block must encode container and slot names with encodeURIComponent")
	}
	if strings.Contains(block, "innerHTML") {
		t.Error("plugin block must not assign innerHTML")
	}
}

func TestSlotEndpointListsPlaceholdersForContainerSlots(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.offers = []plugin.SlotOffer{offer("demo", plugin.SlotContainerSection, "cs", plugin.RoleViewer)}
	e := newPluginEnv(t, m)
	code, body := fragment(t, e, RoleViewer, "/plugins/slot?slot=container.section&container="+url.QueryEscape("my app&x"))
	if code != http.StatusOK || !strings.Contains(body, "plugin=demo") || !strings.Contains(body, "container=my+app%26x") {
		t.Errorf("placeholders: %d %s", code, body)
	}
	if len(m.renders) != 0 {
		t.Error("placeholder listing called a plugin")
	}
	m.renderFn = func(r plugin.UIRequest) (plugin.RenderResult, error) {
		if r.Request.Context.Container != "my app&x" {
			t.Errorf("container ctx %q", r.Request.Context.Container)
		}
		return plugin.RenderResult{Widgets: []plugin.Widget{statW("1")}}, nil
	}
	fragment(t, e, RoleViewer, "/plugins/slot?slot=container.section&plugin=demo&slot_id=cs&container="+url.QueryEscape("my app&x"))
	if code, _ := fragment(t, e, RoleViewer, "/plugins/slot?slot=settings.form&plugin=demo&slot_id=x"); code != http.StatusBadRequest {
		t.Errorf("host-rendered slot = %d", code)
	}
}

func TestHostPageHasSectionPlaceholders(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.offers = []plugin.SlotOffer{offer("demo", plugin.SlotHostSection, "hs", plugin.RoleViewer)}
	e := newPluginEnv(t, m)
	_, body := fragment(t, e, RoleViewer, "/host")
	if !strings.Contains(body, "slot=host.section") || len(m.renders) != 0 {
		t.Errorf("host sections missing or called synchronously")
	}
}

func TestActionBackPathStaysOnThisSite(t *testing.T) {
	cases := map[string]string{
		"":                               "/",
		"http://example.test/monitoring": "/monitoring",
		"http://example.test/n/abc/monitoring?x=1": "/n/abc/monitoring?x=1",
		"http://evil.test/monitoring":              "/",
		"http://example.test//evil.test/x":         "/",
		"http://example.test/plugins/action":       "/",
		"not a url %%":                             "/",
	}
	for ref, want := range cases {
		r := httptest.NewRequest(http.MethodPost, "http://example.test/plugins/action", nil)
		if ref != "" {
			r.Header.Set("Referer", ref)
		}
		if got := actionBackPath(r); got != want {
			t.Errorf("referer %q: back %q, want %q", ref, got, want)
		}
	}
}

func TestContainerSectionPlaceholderNamesThePlugin(t *testing.T) {
	m := newFakeManager(demoStatus())
	o := offer("demo", plugin.SlotContainerSection, "cs", plugin.RoleViewer)
	o.Title = "Resource <watch>"
	a := offer("demo", plugin.SlotContainerAction, "ca", plugin.RoleViewer)
	a.Title = "ActionTitle"
	m.offers = []plugin.SlotOffer{o, a}
	e := newPluginEnv(t, m)
	_, body := fragment(t, e, RoleViewer, "/plugins/slot?slot=container.section&container=web")
	if !strings.Contains(body, "Resource &lt;watch&gt;") {
		t.Errorf("section placeholder lacks its title: %s", body)
	}
	_, body = fragment(t, e, RoleViewer, "/plugins/slot?slot=container.action&container=web")
	if strings.Contains(body, "ActionTitle") {
		t.Errorf("action placeholder should not carry a section heading: %s", body)
	}
}

func TestDashboardPanelKeepsItsTitleAfterLoading(t *testing.T) {
	m := newFakeManager(statusNamed("a"))
	o := offer("a", plugin.SlotDashboardPanel, "one", plugin.RoleViewer)
	o.Title = "Uptime overview"
	m.offers = []plugin.SlotOffer{o}
	e := newPluginEnv(t, m)
	e.post(RoleViewer, "/dashboard/panels", map[string][]string{"panel": {"a/one"}})
	_, dash := fragment(t, e, RoleViewer, "/")
	i := strings.Index(dash, `class="plugin-panel"`)
	if i < 0 {
		t.Fatal("no plugin panel")
	}
	panel := dash[i:]
	title, loader := strings.Index(panel, "Uptime overview"), strings.Index(panel, "hx-get=")
	if title < 0 || loader < 0 || title > loader {
		t.Errorf("panel title must sit outside the element htmx replaces:\n%.400s", panel)
	}
}
