package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// ---- RBAC / CSRF / gating -------------------------------------------------

// TestFleetManagedSoloDaemon404s pins "master only, else 404" for the page.
func TestFleetManagedSoloDaemon404s(t *testing.T) {
	d := fleetSoloDeps(t)
	rr := fleetGetAsViewer(t, d, "/fleet/managed")
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /fleet/managed on solo status = %d, want 404", rr.Code)
	}
}

// TestFleetManagedAnonymousRedirectsToLogin pins the anonymous case: no
// session at all gets a 302 to the login page, never a 403/404.
func TestFleetManagedAnonymousRedirectsToLogin(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	rr := fleetGetAnonymous(d, "/fleet/managed")
	if rr.Code != http.StatusFound {
		t.Errorf("GET /fleet/managed anonymous status = %d, want 302", rr.Code)
	}
}

// TestFleetManagedViewerReadOnly pins that a viewer sees the page (200), the
// fragments list and status table, but no create/edit/delete controls at all.
func TestFleetManagedViewerReadOnly(t *testing.T) {
	fleet := &fakeFleet{managed: []core.ManagedFragment{
		{ID: "f1", Tag: "web", Values: map[string]string{"thresholds.cpu_pct": "90"}, Version: 2, Author: "root"},
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/managed")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet/managed as viewer status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "thresholds.cpu_pct=90") {
		t.Errorf("viewer must still see fragment values, body:\n%s", body)
	}
	if strings.Contains(body, "Create fragment") || strings.Contains(body, "New fragment") {
		t.Error("viewer must not see the create-fragment form")
	}
	if strings.Contains(body, "Confirm delete") {
		t.Error("viewer must not see the delete control")
	}
	if strings.Contains(body, ">Edit<") {
		t.Error("viewer must not see the edit link")
	}
}

// TestFleetManagedViewerDenied403OnPost pins the RBAC floor: a viewer gets
// 403 on every mutation route, before any master-role/CSRF/id logic runs.
func TestFleetManagedViewerDenied403OnPost(t *testing.T) {
	fleet := &fakeFleet{managed: []core.ManagedFragment{{ID: "f1", Tag: "web"}}}
	d := fleetAdminDeps(t, fleet)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	for _, target := range []string{
		"/fleet/managed",
		"/fleet/managed/f1/delete",
	} {
		req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodPost, target)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("POST %s as viewer = %d, want 403", target, rr.Code)
		}
	}
}

// TestFleetManagedMissingCSRFForbidden pins requireCSRF on every mutation
// route: an admin session with no CSRF token gets 403.
func TestFleetManagedMissingCSRFForbidden(t *testing.T) {
	fleet := &fakeFleet{managed: []core.ManagedFragment{{ID: "f1", Tag: "web"}}}
	d := fleetAdminDeps(t, fleet)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)

	for _, target := range []string{
		"/fleet/managed",
		"/fleet/managed/f1/delete",
	} {
		rr := postForm(h, target, url.Values{}, cookie, "")
		if rr.Code != http.StatusForbidden {
			t.Errorf("POST %s with no CSRF token = %d, want 403", target, rr.Code)
		}
	}
}

// ---- key select allowlist --------------------------------------------------

// TestFleetManagedKeySelectOffersOnlyAllowlistedKeys pins that the key <select>
// offers exactly core.ManagedKeys, nothing more/less.
func TestFleetManagedKeySelectOffersOnlyAllowlistedKeys(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	body, _, _ := fleetAdminSessionGet(t, d, "/fleet/managed")
	for _, k := range core.ManagedKeys {
		if !strings.Contains(body, `value="`+k+`"`) {
			t.Errorf("key select missing allowlisted key %q, body:\n%s", k, body)
		}
	}
	// Sanity: something OUTSIDE the allowlist must not appear as a select option value.
	if strings.Contains(body, `value="server.name"`) {
		t.Error("key select must not offer a non-allowlisted key")
	}
}

