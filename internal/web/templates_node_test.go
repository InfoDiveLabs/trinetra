package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// ---- link audit: every same-origin href/action/hx-get on a node page is
// either node-prefixed or in the master-local allowlist ----------------

// sameOriginAttr matches an href/action/hx-get attribute's double-quoted value, mirroring
// brand_test.go's own regex-over-rendered-HTML convention.
var sameOriginAttr = regexp.MustCompile(`(?:href|action|hx-get)\s*=\s*"([^"]*)"`)

// nodeLinkAllowlist is the master-local allowlist: a same-origin URL on a /n/{node}/...
// page that ISN'T prefixed with the node's own prefix must start with one of these instead.
var nodeLinkAllowlist = []string{
	"/fleet",
	"/config",
	"/channels",
	"/users",
	"/settings/public",
	"/logout",
	"/assets/",
}

// allowedNodeLink reports whether url is either prefixed with prefix or
// falls under nodeLinkAllowlist -- the audit's pass/fail rule.
func allowedNodeLink(url, prefix string) bool {
	if url == "" || url == "#" {
		return true
	}
	if strings.HasPrefix(url, prefix+"/") || url == prefix {
		return true
	}
	for _, a := range nodeLinkAllowlist {
		if url == a || strings.HasPrefix(url, a) {
			return true
		}
	}
	return false
}

// nodeSwitcherItemTag matches a topbar node-switcher entry's own opening <a> tag.
var nodeSwitcherItemTag = regexp.MustCompile(`<a[^>]*\bclass="ns-item[^"]*"[^>]*>`)

// TestNodeScopedPagesLinkAudit renders every node-routable page under /n/child1/.
func TestNodeScopedPagesLinkAudit(t *testing.T) {
	master := fakeAPI{snap: core.DashboardView{TopCPUContainers: []core.ContainerView{{Name: "svc", State: "running"}}}}
	child := fakeAPI{snap: core.DashboardView{TopCPUContainers: []core.ContainerView{{Name: "child-svc", State: "running"}}},
		active: []core.AlertRecord{{Key: "k1", Time: 1, Source: "child alert"}}}
	d := nodeScopedDeps(t, master, child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	for _, page := range []string{"/", "/monitoring", "/host", "/alerts", "/history"} {
		t.Run(page, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/n/child1"+page))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
			}
			body := nodeSwitcherItemTag.ReplaceAllString(rr.Body.String(), "<removed-ns-item>")
			for _, m := range sameOriginAttr.FindAllStringSubmatch(body, -1) {
				url := m[1]
				if strings.Contains(url, "://") || strings.HasPrefix(url, "//") {
					continue // external/protocol-relative, not this audit's concern
				}
				if !allowedNodeLink(url, "/n/child1") {
					t.Errorf("%s: same-origin link %q is neither /n/child1-prefixed nor in the master-local allowlist", page, url)
				}
			}
		})
	}
}

// TestNodeScopedNavHidesAdminGroup pins that on a remote node page, the whole Admin nav
// group (heading + Configuration/Channels/Users/Public view) is absent, even for an admin.
func TestNodeScopedNavHidesAdminGroup(t *testing.T) {
	master := fakeAPI{}
	child := fakeAPI{}
	d := nodeScopedDeps(t, master, child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/n/child1/monitoring"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{">Server settings<", ">Notifications<", ">Users<", ">Public view<", `class="grp eyebrow">Settings<`} {
		if strings.Contains(body, want) {
			t.Errorf("remote node page nav should hide %q:\n%s", want, body)
		}
	}
}

// ---- replica banner: text/accent by NodeSummary.State ------------------

