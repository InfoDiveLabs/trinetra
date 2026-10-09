package web

import (
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// errFleetGeneric stands in for an ordinary daemon-side rejection that
// isn't one of core.FleetAPI's typed sentinels (e.g. internal/fleet's
// registry.Update/TokenStore.Delete return plain fmt.Errorf/errors.New
// values for "no such node"/"no such token" -- see
// TestFleetAdminFleetAPIErrorsRenderAsFlashNever500's doc).
var errFleetGeneric = errors.New("fleet: rejected")

// fleetAdminNodeRoster is this file's fixture: self plus one node in every
// state the admin page's rename/tags/revoke/remove/link-health rules care
// about -- web1 online (a normal manageable node), db1 down (removable),
// old1 revoked (removable), web2 lagging with skew/replica-drops set (so
// the link-health table's warning chips have something to show).
func fleetAdminNodeRoster() []core.NodeSummary {
	return []core.NodeSummary{
		{ID: core.SelfNodeID, Name: "self", Self: true, State: "online"},
		{ID: "web1", Name: "web1", State: "online", Tags: []string{"web"}, RemoteAddr: "10.0.0.1:443"},
		{ID: "web2", Name: "web2", State: "lagging", RemoteAddr: "10.0.0.2:443",
			SkewSec: 90, DroppedOutOfOrder: 2, DroppedCardinality: 1, DroppedDuplicate: 1,
			OutboxBytes: 4096, OutboxOldest: 500, OutboxGaps: 1},
		{ID: "db1", Name: "db1", State: "down", RemoteAddr: "10.0.0.3:443"},
		{ID: "old1", Name: "old1", State: "revoked", Revoked: true, RemoteAddr: "10.0.0.4:443"},
	}
}

// fleetAdminDeps builds a fleet-master Deps wired to a caller-supplied
// *fakeFleet (defaulting its Status to config.RoleMaster if unset), so a
// test can inject mutation-error fixtures (fakeFleet's renameErr/tagsErr/
// revokeErr/removeErr/deleteTokenErr/createTokenErr, deps_api_test.go)
// that fleetMasterDeps (handlers_fleet_test.go) has no way to configure.
func fleetAdminDeps(t *testing.T, fleet *fakeFleet) Deps {
	t.Helper()
	if fleet.status.Role == "" {
		fleet.status = core.FleetStatus{Role: config.RoleMaster}
	}
	if fleet.nodes == nil {
		fleet.nodes = fleetAdminNodeRoster()
	}
	return fleetTestDeps(t, masterFakeAPI(fleet, nil))
}

// fleetAdminPost issues an admin-signed-in, CSRF-valid POST against h,
// mirroring handlers_users_test.go's postForm/seedAdmin pair (same
// package, reused directly).
func fleetAdminPost(t *testing.T, d Deps, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)
	return postForm(h, target, form, cookie, csrf)
}

// fleetAdminGetAsRole drives GET /fleet/admin as role, mirroring
// handlers_fleet_test.go's fleetGetAsRole.
func fleetAdminGetAsRole(t *testing.T, d Deps, role Role, target string) *httptest.ResponseRecorder {
	t.Helper()
	return fleetGetAsRole(t, d, role, target)
}