// TestFleetManagedNonAllowlistedKeyPostedDirectlyRejectedInline pins the backend rejection:
// a key outside the allowlist, posted directly.
func TestFleetManagedNonAllowlistedKeyPostedDirectlyRejectedInline(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/managed")

	values := formValuesForButton(t, body, "op", "save")
	values.Set("tag", "web")
	values.Set("row_0_key", "server.name") // not in core.ManagedKeys
	values.Set("row_0_value", "evil")
	rr := postForm(h, "/fleet/managed", values, cookie, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /fleet/managed with a non-allowlisted key status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.managed) != 0 {
		t.Errorf("SaveManaged must not have persisted a fragment, got %d", len(fleet.managed))
	}
	got := rr.Body.String()
	if !strings.Contains(got, "is not a managed-config key") {
		t.Errorf("missing inline non-allowlisted-key rejection, body:\n%s", got)
	}
	if !strings.Contains(got, `value="server.name"`) {
		t.Errorf("posted key not preserved on the rejected row, body:\n%s", got)
	}
}

// ---- create / upsert -------------------------------------------------------

// TestFleetManagedCreateRoundTripsWithSessionUserAsAuthor drives the real rendered form
// (formValuesForButton), never a hand-built payload.
func TestFleetManagedCreateRoundTripsWithSessionUserAsAuthor(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/managed")

	values := formValuesForButton(t, body, "op", "save")
	values.Set("tag", "web")
	values.Set("row_0_key", "thresholds.cpu_pct")
	values.Set("row_0_value", "88")
	rr := postForm(h, "/fleet/managed", values, cookie, "")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST /fleet/managed status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/fleet/managed?flash=fragment_saved" {
		t.Errorf("redirect Location = %q, want .../fleet/managed?flash=fragment_saved", loc)
	}
	if len(fleet.managed) != 1 {
		t.Fatalf("fragments saved = %d, want 1", len(fleet.managed))
	}
	f := fleet.managed[0]
	if f.Author != "root" {
		t.Errorf("Fragment Author = %q, want the signed-in admin's name (root)", f.Author)
	}
	if fleet.savedManagedActor != "root" {
		t.Errorf("SaveManaged actor = %q, want root", fleet.savedManagedActor)
	}
	if f.Tag != "web" || f.Values["thresholds.cpu_pct"] != "88" {
		t.Errorf("Fragment = %+v, want tag=web thresholds.cpu_pct=88", f)
	}
}

// TestFleetManagedSaveUpsertsByTag pins that saving a fresh draft.
func TestFleetManagedSaveUpsertsByTag(t *testing.T) {
	fleet := &fakeFleet{managed: []core.ManagedFragment{
		{ID: "f1", Tag: "web", Values: map[string]string{"thresholds.cpu_pct": "80"}, Version: 1, Author: "root"},
	}}
	d := fleetAdminDeps(t, fleet)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	values := url.Values{
		"csrf_token":  {csrf},
		"id":          {""},
		"tag":         {"web"},
		"row_count":   {"1"},
		"row_0_key":   {"thresholds.mem_pct"},
		"row_0_value": {"70"},
		"op":          {"save"},
	}
	rr := postForm(h, "/fleet/managed", values, cookie, csrf)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST /fleet/managed status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.managed) != 1 {
		t.Fatalf("fragments = %d, want 1 (upsert by tag, not a new fragment)", len(fleet.managed))
	}
	if fleet.managed[0].ID != "f1" {
		t.Errorf("upserted fragment id = %q, want the EXISTING fragment's id f1", fleet.managed[0].ID)
	}
	if fleet.managed[0].Values["thresholds.mem_pct"] != "70" {
		t.Errorf("upserted fragment values = %+v, want thresholds.mem_pct=70", fleet.managed[0].Values)
	}
}

// TestFleetManagedSaveAuditDescribesChangeNotActor pins the C5 review carry-over: the save
// mutation's audit record New field describes the fragment's tag/keys, not the acting user.
func TestFleetManagedSaveAuditDescribesChangeNotActor(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	values := url.Values{
		"csrf_token":  {csrf},
		"tag":         {"web"},
		"row_count":   {"1"},
		"row_0_key":   {"thresholds.cpu_pct"},
		"row_0_value": {"88"},
		"op":          {"save"},
	}
	rr := postForm(h, "/fleet/managed", values, cookie, csrf)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	recs := readAuditRecords(t, d.StateDir)
	var found *AuditRecord
	for i := range recs {
		if recs[i].Action == "fleet.managed.save" {
			found = &recs[i]
		}
	}
	if found == nil {
		t.Fatalf("no fleet.managed.save audit record, got: %+v", recs)
	}
	if found.User != "root" {
		t.Errorf("audit User = %q, want root", found.User)
	}
	if strings.Contains(found.New, "root") {
		t.Errorf("audit New = %q, must describe the change, not repeat the actor", found.New)
	}
	if !strings.Contains(found.New, "tag=web") || !strings.Contains(found.New, "thresholds.cpu_pct=88") {
		t.Errorf("audit New = %q, want it to describe tag=web and thresholds.cpu_pct=88", found.New)
	}
}