// bannerFor renders a /n/child1/ dashboard for a child whose fleet roster
// entry has the given NodeSummary fields, returning the response body.
func bannerFor(t *testing.T, summary core.NodeSummary) string {
	t.Helper()
	fleet := &fakeFleet{
		status: core.FleetStatus{Role: config.RoleMaster},
		nodes: []core.NodeSummary{
			{ID: core.SelfNodeID, Self: true, State: "online"},
			summary,
		},
	}
	master := fakeAPI{fleet: fleet, nodes: map[string]core.API{"child1": fakeAPI{}}}
	d := fleetTestDeps(t, master)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

// TestNodeBannerOnlineState pins the online banner's exact text and the
// verdigris (.led.ok) accent -- "verdigris is only for online/linked".
func TestNodeBannerOnlineState(t *testing.T) {
	body := bannerFor(t, core.NodeSummary{
		ID: "child1", Name: "child1", State: "online",
		LastSeen: time.Now().Add(-3 * time.Second).Unix(),
	})
	if !strings.Contains(body, "Viewing child1 · replica, updated 3s ago") {
		t.Errorf("online banner missing exact text:\n%s", body)
	}
	if !strings.Contains(body, `<div class="nodebanner" role="status">`) {
		t.Errorf("online banner should carry no ember/amber accent class:\n%s", body)
	}
	if !strings.Contains(body, `<span class="led ok"></span> Viewing child1`) {
		t.Errorf("online banner missing the verdigris (.led.ok) accent:\n%s", body)
	}
}

// TestNodeBannerCatchingUpState pins the "catching up, N behind" text, sourced from
// NodeSummary.OutboxBytes/OutboxOldest, with the neutral/amber "nb-behind" accent.
func TestNodeBannerCatchingUpState(t *testing.T) {
	body := bannerFor(t, core.NodeSummary{
		ID: "child1", Name: "child1", State: "catching up",
		OutboxBytes: 2 * 1 << 20, OutboxOldest: time.Now().Add(-4 * time.Minute).Unix(),
	})
	if !strings.Contains(body, "Viewing child1 · replica, catching up, 2 MB, oldest 4m ago behind") {
		t.Errorf("catching-up banner missing exact behind text:\n%s", body)
	}
	if !strings.Contains(body, `class="nodebanner nb-behind"`) {
		t.Errorf("catching-up banner should carry the neutral/amber nb-behind class, not ember:\n%s", body)
	}
	if strings.Contains(body, "nb-down") {
		t.Errorf("catching-up banner must never carry the ember nb-down class:\n%s", body)
	}
}

// TestNodeBannerLaggingState pins that "lagging".
func TestNodeBannerLaggingState(t *testing.T) {
	body := bannerFor(t, core.NodeSummary{
		ID: "child1", Name: "child1", State: "lagging",
		OutboxBytes: 512 * 1024,
	})
	if !strings.Contains(body, "Viewing child1 · replica, catching up, 512 KB behind") {
		t.Errorf("lagging banner missing exact behind text:\n%s", body)
	}
	if !strings.Contains(body, `class="nodebanner nb-behind"`) {
		t.Errorf("lagging banner should carry the nb-behind class:\n%s", body)
	}
}

// TestNodeBannerDownState pins the down banner's exact text and its ember
// (.led.crit / .nb-down) accent -- the ONE state that gets ember.
func TestNodeBannerDownState(t *testing.T) {
	lastSeen := time.Date(2024, 1, 1, 14, 2, 0, 0, time.Local).Unix()
	body := bannerFor(t, core.NodeSummary{ID: "child1", Name: "child1", State: "down", LastSeen: lastSeen})
	if !strings.Contains(body, "child1 is down since 14:02 — showing the last data received") {
		t.Errorf("down banner missing exact text:\n%s", body)
	}
	if !strings.Contains(body, `class="nodebanner nb-down"`) {
		t.Errorf("down banner missing the ember nb-down class:\n%s", body)
	}
	if !strings.Contains(body, `<span class="led crit"></span> child1 is down`) {
		t.Errorf("down banner missing the ember (.led.crit) accent:\n%s", body)
	}
}

// TestNodeBannerStaleState pins that "stale" renders the SAME "is down since..." text as
// "down" but WITHOUT the ember accent -- "ember accent only for down".
func TestNodeBannerStaleState(t *testing.T) {
	lastSeen := time.Date(2024, 1, 1, 14, 2, 0, 0, time.Local).Unix()
	body := bannerFor(t, core.NodeSummary{ID: "child1", Name: "child1", State: "stale", LastSeen: lastSeen})
	if !strings.Contains(body, "child1 is down since 14:02 — showing the last data received") {
		t.Errorf("stale banner missing exact text:\n%s", body)
	}
	if strings.Contains(body, "nb-down") {
		t.Errorf("stale banner must not carry the ember nb-down class:\n%s", body)
	}
	if strings.Contains(body, `led crit`) {
		t.Errorf("stale banner must not carry the ember (.led.crit) accent:\n%s", body)
	}
}

// TestNodeBannerRevokedState pins the revoked banner's exact, neutral text.
func TestNodeBannerRevokedState(t *testing.T) {
	body := bannerFor(t, core.NodeSummary{ID: "child1", Name: "child1", State: "revoked"})
	if !strings.Contains(body, "child1 was revoked") {
		t.Errorf("revoked banner missing exact text:\n%s", body)
	}
	if strings.Contains(body, "nb-down") || strings.Contains(body, "nb-behind") {
		t.Errorf("revoked banner should carry no accent class:\n%s", body)
	}
}

// TestNodeBannerAbsentOnSelfPages pins that the banner never renders for a
// self-scoped page (master's own dashboard), even on a fleet master.
func TestNodeBannerAbsentOnSelfPages(t *testing.T) {
	fleet := &fakeFleet{status: core.FleetStatus{Role: config.RoleMaster}, nodes: []core.NodeSummary{
		{ID: core.SelfNodeID, Self: true, State: "online"},
		{ID: "child1", State: "down"},
	}}
	d := fleetTestDeps(t, fakeAPI{fleet: fleet})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `class="nodebanner`) {
		t.Errorf("master's own dashboard must not render a replica banner:\n%s", rr.Body.String())
	}
}