func readAuditFileRaw(t *testing.T, stateDir string) string {
	t.Helper()
	b, err := os.ReadFile(auditLogPath(stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read audit log: %v", err)
	}
	return string(b)
}

// TestFleetAdminPageRendersNodesAndTokens pins the page's basic shape: the
// join-token table's columns, the node-management table (self excluded),
// and the link-health table's CLI-worded warning chips + RemoteAddr
// (admin-only page, so RemoteAddr is shown, unlike the viewer-facing
// /fleet table).
func TestFleetAdminPageRendersNodesAndTokens(t *testing.T) {
	fleet := &fakeFleet{tokens: []core.TokenView{
		{ID: "tok1", Uses: 3, Expires: 4102444800, Tags: []string{"web"}, Creator: "root"},
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminGetAsRole(t, d, RoleAdmin, "/fleet/admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet/admin status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"tok1", "3", "web1", "web2", "db1", "old1",
		"10.0.0.1:443", "10.0.0.2:443", "10.0.0.3:443", "10.0.0.4:443",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /fleet/admin: missing %q\nbody:\n%s", want, body)
		}
	}
	if strings.Contains(body, ">self<") {
		t.Errorf("GET /fleet/admin: self should not appear in the manageable node rows")
	}
	unescaped := html.UnescapeString(body)
	if !strings.Contains(unescaped, "clock differs from this master's by +90s (fix NTP on that host)") {
		t.Errorf("GET /fleet/admin: missing web2's skew warning chip\nbody:\n%s", unescaped)
	}
	if !strings.Contains(unescaped, "replica drops: 2 out of order, 1 over the series limit, 1 duplicates (harmless re-sends)") {
		t.Errorf("GET /fleet/admin: missing web2's replica-drops warning chip\nbody:\n%s", unescaped)
	}
}

// TestFleetAdminSoloDaemon404s pins "master only else 404" for the page
// itself on a non-master daemon.
func TestFleetAdminSoloDaemon404s(t *testing.T) {
	d := fleetSoloDeps(t)
	rr := fleetAdminGetAsRole(t, d, RoleAdmin, "/fleet/admin")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /fleet/admin on solo status = %d, want 404", rr.Code)
	}
}

// TestFleetAdminViewerDenied pins the RBAC floor: a viewer gets 403 on the
// page AND on every mutation route, before any master-role/CSRF/id logic
// ever runs.
func TestFleetAdminViewerDenied(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	if rr := fleetGetAsRole(t, d, RoleViewer, "/fleet/admin"); rr.Code != http.StatusForbidden {
		t.Errorf("GET /fleet/admin as viewer = %d, want 403", rr.Code)
	}

	targets := []string{
		"/fleet/tokens",
		"/fleet/tokens/tok1/delete",
		"/fleet/nodes/web1/rename",
		"/fleet/nodes/web1/tags",
		"/fleet/nodes/web1/revoke",
		"/fleet/nodes/db1/remove",
	}
	for _, target := range targets {
		// A genuine viewer session (seedSignedInRequest, rbac_test.go) --
		// this exercises the SAME requireRole(RoleAdmin,...) gate the route
		// wiring applies, before requireCSRF or any master/self-id check
		// ever runs.
		viewerReq := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodPost, target)
		viewerReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, viewerReq)
		if rr.Code != http.StatusForbidden {
			t.Errorf("POST %s as viewer = %d, want 403", target, rr.Code)
		}
	}
}

// TestFleetAdminMissingCSRFForbidden pins requireCSRF on every mutation
// route: an admin session with no CSRF token gets 403.
func TestFleetAdminMissingCSRFForbidden(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)

	targets := []string{
		"/fleet/tokens",
		"/fleet/tokens/tok1/delete",
		"/fleet/nodes/web1/rename",
		"/fleet/nodes/web1/tags",
		"/fleet/nodes/web1/revoke",
		"/fleet/nodes/db1/remove",
	}
	for _, target := range targets {
		rr := postForm(h, target, url.Values{}, cookie, "")
		if rr.Code != http.StatusForbidden {
			t.Errorf("POST %s with no CSRF token = %d, want 403", target, rr.Code)
		}
	}
}

// TestFleetAdminSelfIDRejected pins that the "self" id is rejected with 400 on
// rename/tags/revoke/remove.
func TestFleetAdminSelfIDRejected(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	for _, tc := range []struct {
		target string
		form   url.Values
	}{
		{"/fleet/nodes/self/rename", url.Values{"name": {"new-name"}}},
		{"/fleet/nodes/self/tags", url.Values{"tags": {"x"}}},
		{"/fleet/nodes/self/revoke", url.Values{}},
		{"/fleet/nodes/self/remove", url.Values{}},
	} {
		rr := fleetAdminPost(t, d, tc.target, tc.form)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("POST %s (self) status = %d, want 400, body: %s", tc.target, rr.Code, rr.Body.String())
		}
	}
}