// TestFleetManagedDeleteAuditDescribesChangeNotActor is
// TestFleetManagedSaveAuditDescribesChangeNotActor's delete counterpart.
func TestFleetManagedDeleteAuditDescribesChangeNotActor(t *testing.T) {
	fleet := &fakeFleet{managed: []core.ManagedFragment{
		{ID: "f1", Tag: "web", Values: map[string]string{"thresholds.cpu_pct": "80"}, Version: 1, Author: "root"},
	}}
	d := fleetAdminDeps(t, fleet)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/fleet/managed/f1/delete", nil, cookie, csrf)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	recs := readAuditRecords(t, d.StateDir)
	var found *AuditRecord
	for i := range recs {
		if recs[i].Action == "fleet.managed.delete" {
			found = &recs[i]
		}
	}
	if found == nil {
		t.Fatalf("no fleet.managed.delete audit record, got: %+v", recs)
	}
	if strings.Contains(found.New, "root") {
		t.Errorf("audit New = %q, must describe the change, not repeat the actor", found.New)
	}
	if !strings.Contains(found.New, "tag=web") || !strings.Contains(found.New, "thresholds.cpu_pct=80") {
		t.Errorf("audit New = %q, want it to describe the deleted fragment's tag=web and thresholds.cpu_pct=80", found.New)
	}
}

// ---- row add/remove field survival -----------------------------------------

// TestFleetManagedAddRowRoundTrip pins the add/remove-row draft pattern itself: clicking
// "Add key" must reshape the draft.
func TestFleetManagedAddRowRoundTrip(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/managed")

	values := formValuesForButton(t, body, "op", "add_row")
	rr := postForm(h, "/fleet/managed", values, cookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/managed op=add_row status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.managed) != 0 {
		t.Errorf("add_row must never call SaveManaged, got %d fragments", len(fleet.managed))
	}
	if strings.Count(rr.Body.String(), `name="row_0_key"`) != 1 || !strings.Contains(rr.Body.String(), `name="row_1_key"`) {
		t.Errorf("add_row did not add a second row, body:\n%s", rr.Body.String())
	}
}