// ---- child topbar link pill ---------------------------------------------

// childPillFor renders a child daemon's own dashboard with the given
// Fleet().Status() Link, returning the response body.
func childPillFor(t *testing.T, link *core.LinkView) string {
	t.Helper()
	fleet := &fakeFleet{status: core.FleetStatus{Role: config.RoleChild, NodeID: "child1", MasterURL: "https://master.example:8443", Link: link}}
	d := fleetTestDeps(t, fakeAPI{fleet: fleet})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

// TestChildTopbarPillLinkedHealthy pins the healthy verdigris pill.
func TestChildTopbarPillLinkedHealthy(t *testing.T) {
	body := childPillFor(t, &core.LinkView{State: "linked", LastAck: time.Now().Add(-2 * time.Second).Unix()})
	if !strings.Contains(body, "Linked to master · ack 2s ago") {
		t.Errorf("child pill missing exact healthy text:\n%s", body)
	}
	if !strings.Contains(body, `<span class="led ok"></span> Linked to master`) {
		t.Errorf("child pill missing the verdigris (.led.ok) dot:\n%s", body)
	}
	if strings.Contains(body, "Master unreachable") {
		t.Errorf("healthy child pill must not render the unreachable text:\n%s", body)
	}
}

// TestChildTopbarPillUnreachableAfterTwoMinutes pins the amber pill once a
// "retrying" link has gone more than 2 minutes without an ack.
func TestChildTopbarPillUnreachableAfterTwoMinutes(t *testing.T) {
	body := childPillFor(t, &core.LinkView{State: "retrying", LastAck: time.Now().Add(-12 * time.Minute).Unix()})
	if !strings.Contains(body, "Master unreachable 12m · alerting locally") {
		t.Errorf("child pill missing exact unreachable text:\n%s", body)
	}
	if !strings.Contains(body, `<span class="led warn"></span> Master unreachable`) {
		t.Errorf("child pill missing the amber (.led.warn) dot:\n%s", body)
	}
}

// TestChildTopbarPillRetryingUnderTwoMinutesStaysHealthy pins the threshold: a "retrying"
// link under 2 minutes still renders the healthy pill, not the amber one.
func TestChildTopbarPillRetryingUnderTwoMinutesStaysHealthy(t *testing.T) {
	body := childPillFor(t, &core.LinkView{State: "retrying", LastAck: time.Now().Add(-30 * time.Second).Unix()})
	if strings.Contains(body, "Master unreachable") {
		t.Errorf("a retrying link under the 2m threshold must not render the unreachable pill:\n%s", body)
	}
	if !strings.Contains(body, "Linked to master · ack 30s ago") {
		t.Errorf("child pill missing the healthy fallback text:\n%s", body)
	}
}

// TestChildTopbarPillAbsentWithoutFleetLink pins that a "child"-role daemon whose Status()
// reports no Link at all renders no pill (no panic, no empty markup).
func TestChildTopbarPillAbsentWithoutFleetLink(t *testing.T) {
	body := childPillFor(t, nil)
	if strings.Contains(body, "Linked to master") || strings.Contains(body, "Master unreachable") {
		t.Errorf("child pill should be absent when Status().Link is nil:\n%s", body)
	}
}

// ---- solo: no fleet markup leaks at all ---------------------------------

// TestSoloRendersNoFleetMarkup is a positive-absence check: solo renders no switcher, no
// banner, no child badge, and no "/n/" link anywhere on its dashboard.
func TestSoloRendersNoFleetMarkup(t *testing.T) {
	fleet := &fakeFleet{status: core.FleetStatus{Role: config.RoleSolo, Nodes: 1}}
	d := fleetTestDeps(t, fakeAPI{fleet: fleet, snap: core.DashboardView{TopCPUContainers: []core.ContainerView{{Name: "svc", State: "running"}}}})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "/n/") {
		t.Errorf("solo page must not render any /n/ link:\n%s", body)
	}
	if strings.Contains(body, `class="nodebanner`) {
		t.Errorf("solo page must not render a replica banner:\n%s", body)
	}
	if strings.Contains(body, "Linked to master") || strings.Contains(body, "Master unreachable") {
		t.Errorf("solo page must not render a child link badge:\n%s", body)
	}
	if strings.Contains(body, "node-switcher") || strings.Contains(body, "nodeSwitcher") {
		t.Errorf("solo page must not render a node switcher:\n%s", body)
	}
}