// TestFleetAdminTokenCreate pins the create-token flow end to end: the response
// renders the page directly (200, not a redirect) with the exact `sudo trinetra
// fleet join <code>` command and a "shown once" note, Cache-Control: no-store
// is set, Fleet().CreateToken received Creator == the acting admin's name, an
// audit record was written with the right Action, and -- the security
// requirement -- the join code itself never appears anywhere in the audit log.
func TestFleetAdminTokenCreate(t *testing.T) {
	fleet := &fakeFleet{createdToken: core.CreatedToken{
		Token:    core.TokenView{ID: "tok-new", Uses: 5, Expires: 4102444800, Tags: []string{"web", "prod"}, Creator: "root"},
		JoinCode: "swj1_deadbeefCAFEjoincodeexample",
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/tokens", url.Values{"ttl": {"2h"}, "uses": {"5"}, "tags": {"web, prod"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/tokens status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("POST /fleet/tokens Cache-Control = %q, want %q", got, "no-store")
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sudo trinetra fleet join swj1_deadbeefCAFEjoincodeexample") {
		t.Errorf("POST /fleet/tokens: missing join command in body:\n%s", body)
	}
	if !strings.Contains(strings.ToLower(body), "shown once") {
		t.Errorf("POST /fleet/tokens: missing \"shown once\" note in body:\n%s", body)
	}

	recs := readAuditRecords(t, d.StateDir)
	found := false
	for _, r := range recs {
		if r.Action == "fleet.token.create" {
			found = true
			if r.Key != "tok-new" {
				t.Errorf("audit fleet.token.create Key = %q, want %q", r.Key, "tok-new")
			}
			if strings.Contains(r.New, "swj1_") || strings.Contains(r.Old, "swj1_") {
				t.Errorf("audit fleet.token.create record carries the join code: %+v", r)
			}
		}
	}
	if !found {
		t.Errorf("no fleet.token.create audit record found: %+v", recs)
	}

	raw := readAuditFileRaw(t, d.StateDir)
	if strings.Contains(raw, "swj1_") {
		t.Errorf("audit log file contains the join code prefix \"swj1_\":\n%s", raw)
	}
}

// TestFleetAdminTokenCreateSetsCreator asserts Fleet().CreateToken is called
// with Creator set to the acting admin's own user name.
func TestFleetAdminTokenCreateSetsCreator(t *testing.T) {
	fleet := &fakeFleet{createdToken: core.CreatedToken{Token: core.TokenView{ID: "tok-1"}, JoinCode: "swj1_x"}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/tokens", url.Values{})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	// seedAdmin (used by fleetAdminPost) always names the admin "root"; the
	// audit New field is "ttl=... uses=... tags=... creator=<name>".
	recs := readAuditRecords(t, d.StateDir)
	var newField string
	for _, r := range recs {
		if r.Action == "fleet.token.create" {
			newField = r.New
		}
	}
	if !strings.Contains(newField, "creator=root") {
		t.Errorf("fleet.token.create audit New = %q, want it to contain creator=root", newField)
	}
}

// TestNewTokenRowExpiresInMasterLocalZone pins that a join-token's Expires must
// render through the same master-local-zone- with-abbreviation convention
// (silenceTimeText) as every other absolute timestamp on the fleet surface,
// rather than its own RFC3339/UTC convention -- a third distinct format the
// review flagged as worth folding into the same cleanup as finding I1.
func TestNewTokenRowExpiresInMasterLocalZone(t *testing.T) {
	withLocalTZ(t, "Asia/Kolkata")
	row := newTokenRow(core.TokenView{ID: "tok1", Expires: 1893456000})
	want := silenceTimeText(1893456000)
	if row.Expires != want {
		t.Errorf("newTokenRow Expires = %q, want %q (silenceTimeText's own output)", row.Expires, want)
	}
	if !strings.Contains(row.Expires, "IST") {
		t.Errorf("newTokenRow Expires missing IST zone abbreviation, got %q", row.Expires)
	}
}

// TestFleetAdminTokenCreateValidation pins TTL/uses/tags bounds, and that
// errors render inline with the form input kept.
func TestFleetAdminTokenCreateValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"ttl too short", url.Values{"ttl": {"4m"}}, "4m"},
		{"ttl too long", url.Values{"ttl": {"721h"}}, "721h"},
		{"ttl not a duration", url.Values{"ttl": {"soon"}}, "soon"},
		{"uses zero", url.Values{"uses": {"0"}}, "0"},
		{"uses too many", url.Values{"uses": {"101"}}, "101"},
		{"uses not a number", url.Values{"uses": {"many"}}, "many"},
		{"tag has uppercase", url.Values{"tags": {"Web"}}, "Web"},
		{"tag has a dot", url.Values{"tags": {"web.prod"}}, "web.prod"},
		{"too many tags", url.Values{"tags": {"a,b,c,d,e,f,g,h,i,j,k"}}, "a,b,c,d,e,f,g,h,i,j,k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := fleetAdminDeps(t, &fakeFleet{})
			rr := fleetAdminPost(t, d, "/fleet/tokens", tc.form)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.want) {
				t.Errorf("body does not keep the submitted input %q:\n%s", tc.want, rr.Body.String())
			}
			recs := readAuditRecords(t, d.StateDir)
			for _, r := range recs {
				if r.Action == "fleet.token.create" {
					t.Errorf("a validation failure must not write an audit record: %+v", r)
				}
			}
		})
	}
}

