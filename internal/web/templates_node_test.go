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

// sameOriginAttr matches an href/action/hx-get attribute's double-quoted
// value, mirroring brand_test.go's own regex-over-rendered-HTML convention
// (this package's tests don't pull in an HTML parser -- see
// TestNoExternalAssetURLs's externalAssetURL for the precedent).
var sameOriginAttr = regexp.MustCompile(`(?:href|action|hx-get)\s*=\s*"([^"]*)"`)

// nodeLinkAllowlist is task-3-brief.md's exact master-local allowlist: a
// same-origin URL on a /n/{node}/... page that ISN'T prefixed with the
// node's own prefix must start with one of these instead.
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

// TestNodeScopedPagesLinkAudit renders every node-routable page under
// /n/child1/ and asserts every same-origin href/action/hx-get is either
// node-prefixed or in the master-local allowlist (task-3-brief.md step 1).
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
			body := rr.Body.String()
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

// TestNodeScopedNavHidesAdminGroup pins task-3-brief.md's nav ruling: on a
// remote node page, the whole Admin nav group (heading + Configuration/
// Channels/Users/Public view) is absent, even for an admin -- those pages
// are master-local and stay reachable only from the master's own nav.
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
	for _, want := range []string{">Configuration<", ">Channels<", ">Users<", ">Public view<", `class="grp eyebrow">Admin<`} {
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

// TestNodeBannerCatchingUpState pins the "catching up, N behind" text,
// sourced from NodeSummary.OutboxBytes/OutboxOldest per the controller
// ruling, with the neutral/amber "nb-behind" accent (never ember).
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

// TestNodeBannerLaggingState pins that "lagging" (NodeSummary.State's other
// behind-but-alive value) renders the same catching-up-shaped sentence and
// accent as "catching up".
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
// (.led.crit / .nb-down) accent -- the ONE state that gets ember, per the
// controller ruling.
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

// TestNodeBannerStaleState pins that "stale" renders the SAME "is down
// since..." text as "down" (per the brief) but WITHOUT the ember accent --
// "ember accent only for down".
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

// TestChildTopbarPillRetryingUnderTwoMinutesStaysHealthy pins the
// threshold: a "retrying" link under 2 minutes still renders the healthy
// pill, not the amber one.
func TestChildTopbarPillRetryingUnderTwoMinutesStaysHealthy(t *testing.T) {
	body := childPillFor(t, &core.LinkView{State: "retrying", LastAck: time.Now().Add(-30 * time.Second).Unix()})
	if strings.Contains(body, "Master unreachable") {
		t.Errorf("a retrying link under the 2m threshold must not render the unreachable pill:\n%s", body)
	}
	if !strings.Contains(body, "Linked to master · ack 30s ago") {
		t.Errorf("child pill missing the healthy fallback text:\n%s", body)
	}
}

// TestChildTopbarPillAbsentWithoutFleetLink pins that a "child"-role daemon
// whose Status() reports no Link at all renders no pill (no panic, no
// empty markup).
func TestChildTopbarPillAbsentWithoutFleetLink(t *testing.T) {
	body := childPillFor(t, nil)
	if strings.Contains(body, "Linked to master") || strings.Contains(body, "Master unreachable") {
		t.Errorf("child pill should be absent when Status().Link is nil:\n%s", body)
	}
}

// ---- solo: no fleet markup leaks at all ---------------------------------

// TestSoloRendersNoFleetMarkup pins the controller ruling directly (solo's
// golden-file idea is dropped in favor of this positive-absence check):
// solo renders no switcher, no banner, no child badge, and no "/n/" link
// anywhere on its dashboard.
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

// TestSoloDeposWithNoFleetRendersNoFleetMarkup covers the more common test
// fixture shape (d.Fleet left nil entirely, as most of this package's
// pre-fleet tests do) -- fleetRole/resolveFleetPageInfo must collapse that
// to "solo" too, with the exact same absence of fleet markup.
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

// countingFleet wraps a core.FleetAPI and counts Status()/Nodes() calls
// separately, so a test can pin the controller ruling: "call
// d.Fleet().Status() at most once per request, and only when the role
// isn't already known from resolveMasterAndNodes" -- and, as of the fleet
// overview's (task 5) round-1 review, the SAME per-request budget for
// Nodes(). Either counter pointer may be left nil by a test that only
// cares about the other one (e.g. the two Status()-only tests below):
// both methods no-op the increment when their pointer is nil rather than
// panicking, so a caller only pays for what it wires up.
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

// TestFleetStatusCalledAtMostOnceOnRemoteNodePage pins the ctx-known-master
// short circuit: a /n/child1/... request's Fleet().Status() call count is
// 0, both because resolveMasterAndNodes (withNodeRouter) already proves
// master via Nodes() alone (masterFleetWithChild's roster has 2 entries)
// and because resolveFleetPageInfo (templates.go) skips its own Status()
// call entirely once it sees the request already came through the node
// router.
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

// TestFleetStatusCalledExactlyOnceOnSelfPage pins the budget's other half:
// an ordinary self-scoped page (never routed through withNodeRouter at
// all, since its path doesn't start with /n/) calls Fleet().Status()
// exactly once, from resolveFleetPageInfo.
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

// ---- Task 4 carry-overs from the Task 3 review --------------------------

// TestChildTopbarPillConnectingWhenNeverAcked pins the controller ruling: a
// child link that has never once acked (LastAck<=0) but isn't (yet)
// "retrying" long enough to count as linkUnreachable must not render the
// confusing "Linked to master · ack never" -- it renders "Connecting to
// master" instead.
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

// TestChildBadgeAbsentWhenFleetStatusErrors pins the Task 3 review
// carry-over: when Fleet().Status() itself errors (a transient
// control-socket hiccup, or an old daemon that doesn't implement it yet),
// resolveFleetPageInfo collapses that to "solo" (see its own doc), so no
// child link pill/badge renders at all -- never one built from a
// zero-valued/stale FleetStatus.
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

// ---- Task 4: <body data-node-prefix> ------------------------------------

// TestBodyDataNodePrefixOnNodeScopedPage pins the controller ruling: a
// master's node-scoped page (/n/{id}/...) renders <body data-node-prefix>
// with that node's URL prefix, so assets/app.js's nodeURL() helper can
// prepend it to the page's own same-origin data fetches (/api/*, /events)
// instead of hitting the master's own data.
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

// TestBodyDataNodePrefixEmptyOnSelfPage pins the other half: every
// self-scoped page (solo, a master's own view, a child's own view) renders
// the attribute present but empty, so nodeURL() is a no-op there.
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

// ---- Task 4: app.js's same-origin data calls go through nodeURL() ------

// appJSNodeScopedLiteral matches a quoted string literal that is one of
// this app's node-scoped data URLs (/api/* or the bare /events -- never
// /public/events, /enroll/*, /login/*, /logout, which are master-local and
// must always target the master regardless of node scope), capturing
// whether it's immediately preceded by "nodeURL(".
//
// Task 4 review carry-over (task-5-brief.md): this regex ONLY catches a
// quoted string literal ('/api/...' or "/api/..."), not a same-origin URL
// built any other way (string concatenation, a template literal, a path
// assembled from a variable) -- it would silently miss a future node-scoped
// fetch written in one of those shapes. It's sufficient for every call site
// in app.js today (see the doc above), but a future addition that builds
// its URL differently needs its own check, not just this scan.
var appJSNodeScopedLiteral = regexp.MustCompile(`(nodeURL\(\s*)?['"](/api/[^'"]*|/events)['"]`)

// TestAppJSDataFetchesGoThroughNodeURL pins the controller ruling: every
// fetch()/EventSource() app.js makes against THIS daemon's own data API
// (/api/series, /api/container/logs, /api/downtime, /events -- whether the
// literal path sits directly in the call or is first assembled into a
// `var url=...` the call later references) must be wrapped in nodeURL(...),
// so a remote node's page (base.html's <body data-node-prefix>) reads that
// node's own data instead of silently falling back to the master's.
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
		if wrapped == "" {
			unwrapped++
			t.Errorf("app.js: %q is a same-origin /api or /events URL not routed through nodeURL(...)", path)
		}
	}
	if unwrapped > 0 {
		t.Errorf("%d unwrapped node-scoped data URL(s) in app.js", unwrapped)
	}
}