// TestSoloDeposWithNoFleetRendersNoFleetMarkup covers the more common test fixture shape
// (d.Fleet left nil entirely, as most of this package's pre-fleet tests do).
func TestSoloDepsWithNoFleetRendersNoFleetMarkup(t *testing.T) {
	d := enrollTestDeps(t)
	d.API = fakeAPI{snap: core.DashboardView{}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "/n/") || strings.Contains(body, `class="nodebanner`) ||
		strings.Contains(body, "Linked to master") || strings.Contains(body, "Master unreachable") {
		t.Errorf("a Deps with no Fleet wired at all must render no fleet markup:\n%s", body)
	}
}

// ---- Fleet().Status() call budget ---------------------------------------

// countingFleet wraps a core.FleetAPI and counts Status()/Nodes() calls separately, so a
// test can pin that d.Fleet().Status() is called at most once per request.
type countingFleet struct {
	core.FleetAPI
	statusCalls *int
	nodesCalls  *int
}

func (c countingFleet) Status() (core.FleetStatus, error) {
	if c.statusCalls != nil {
		*c.statusCalls++
	}
	return c.FleetAPI.Status()
}

func (c countingFleet) Nodes(f core.NodeFilter) ([]core.NodeSummary, error) {
	if c.nodesCalls != nil {
		*c.nodesCalls++
	}
	return c.FleetAPI.Nodes(f)
}

var _ core.FleetAPI = countingFleet{}

// TestFleetStatusCalledAtMostOnceOnRemoteNodePage pins the ctx-known-master short circuit:
// a /n/child1/... request's Fleet().Status() call count is 0.
func TestFleetStatusCalledAtMostOnceOnRemoteNodePage(t *testing.T) {
	n := 0
	realFleet := masterFleetWithChild()
	master := fakeAPI{
		fleet: nil, // set below via a func wrapper so Fleet() returns the counting proxy
		nodes: map[string]core.API{"child1": fakeAPI{}},
	}
	master.fleet = realFleet
	d := fleetTestDeps(t, master)
	d.Fleet = func() core.FleetAPI { return countingFleet{FleetAPI: realFleet, statusCalls: &n} }
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/monitoring"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if n != 0 {
		t.Errorf("Fleet().Status() called %d time(s) for a /n/child1/... request, want 0 (role already known from resolveMasterAndNodes)", n)
	}
}

// TestFleetStatusCalledExactlyOnceOnSelfPage pins the budget's other half: an ordinary
// self-scoped page.
func TestFleetStatusCalledExactlyOnceOnSelfPage(t *testing.T) {
	n := 0
	realFleet := masterFleetWithChild()
	d := fleetTestDeps(t, fakeAPI{fleet: realFleet})
	d.Fleet = func() core.FleetAPI { return countingFleet{FleetAPI: realFleet, statusCalls: &n} }
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if n != 1 {
		t.Errorf("Fleet().Status() called %d time(s) for a self-scoped page, want exactly 1", n)
	}
}

// ---- child topbar pill / node badge edge cases ----------------------------

// TestChildTopbarPillConnectingWhenNeverAcked pins that a child link that has never once
// acked (LastAck<=0) but isn't.
func TestChildTopbarPillConnectingWhenNeverAcked(t *testing.T) {
	body := childPillFor(t, &core.LinkView{State: "linked", LastAck: 0})
	if !strings.Contains(body, "Connecting to master") {
		t.Errorf("child pill missing \"Connecting to master\" for a never-acked link:\n%s", body)
	}
	if strings.Contains(body, "ack never") {
		t.Errorf("child pill must not render the old \"ack never\" text:\n%s", body)
	}
	if strings.Contains(body, "Master unreachable") {
		t.Errorf("a never-acked, non-retrying link must not render the unreachable pill:\n%s", body)
	}
}