// TestFleetAdminTokenDelete pins delete + its audit record.
func TestFleetAdminTokenDelete(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/tokens/tok1/delete", url.Values{})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.deletedTokens) != 1 || fleet.deletedTokens[0] != "tok1" {
		t.Errorf("deletedTokens = %v, want [tok1]", fleet.deletedTokens)
	}
	recs := readAuditRecords(t, d.StateDir)
	var found bool
	for _, r := range recs {
		if r.Action == "fleet.token.delete" && r.Key == "tok1" {
			found = true
		}
	}
	if !found {
		t.Errorf("no fleet.token.delete audit record for tok1: %+v", recs)
	}
}

// TestFleetAdminNodeRename pins rename success + its audit record's old/new
// values.
func TestFleetAdminNodeRename(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/nodes/web1/rename", url.Values{"name": {"web-1"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.renamed["web1"] != "web-1" {
		t.Errorf("renamed[web1] = %q, want web-1", fleet.renamed["web1"])
	}
	// Actor plumbing: a web rename must pass the SIGNED-IN user's own name
	// (auditUser(r), seedAdmin's "root" here) as the actor, never the literal
	// placeholder "unknown" on the FleetAPI's own audit log.
	if fleet.renamedActor != "root" {
		t.Errorf("RenameNode actor = %q, want the signed-in admin's name (root), not a placeholder", fleet.renamedActor)
	}
	recs := readAuditRecords(t, d.StateDir)
	var got *AuditRecord
	for i, r := range recs {
		if r.Action == "fleet.node.rename" {
			got = &recs[i]
		}
	}
	if got == nil {
		t.Fatalf("no fleet.node.rename audit record: %+v", recs)
	}
	if got.Key != "web1" || got.Old != "web1" || got.New != "web-1" {
		t.Errorf("fleet.node.rename audit record = %+v, want Key=web1 Old=web1 New=web-1", got)
	}
}