// TestFleetManagedAddThenRemoveRowPreservesOtherFields is the browser- faithful
// field-survival test: fill Tag + the first row's key/value, click "Add key".
func TestFleetManagedAddThenRemoveRowPreservesOtherFields(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/managed")

	values := formValuesForButton(t, body, "op", "add_row")
	values.Set("tag", "web")
	values.Set("row_0_key", "thresholds.cpu_pct")
	values.Set("row_0_value", "85")
	rr := postForm(h, "/fleet/managed", values, cookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("add_row status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body2 := rr.Body.String()
	if !strings.Contains(body2, `value="web"`) || !strings.Contains(body2, `value="85"`) {
		t.Fatalf("fields not preserved after add_row, body:\n%s", body2)
	}

	values2 := formValuesForButton(t, body2, "op", "remove_row:1")
	rr2 := postForm(h, "/fleet/managed", values2, cookie, "")
	if rr2.Code != http.StatusOK {
		t.Fatalf("remove_row status = %d, want 200, body: %s", rr2.Code, rr2.Body.String())
	}
	got := rr2.Body.String()
	for _, want := range []string{`value="web"`, `name="row_0_key"`, `value="85"`} {
		if !strings.Contains(got, want) {
			t.Errorf("field not preserved after remove_row: missing %q\nbody:\n%s", want, got)
		}
	}
	if strings.Contains(got, `name="row_1_key"`) {
		t.Errorf("remove_row did not actually remove the second row, body:\n%s", got)
	}
	if len(fleet.managed) != 0 {
		t.Errorf("add_row/remove_row must never call SaveManaged, got %d fragments", len(fleet.managed))
	}
}

// ---- edit an existing fragment ---------------------------------------------

// TestFleetManagedEditLoadsExistingFragment pins the ?edit=<id> load: the form is preloaded
// with that fragment's tag/keys/values plus a hidden id.
func TestFleetManagedEditLoadsExistingFragment(t *testing.T) {
	fleet := &fakeFleet{managed: []core.ManagedFragment{
		{ID: "f1", Tag: "db", Values: map[string]string{"thresholds.mem_pct": "75"}, Version: 3, Author: "root"},
	}}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/managed?edit=f1")
	if !strings.Contains(body, `value="db"`) || !strings.Contains(body, `value="75"`) || !strings.Contains(body, `value="f1"`) {
		t.Fatalf("edit form not preloaded from the existing fragment, body:\n%s", body)
	}

	values := formValuesForButton(t, body, "op", "save")
	values.Set("row_0_value", "80")
	rr := postForm(h, "/fleet/managed", values, cookie, "")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST /fleet/managed status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.managed) != 1 || fleet.managed[0].ID != "f1" {
		t.Fatalf("expected the SAME fragment f1 updated in place, got %+v", fleet.managed)
	}
	if fleet.managed[0].Values["thresholds.mem_pct"] != "80" {
		t.Errorf("updated value = %+v, want thresholds.mem_pct=80", fleet.managed[0].Values)
	}
}

// ---- status table -----------------------------------------------------------

// TestFleetManagedStatusRendersDriftAndConflicts pins that the per-node
// status table shows drift keys as badges and lists conflicts.
func TestFleetManagedStatusRendersDriftAndConflicts(t *testing.T) {
	fleet := &fakeFleet{managedStatus: []core.ManagedStatus{
		{
			Node: "web1", Version: 2, Desired: 3, Applied: false,
			Error: "push failed: connection reset",
			Drift: []string{"thresholds.cpu_pct", "quiet_hours"},
			Conflicts: []core.ManagedConflict{
				{Key: "thresholds.cpu_pct", Fragments: []string{"f1", "f2"}},
			},
		},
		{Node: "db1", Version: 5, Desired: 5, Applied: true},
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/managed")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "web1") || !strings.Contains(body, "2 / 3") {
		t.Errorf("missing web1's applied/desired version, body:\n%s", body)
	}
	if !strings.Contains(body, "push failed: connection reset") {
		t.Errorf("missing web1's error, body:\n%s", body)
	}
	if !strings.Contains(body, `badge warn">thresholds.cpu_pct<`) || !strings.Contains(body, `badge warn">quiet_hours<`) {
		t.Errorf("drift keys not rendered as warn badges, body:\n%s", body)
	}
	if !strings.Contains(body, "thresholds.cpu_pct (f1, f2)") {
		t.Errorf("missing conflict listing, body:\n%s", body)
	}
	if !strings.Contains(body, "db1") || !strings.Contains(body, "5 / 5") {
		t.Errorf("missing db1's fully-applied row, body:\n%s", body)
	}
}

// TestFleetManagedStatusShowsNodeNameNotID pins U2 (2026-09-25 UI audit): the per-node
// status table's Node column must show the roster's display name.
func TestFleetManagedStatusShowsNodeNameNotID(t *testing.T) {
	const rawID = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"
	fleet := &fakeFleet{
		nodes: []core.NodeSummary{
			{ID: core.SelfNodeID, Name: "self", Self: true, State: "online"},
			{ID: rawID, Name: "api-01", State: "online"},
		},
		managedStatus: []core.ManagedStatus{{Node: rawID, Version: 1, Desired: 1, Applied: true}},
	}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/managed")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "api-01") {
		t.Errorf("missing node display name api-01, body:\n%s", body)
	}
	if !strings.Contains(body, `title="`+rawID+`"`) {
		t.Errorf("missing raw id as a title/tooltip, body:\n%s", body)
	}
	start := strings.Index(body, "Per-node status")
	if start < 0 {
		t.Fatalf("no Per-node status section, body:\n%s", body)
	}
	if strings.Contains(body[start:], ">"+rawID+"<") {
		t.Errorf("Node column still shows the raw id as visible text, body:\n%s", body[start:])
	}
}