// TestChildBadgeAbsentWhenFleetStatusErrors pins that when Fleet().Status() itself errors
// (a transient control-socket hiccup, or an old daemon that doesn't implement it yet).
func TestChildBadgeAbsentWhenFleetStatusErrors(t *testing.T) {
	fleet := &fakeFleet{statusErr: errors.New("control socket unavailable")}
	d := fleetTestDeps(t, fakeAPI{fleet: fleet})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "Linked to master") || strings.Contains(body, "Master unreachable") || strings.Contains(body, "Connecting to master") {
		t.Errorf("no child badge/pill must render when Fleet().Status() errors:\n%s", body)
	}
}

// ---- <body data-node-prefix> ----------------------------------------

// TestBodyDataNodePrefixOnNodeScopedPage pins that a master's node-scoped page
// (/n/{id}/...) renders <body data-node-prefix> with that node's URL prefix.
func TestBodyDataNodePrefixOnNodeScopedPage(t *testing.T) {
	d := nodeScopedDeps(t, fakeAPI{}, fakeAPI{})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `data-node-prefix="/n/child1"`) {
		t.Errorf("node-scoped page missing data-node-prefix=\"/n/child1\":\n%s", rr.Body.String())
	}
}

// TestBodyDataNodePrefixEmptyOnSelfPage pins the other half: every self-scoped page (solo,
// a master's own view, a child's own view) renders the attribute present but empty.
func TestBodyDataNodePrefixEmptyOnSelfPage(t *testing.T) {
	d := enrollTestDeps(t)
	d.API = fakeAPI{}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `data-node-prefix=""`) {
		t.Errorf("self page missing empty data-node-prefix=\"\":\n%s", rr.Body.String())
	}
}

// ---- app.js's same-origin data calls go through nodeURL() ---------------

// appJSNodeScopedLiteral matches a quoted string literal that is one of this app's
// node-scoped data URLs.
var appJSNodeScopedLiteral = regexp.MustCompile(`(nodeURL\(\s*)?['"](/api/[^'"]*|/events)['"]`)

// TestAppJSDataFetchesGoThroughNodeURL pins that every fetch()/EventSource() app.js makes
// against THIS daemon's own data API.
func TestAppJSDataFetchesGoThroughNodeURL(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(b)

	if !strings.Contains(src, "function nodeURL(") {
		t.Fatal("app.js is missing a nodeURL(path) helper")
	}

	matches := appJSNodeScopedLiteral.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		t.Fatal("app.js: found no /api/* or /events literal to check -- the scan regex may be stale")
	}
	unwrapped := 0
	for _, m := range matches {
		wrapped, path := m[1], m[2]
		if path == "/api/fleet/nodes" {
			// Deliberately exempt: the Ctrl-K palette fetches the FLEET roster, a master-local
			// endpoint.
			continue
		}
		if wrapped == "" {
			unwrapped++
			t.Errorf("app.js: %q is a same-origin /api or /events URL not routed through nodeURL(...)", path)
		}
	}
	if unwrapped > 0 {
		t.Errorf("%d unwrapped node-scoped data URL(s) in app.js", unwrapped)
	}
}

// ---- topbar node switcher + Ctrl-K palette ---------------------------

// switcherFixtureNodes is this section's shared roster: self (online), web1 (online), db1
// (down).
func switcherFixtureNodes() []core.NodeSummary {
	return []core.NodeSummary{
		{ID: core.SelfNodeID, Self: true, State: "online"},
		{ID: "web1", Name: "web1", State: "online"},
		{ID: "db1", Name: "db1", State: "down"},
	}
}

// switcherTestDeps builds a master Deps whose fleet roster is nodes, with a fake per-node
// core.API registered for every non-self id in nodes.
func switcherTestDeps(t *testing.T, nodes []core.NodeSummary) Deps {
	t.Helper()
	perNode := map[string]core.API{}
	for _, n := range nodes {
		if !n.Self {
			perNode[n.ID] = fakeAPI{}
		}
	}
	fleet := &fakeFleet{status: core.FleetStatus{Role: config.RoleMaster}, nodes: nodes}
	return fleetTestDeps(t, masterFakeAPI(fleet, perNode))
}