// TestFleetAdminNodeRenameValidation pins the rename bounds (1..64 chars,
// no control characters) and that the rejected input is kept in the form.
func TestFleetAdminNodeRenameValidation(t *testing.T) {
	tooLong := strings.Repeat("x", 65)
	for _, tc := range []struct {
		name string
		val  string
	}{
		{"empty", ""},
		{"too long", tooLong},
		{"control character", "bad\x00name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := fleetAdminDeps(t, &fakeFleet{})
			rr := fleetAdminPost(t, d, "/fleet/nodes/web1/rename", url.Values{"name": {tc.val}})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestFleetAdminNodeTags pins tags-update success + its audit record.
func TestFleetAdminNodeTags(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/nodes/web1/tags", url.Values{"tags": {"web, prod"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if got := fleet.tagged["web1"]; len(got) != 2 || got[0] != "web" || got[1] != "prod" {
		t.Errorf("tagged[web1] = %v, want [web prod]", got)
	}
	recs := readAuditRecords(t, d.StateDir)
	var found bool
	for _, r := range recs {
		if r.Action == "fleet.node.tags" && r.Key == "web1" {
			found = true
			if r.New != "web,prod" {
				t.Errorf("fleet.node.tags New = %q, want web,prod", r.New)
			}
		}
	}
	if !found {
		t.Errorf("no fleet.node.tags audit record: %+v", recs)
	}
}

// TestFleetAdminNodeTagsValidation pins the tag shape/count bounds.
func TestFleetAdminNodeTagsValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  string
	}{
		{"uppercase", "Web"},
		{"dot not allowed", "web.prod"},
		{"too many", "a,b,c,d,e,f,g,h,i,j,k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := fleetAdminDeps(t, &fakeFleet{})
			rr := fleetAdminPost(t, d, "/fleet/nodes/web1/tags", url.Values{"tags": {tc.val}})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestFleetAdminNodeRevoke pins revoke success + its audit record. The
// two-step confirm is UI-only (style.css's checkbox toggle); the endpoint
// itself revokes on any valid POST that reaches it.
func TestFleetAdminNodeRevoke(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/nodes/web1/revoke", url.Values{})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.revoked) != 1 || fleet.revoked[0] != "web1" {
		t.Errorf("revoked = %v, want [web1]", fleet.revoked)
	}
	recs := readAuditRecords(t, d.StateDir)
	var found bool
	for _, r := range recs {
		if r.Action == "fleet.node.revoke" && r.Key == "web1" {
			found = true
		}
	}
	if !found {
		t.Errorf("no fleet.node.revoke audit record: %+v", recs)
	}
}

// TestFleetAdminNodeRemoveOnlyDownOrRevoked pins that only revoked or down
// nodes may be removed: an online node is refused (400, no call to
// RemoveNode), a down node succeeds.
func TestFleetAdminNodeRemoveOnlyDownOrRevoked(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)

	rr := fleetAdminPost(t, d, "/fleet/nodes/web1/remove", url.Values{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("remove online node status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.removed) != 0 {
		t.Errorf("RemoveNode should not have been called for an online node, got %v", fleet.removed)
	}

	rr = fleetAdminPost(t, d, "/fleet/nodes/db1/remove", url.Values{})
	if rr.Code != http.StatusOK {
		t.Fatalf("remove down node status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.removed) != 1 || fleet.removed[0] != "db1" {
		t.Errorf("removed = %v, want [db1]", fleet.removed)
	}

	recs := readAuditRecords(t, d.StateDir)
	var found bool
	for _, r := range recs {
		if r.Action == "fleet.node.remove" && r.Key == "db1" {
			found = true
		}
	}
	if !found {
		t.Errorf("no fleet.node.remove audit record for db1: %+v", recs)
	}
}

// TestFleetAdminNodeRemoveRevokedAllowed pins that a revoked node (not just
// a down one) is also removable.
func TestFleetAdminNodeRemoveRevokedAllowed(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/nodes/old1/remove", url.Values{})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.removed) != 1 || fleet.removed[0] != "old1" {
		t.Errorf("removed = %v, want [old1]", fleet.removed)
	}
}

// TestFleetAdminFleetAPIErrorsRenderAsFlashNever500 pins that any FleetAPI
// error (ErrNoSuchNode, ErrNotMaster, or a generic daemon-side rejection)
// renders as a flash message with a 4xx status, never a 500 -- across every
// mutation route.
func TestFleetAdminFleetAPIErrorsRenderAsFlashNever500(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(f *fakeFleet)
		target   string
		form     url.Values
		wantCode int
	}{
		{"rename: no such node", func(f *fakeFleet) { f.renameErr = core.ErrNoSuchNode }, "/fleet/nodes/web1/rename", url.Values{"name": {"x"}}, http.StatusNotFound},
		{"tags: no such node", func(f *fakeFleet) { f.tagsErr = core.ErrNoSuchNode }, "/fleet/nodes/web1/tags", url.Values{"tags": {"x"}}, http.StatusNotFound},
		{"revoke: not master", func(f *fakeFleet) { f.revokeErr = core.ErrNotMaster }, "/fleet/nodes/web1/revoke", url.Values{}, http.StatusNotFound},
		{"remove: generic daemon error", func(f *fakeFleet) { f.removeErr = errFleetGeneric }, "/fleet/nodes/db1/remove", url.Values{}, http.StatusBadRequest},
		{"token delete: no such token", func(f *fakeFleet) { f.deleteTokenErr = errFleetGeneric }, "/fleet/tokens/nope/delete", url.Values{}, http.StatusBadRequest},
		{"token create: daemon rejects", func(f *fakeFleet) { f.createTokenErr = errFleetGeneric }, "/fleet/tokens", url.Values{}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fleet := &fakeFleet{}
			tc.mutate(fleet)
			d := fleetAdminDeps(t, fleet)
			rr := fleetAdminPost(t, d, tc.target, tc.form)
			if rr.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d, body: %s", rr.Code, tc.wantCode, rr.Body.String())
			}
			if rr.Code >= 500 {
				t.Fatalf("must never return 5xx, got %d", rr.Code)
			}
			if !strings.Contains(rr.Body.String(), `class="flash err"`) {
				t.Errorf("body missing the error flash markup:\n%s", rr.Body.String())
			}
		})
	}
}

func TestValidateNodeNameRejectsFormattingCharacters(t *testing.T) {
	for _, bad := range []string{"web‮1", "db​1", "a\x07b"} {
		if _, err := validateNodeName(bad); err == nil {
			t.Errorf("validateNodeName(%q) accepted a control/formatting character", bad)
		}
	}
	if got, err := validateNodeName("  web-1 ünïcode "); err != nil || got != "web-1 ünïcode" {
		t.Errorf("validateNodeName ordinary name = %q, %v", got, err)
	}
}