// TestNodeSwitcherListsNodesAndPreservesPageType pins the switcher's core contract on a
// master: the topbar button/listbox render (role="listbox", no <select>).
func TestNodeSwitcherListsNodesAndPreservesPageType(t *testing.T) {
	d := switcherTestDeps(t, switcherFixtureNodes())
	rr := fleetGetAsViewer(t, d, "/n/web1/monitoring")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	if !strings.Contains(body, `id="nodeSwitcher"`) {
		t.Errorf("missing #nodeSwitcher:\n%s", body)
	}
	if !strings.Contains(body, `role="listbox"`) {
		t.Errorf("switcher menu missing role=\"listbox\":\n%s", body)
	}
	if !strings.Contains(body, ">web1<") {
		t.Errorf("switcher button missing current node's name (web1):\n%s", body)
	}

	// Each entry links to the SAME page type ("/monitoring") under its own
	// node's prefix -- self unprefixed, web1/db1 under /n/<id>.
	for _, want := range []string{
		`href="/monitoring"`,        // self
		`href="/n/web1/monitoring"`, // current node
		`href="/n/db1/monitoring"`,  // down node
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing switcher entry %q:\n%s", want, body)
		}
	}
	// The current node (web1) carries the "current" marker.
	if !strings.Contains(body, `href="/n/web1/monitoring" role="option" class="ns-item current" aria-selected="true"`) {
		t.Errorf("web1 switcher entry missing the current marker:\n%s", body)
	}
	// Self shows "this server", never its (empty) roster name.
	if !strings.Contains(body, `<span class="ns-name">this server</span>`) {
		t.Errorf("self switcher entry missing \"this server\" label:\n%s", body)
	}
	// State text always renders next to the led dot, never color-only.
	if !strings.Contains(body, `<span class="ns-state note">down</span>`) {
		t.Errorf("db1 switcher entry missing its state text:\n%s", body)
	}
	// "View all in Fleet" link.
	if !strings.Contains(body, `<a class="ns-viewall" href="/fleet">View all in Fleet`) {
		t.Errorf("switcher menu missing the \"View all in Fleet\" link:\n%s", body)
	}
}

// TestNodeSwitcherMasterLocalPageSwitchesToDashboard pins that viewing a master-local page
// (e.g. /fleet) from ANY node scope, every switcher entry links to that node's dashboard.
func TestNodeSwitcherMasterLocalPageSwitchesToDashboard(t *testing.T) {
	d := switcherTestDeps(t, switcherFixtureNodes())
	rr := fleetGetAsViewer(t, d, "/fleet")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{`href="/"`, `href="/n/web1/"`, `href="/n/db1/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /fleet: expected switcher entry %q (master-local page switches to the node's dashboard):\n%s", want, body)
		}
	}
	if strings.Contains(body, `href="/n/web1/fleet"`) || strings.Contains(body, `href="/n/db1/fleet"`) {
		t.Errorf("GET /fleet: switcher must not link to a node-scoped /fleet (master-local, no per-node counterpart):\n%s", body)
	}
}

// TestNodePaletteMarkupPresentOnMaster pins the palette's server-rendered
// (hidden) markup: #nodePalette with data-src="/api/fleet/nodes".
func TestNodePaletteMarkupPresentOnMaster(t *testing.T) {
	d := switcherTestDeps(t, switcherFixtureNodes())
	rr := fleetGetAsViewer(t, d, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `id="nodePalette" data-src="/api/fleet/nodes"`) {
		t.Errorf("missing #nodePalette with data-src=\"/api/fleet/nodes\":\n%s", body)
	}
	if !strings.Contains(body, `id="nodePalette" data-src="/api/fleet/nodes" role="dialog" aria-modal="true" aria-label="Switch node" hidden>`) {
		t.Errorf("#nodePalette must render hidden by default:\n%s", body)
	}
}

// TestNodeSwitcherAndPaletteAbsentOnSoloAndChild pins that solo and child
// daemons get no switcher and no palette.
func TestNodeSwitcherAndPaletteAbsentOnSoloAndChild(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps func(t *testing.T) Deps
	}{
		{"solo", fleetSoloDeps},
		{"child", fleetChildDeps},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.deps(t)
			rr := fleetGetAsViewer(t, d, "/")
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
			}
			body := rr.Body.String()
			if strings.Contains(body, "nodeSwitcher") {
				t.Errorf("%s page must not render a node switcher:\n%s", tc.name, body)
			}
			if strings.Contains(body, "nodePalette") {
				t.Errorf("%s page must not render the Ctrl-K palette:\n%s", tc.name, body)
			}
		})
	}
}

// TestNodePaletteFetchesFleetNodesAbsolute pins that the palette's JS must fetch the
// literal absolute '/api/fleet/nodes' (master-local, no per-node counterpart).
func TestNodePaletteFetchesFleetNodesAbsolute(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, `fetch('/api/fleet/nodes',{credentials:'same-origin'})`) {
		t.Errorf("app.js missing the palette's literal fetch('/api/fleet/nodes',{credentials:'same-origin'})")
	}
	if strings.Contains(src, `nodeURL('/api/fleet/nodes')`) || strings.Contains(src, `nodeURL("/api/fleet/nodes")`) {
		t.Errorf("app.js must not route /api/fleet/nodes through nodeURL(...) -- it's master-local")
	}
}

// TestNodeSwitcherFleetStatusCallBudgetUnchanged pins that building the switcher costs no
// EXTRA Fleet() round trip.
func TestNodeSwitcherFleetStatusCallBudgetUnchanged(t *testing.T) {
	n := 0
	realFleet := &fakeFleet{status: core.FleetStatus{Role: config.RoleMaster}, nodes: switcherFixtureNodes()}
	d := fleetTestDeps(t, masterFakeAPI(realFleet, map[string]core.API{"web1": fakeAPI{}, "db1": fakeAPI{}}))
	d.Fleet = func() core.FleetAPI { return countingFleet{FleetAPI: realFleet, nodesCalls: &n} }

	rr := fleetGetAsViewer(t, d, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if n != 1 {
		t.Errorf("Fleet().Nodes() called %d time(s) for a self-scoped master page, want exactly 1 (shared fleetMemo)", n)
	}
}

// ---- switcher/palette regressions --------------------------------------

// TestNodeSwitcherPreservesQueryString pins that item (a): a page with a query string (e.g.
// /history?metric=cpu) keeps it when the switcher switches node.
func TestNodeSwitcherPreservesQueryString(t *testing.T) {
	d := switcherTestDeps(t, switcherFixtureNodes())
	rr := fleetGetAsViewer(t, d, "/n/web1/history?metric=cpu")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`href="/history?metric=cpu"`,        // self
		`href="/n/web1/history?metric=cpu"`, // current node
		`href="/n/db1/history?metric=cpu"`,  // down node
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing query-preserving switcher entry %q:\n%s", want, body)
		}
	}
}

// TestNodeSwitcherMasterLocalPageDropsQueryString pins the other half of (a): a
// master-local page's own query string.
func TestNodeSwitcherMasterLocalPageDropsQueryString(t *testing.T) {
	d := switcherTestDeps(t, switcherFixtureNodes())
	rr := fleetGetAsViewer(t, d, "/fleet?state=down")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{`href="/"`, `href="/n/web1/"`, `href="/n/db1/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected switcher entry %q with no leftover query string:\n%s", want, body)
		}
	}
	// The health strip's own "?state=down" count links are unrelated and expected to remain --
	// scope the negative assertion to the switcher's own entries specifically.
	for _, unwanted := range []string{`href="/?state=down"`, `href="/n/web1/?state=down"`, `href="/n/db1/?state=down"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("switcher must not carry /fleet's own query string onto a node's dashboard, found %q:\n%s", unwanted, body)
		}
	}
}

// paletteFocusableFunc matches app.js's paletteFocusable() function body --
// TestNodePaletteFocusTrapIncludesFooterLink is a static (source-scan) check.
var paletteFocusableFunc = regexp.MustCompile(`(?s)function paletteFocusable\(\)\{.*?\n    \}`)

// TestNodePaletteFocusTrapIncludesFooterLink pins that review's IMPORTANT fix: the
// palette's focus trap must include its own footer link.
func TestNodePaletteFocusTrapIncludesFooterLink(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(b)

	// #nodePaletteViewAll's id lives in base.html, not app.js -- checked directly (app.js only
	// ever REFERENCES the id via getElementById, asserted below).
	htmlB, err := templatesFS.ReadFile("templates/base.html")
	if err != nil {
		t.Fatalf("read base.html: %v", err)
	}
	if !strings.Contains(string(htmlB), `id="nodePaletteViewAll"`) {
		t.Fatal("templates/base.html: the palette's \"View all in Fleet\" link is missing id=\"nodePaletteViewAll\"")
	}

	m := paletteFocusableFunc.FindString(src)
	if m == "" {
		t.Fatal("app.js: missing a paletteFocusable() function to scan -- the focus trap must build its cycle from a single function so this scan (and the trap itself) can't drift apart")
	}
	if !strings.Contains(m, `.ns-item`) {
		t.Errorf("paletteFocusable() must include the rendered .ns-item results:\n%s", m)
	}
	if !strings.Contains(m, "viewAllLink") {
		t.Errorf("paletteFocusable() must include the footer's #nodePaletteViewAll link as the trap's true last element:\n%s", m)
	}

	// The trap handler itself must call paletteFocusable(), not rebuild its
	// own (footer-link-less) list inline.
	if !strings.Contains(src, "var focusable=paletteFocusable();") {
		t.Error("app.js: the Tab-trap keydown handler must build its cycle from paletteFocusable(), not an inline list that leaves the footer link out")
	}
}

// TestNodePaletteOpeningClosesSwitcher pins that item (b): opening the palette (Ctrl/Cmd-K)
// must close the switcher dropdown first.
func TestNodePaletteOpeningClosesSwitcher(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "function openPalette(){")
	if i < 0 {
		t.Fatal("app.js: missing function openPalette(){...}")
	}
	// The function body's first statement should be the closeSwitcher() call.
	window := src[i : i+200]
	if !strings.Contains(window, "closeSwitcher();") {
		t.Errorf("app.js: openPalette() must call closeSwitcher() so opening the palette closes the switcher dropdown:\n%s", window)
	}
}

// ---- switcher polish (recent nodes + page-type jump) -------------------

// TestNodePaletteRecentNodesUseLocalStorageWithTryCatch pins that the palette's.
func TestNodePaletteRecentNodesUseLocalStorageWithTryCatch(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(b)

	if !strings.Contains(src, "window.localStorage.getItem(RECENT_KEY)") {
		t.Error("app.js: missing a localStorage.getItem read for the recent-nodes list")
	}
	if !strings.Contains(src, "window.localStorage.setItem(RECENT_KEY") {
		t.Error("app.js: missing a localStorage.setItem write for the recent-nodes list")
	}
	if !strings.Contains(src, "RECENT_MAX=5") {
		t.Error("app.js: recent-nodes list must be capped at 5")
	}

	i := strings.Index(src, "function loadRecentNodes(){")
	if i < 0 {
		t.Fatal("app.js: missing function loadRecentNodes(){...}")
	}
	if fn := src[i : i+250]; !strings.Contains(fn, "try{") || !strings.Contains(fn, "catch(e)") {
		t.Errorf("app.js: loadRecentNodes() must wrap its localStorage read in try/catch:\n%s", fn)
	}

	j := strings.Index(src, "function recordRecentNode(id){")
	if j < 0 {
		t.Fatal("app.js: missing function recordRecentNode(id){...}")
	}
	if fn := src[j : j+400]; !strings.Contains(fn, "try{") || !strings.Contains(fn, "catch(e)") {
		t.Errorf("app.js: recordRecentNode() must wrap its localStorage write in try/catch:\n%s", fn)
	}

	// "self" is never recorded -- it's already the switcher/palette's own
	// fixed first-class entry.
	if !strings.Contains(src, `if(!id || id==='self') return;`) {
		t.Error("app.js: recordRecentNode() must skip \"self\"")
	}

	// Every page load (that has the palette at all -- master-only chrome)
	// records its own current node.
	if !strings.Contains(src, "recordRecentNode(currentNodeID());") {
		t.Error("app.js: missing a call to record the CURRENT page's node as visited")
	}

	// renderResults actually consults the recent list to reorder results
	// when no query has been typed yet.
	k := strings.Index(src, "function renderResults(nodes){")
	if k < 0 {
		t.Fatal("app.js: missing function renderResults(nodes){...}")
	}
	if fn := src[k : k+900]; !strings.Contains(fn, "loadRecentNodes()") {
		t.Errorf("app.js: renderResults() must consult loadRecentNodes() to surface recent nodes first:\n%s", fn)
	}
}

// TestNodePalettePageTypeJumpParsesTrailingPageWord pins that typing "<name> <pagetype>"
// (e.g. "web1 history") targets that specific page type on the matched node(s).
func TestNodePalettePageTypeJumpParsesTrailingPageWord(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(b)

	if !strings.Contains(src, `var PAGE_TYPES=['dashboard','monitoring','host','alerts','history'];`) {
		t.Error("app.js: missing the exact 5-entry PAGE_TYPES list (dashboard/monitoring/host/alerts/history)")
	}
	if !strings.Contains(src, "function parsePaletteQuery(raw){") {
		t.Fatal("app.js: missing function parsePaletteQuery(raw){...}")
	}
	if !strings.Contains(src, "function pageTypePath(t){") {
		t.Error("app.js: missing function pageTypePath(t){...} to render a page-type's own path")
	}
	// hrefFor must accept and use the parsed page type, not just currentTargetPath().
	if !strings.Contains(src, "function hrefFor(node,pageType){") {
		t.Error("app.js: hrefFor() must take a pageType parameter")
	}
	if !strings.Contains(src, "row.href=hrefFor(n,parsed.pageType);") {
		t.Error("app.js: renderResults() must pass the parsed page type through to hrefFor()")
	}
}
