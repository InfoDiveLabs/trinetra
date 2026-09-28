package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// withLocalTZ temporarily replaces time.Local with the named IANA zone for
// the duration of the calling test, restoring the original afterward
// (t.Cleanup) -- fix round 1 review's own ruling ("pin time.Local in the
// test via a helper that swaps and restores it"), so a test asserting the
// silence page's zone-labeled time rendering (silenceTimeText/
// silenceTimeZoneNote, handlers_fleet_silences.go) gets a deterministic
// zone/abbreviation instead of depending on whatever zone the test machine
// happens to run in.
func withLocalTZ(t *testing.T, name string) {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("time.LoadLocation(%q): %v", name, err)
	}
	orig := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = orig })
}

// ---- RBAC / CSRF / gating -------------------------------------------------

// TestFleetSilencesSoloDaemon404s pins "master only, else 404" for the page.
func TestFleetSilencesSoloDaemon404s(t *testing.T) {
	d := fleetSoloDeps(t)
	rr := fleetGetAsViewer(t, d, "/fleet/silences")
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /fleet/silences on solo status = %d, want 404", rr.Code)
	}
}

// TestFleetSilencesAnonymousRedirectsToLogin pins the anonymous case: no
// session at all gets a 302 to the login page, never a 403/404.
func TestFleetSilencesAnonymousRedirectsToLogin(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	rr := fleetGetAnonymous(d, "/fleet/silences")
	if rr.Code != http.StatusFound {
		t.Errorf("GET /fleet/silences anonymous status = %d, want 302", rr.Code)
	}
}

// TestFleetSilencesViewerReadOnly pins the ruling: a viewer sees the page
// (200) but no create/expire/delete controls at all.
func TestFleetSilencesViewerReadOnly(t *testing.T) {
	fleet := &fakeFleet{silences: []core.Silence{
		{ID: "sil1", Matchers: []core.Matcher{{Tag: "web"}}, Start: 1000, End: 9999999999, Author: "root"},
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/silences")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet/silences as viewer status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "Create silence") {
		t.Error("viewer must not see the create-silence form")
	}
	if strings.Contains(body, "Create maintenance window") {
		t.Error("viewer must not see the create-maintenance form")
	}
	if strings.Contains(body, "Confirm expire") {
		t.Error("viewer must not see the expire control")
	}
}

// TestFleetSilencesViewerDenied403OnPost pins the RBAC floor: a viewer gets
// 403 on every mutation route, before any master-role/CSRF/id logic runs.
func TestFleetSilencesViewerDenied403OnPost(t *testing.T) {
	fleet := &fakeFleet{
		silences:     []core.Silence{{ID: "sil1", Matchers: []core.Matcher{{Tag: "web"}}, Start: 1000, End: 9999999999}},
		maintenances: []core.Maintenance{{ID: "mnt1", Name: "nightly", Matchers: []core.Matcher{{Tag: "web"}}, Weekdays: []int{1}, From: "22:00", To: "23:00", TZ: "UTC"}},
	}
	d := fleetAdminDeps(t, fleet)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	for _, target := range []string{
		"/fleet/silences",
		"/fleet/silences/sil1/expire",
		"/fleet/maintenance",
		"/fleet/maintenance/mnt1/delete",
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

// TestFleetSilencesMissingCSRFForbidden pins requireCSRF on every mutation
// route: an admin session with no CSRF token gets 403.
func TestFleetSilencesMissingCSRFForbidden(t *testing.T) {
	fleet := &fakeFleet{
		silences:     []core.Silence{{ID: "sil1", Matchers: []core.Matcher{{Tag: "web"}}, Start: 1000, End: 9999999999}},
		maintenances: []core.Maintenance{{ID: "mnt1", Name: "nightly", Matchers: []core.Matcher{{Tag: "web"}}, Weekdays: []int{1}, From: "22:00", To: "23:00", TZ: "UTC"}},
	}
	d := fleetAdminDeps(t, fleet)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)

	for _, target := range []string{
		"/fleet/silences",
		"/fleet/silences/sil1/expire",
		"/fleet/maintenance",
		"/fleet/maintenance/mnt1/delete",
	} {
		rr := postForm(h, target, url.Values{}, cookie, "")
		if rr.Code != http.StatusForbidden {
			t.Errorf("POST %s with no CSRF token = %d, want 403", target, rr.Code)
		}
	}
}

// ---- create silence --------------------------------------------------------

// TestFleetSilencesCreateRoundTripsWithSessionUserAsAuthor drives the real
// rendered form (formValuesForButton, formhelpers_test.go), never a
// hand-built payload, and pins that the created silence's Author is the
// SIGNED-IN web user, not a placeholder -- landing on the Active tab since
// the default Start (left blank, meaning "now") isn't in the future.
func TestFleetSilencesCreateRoundTripsWithSessionUserAsAuthor(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/silences")

	values := formValuesForButton(t, body, "op", "save")
	values.Set("matcher_0_tag", "web")
	values.Set("comment", "planned work")
	rr := postForm(h, "/fleet/silences", values, cookie, "")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST /fleet/silences status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/fleet/silences?flash=silence_created&tab=active" {
		t.Errorf("redirect Location = %q, want .../fleet/silences?flash=silence_created&tab=active", loc)
	}
	if len(fleet.silences) != 1 {
		t.Fatalf("silences created = %d, want 1", len(fleet.silences))
	}
	s := fleet.silences[0]
	if s.Author != "root" {
		t.Errorf("Silence Author = %q, want the signed-in admin's name (root)", s.Author)
	}
	if s.Comment != "planned work" {
		t.Errorf("Silence Comment = %q, want %q", s.Comment, "planned work")
	}
	if len(s.Matchers) != 1 || s.Matchers[0].Tag != "web" {
		t.Errorf("Silence Matchers = %+v, want one matcher tag=web", s.Matchers)
	}
	if s.End-s.Start != 1800 {
		t.Errorf("Silence window = %ds, want 1800s (the default 30m preset)", s.End-s.Start)
	}
}

// TestFleetSilencesDisplayedStartMatchesTypedWallClockPlusZone pins fix
// round 1 review's IMPORTANT finding: a silence's Start/End datetime-local
// inputs are parsed in time.Local, so the list must render them back in
// THAT SAME zone, with its abbreviation, rather than UTC with no zone label
// at all -- an admin typing "12:00" must see "12:00" (plus the zone) back,
// never some UTC-shifted reading. Asia/Kolkata (IST, no DST) gives a fixed,
// deterministic abbreviation for the assertion.
func TestFleetSilencesDisplayedStartMatchesTypedWallClockPlusZone(t *testing.T) {
	withLocalTZ(t, "Asia/Kolkata")
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/silences")

	values := formValuesForButton(t, body, "op", "save")
	values.Set("matcher_0_tag", "web")
	values.Set("start_at", "2030-06-01T12:00")
	rr := postForm(h, "/fleet/silences", values, cookie, "")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST /fleet/silences status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}

	// 2030-06-01 is far in the future relative to whenever this test runs,
	// so the created silence lands on the Upcoming tab, not Active.
	rr2 := fleetGetAsViewer(t, d, "/fleet/silences?tab=upcoming")
	if rr2.Code != http.StatusOK {
		t.Fatalf("GET /fleet/silences?tab=upcoming status = %d, want 200, body: %s", rr2.Code, rr2.Body.String())
	}
	got := rr2.Body.String()
	if !strings.Contains(got, "2030-06-01 12:00 IST") {
		t.Errorf("displayed start missing the typed wall-clock value plus zone label 'IST', body:\n%s", got)
	}
}

// TestFleetSilencesTimeZoneNotePresent pins the other half of fix round 1's
// ruling: a visible note next to the create-silence form's Start/End inputs
// naming the zone. Driven as admin -- the note lives inside the create
// form itself, which (like every mutation control on this page) a viewer
// never sees at all.
func TestFleetSilencesTimeZoneNotePresent(t *testing.T) {
	withLocalTZ(t, "Asia/Kolkata")
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, _, _ := fleetAdminSessionGet(t, d, "/fleet/silences")
	if !strings.Contains(body, "Times are in Asia/Kolkata (IST)") {
		t.Errorf("missing the 'Times are in ...' zone note, body:\n%s", body)
	}
}

// TestFleetSilencesCreateFutureStartLandsOnUpcoming pins task-4-brief.md's
// exact ruling: "allow a future start, which lands on the Upcoming tab".
func TestFleetSilencesCreateFutureStartLandsOnUpcoming(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/silences")

	values := formValuesForButton(t, body, "op", "save")
	values.Set("matcher_0_tag", "web")
	values.Set("start_at", "2099-01-01T00:00")
	rr := postForm(h, "/fleet/silences", values, cookie, "")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST /fleet/silences status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/fleet/silences?flash=silence_created&tab=upcoming" {
		t.Errorf("redirect Location = %q, want .../fleet/silences?flash=silence_created&tab=upcoming", loc)
	}
}

// TestFleetSilencesCreateEmptyMatcherRejected pins the backend's "a silence
// must match something" rejection surfacing inline, never a 500 -- the
// default fresh form's one matcher row is left entirely blank.
func TestFleetSilencesCreateEmptyMatcherRejected(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/silences")

	values := formValuesForButton(t, body, "op", "save")
	rr := postForm(h, "/fleet/silences", values, cookie, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /fleet/silences with an empty matcher status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.silences) != 0 {
		t.Errorf("CreateSilence must not be called on a validation failure, got %d silences", len(fleet.silences))
	}
	if !strings.Contains(rr.Body.String(), "must match something") {
		t.Errorf("missing inline empty-matcher validation message, body:\n%s", rr.Body.String())
	}
}

// TestFleetSilencesCreateValidationErrorKeepsInput pins global-
// constraints.md's "validation errors render inline ... ALL input
// preserved": an end time before the start time is rejected, and the
// matcher/comment/start/end the admin typed are all still on the page.
func TestFleetSilencesCreateValidationErrorKeepsInput(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/silences")

	values := formValuesForButton(t, body, "op", "save")
	values.Set("matcher_0_tag", "web")
	values.Set("comment", "oops backwards")
	values.Set("start_at", "2030-06-01T12:00")
	values.Set("end_at", "2030-06-01T10:00") // before start
	rr := postForm(h, "/fleet/silences", values, cookie, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /fleet/silences with end before start status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.silences) != 0 {
		t.Errorf("CreateSilence must not be called on a validation failure, got %d silences", len(fleet.silences))
	}
	got := rr.Body.String()
	if !strings.Contains(got, "must be after") {
		t.Errorf("missing inline end-before-start validation message, body:\n%s", got)
	}
	for _, want := range []string{`value="web"`, `value="oops backwards"`, `value="2030-06-01T12:00"`, `value="2030-06-01T10:00"`} {
		if !strings.Contains(got, want) {
			t.Errorf("posted input not preserved: missing %q\nbody:\n%s", want, got)
		}
	}
}

// TestFleetSilencesCreateAddMatcherRowRoundTrip pins the add/remove-row
// draft pattern itself (task-4-brief.md: "the same single-form draft
// pattern as the alerting editor"): clicking "Add matcher" must reshape the
// draft (now two rows) and re-render at 200 WITHOUT saving anything.
func TestFleetSilencesCreateAddMatcherRowRoundTrip(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/silences")

	values := formValuesForButton(t, body, "op", "add_matcher")
	rr := postForm(h, "/fleet/silences", values, cookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/silences op=add_matcher status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.silences) != 0 {
		t.Errorf("add_matcher must never call CreateSilence, got %d silences", len(fleet.silences))
	}
	if strings.Count(rr.Body.String(), `name="matcher_0_tag"`) != 1 || !strings.Contains(rr.Body.String(), `name="matcher_1_tag"`) {
		t.Errorf("add_matcher did not add a second matcher row, body:\n%s", rr.Body.String())
	}
}

// TestFleetSilencesAddThenRemoveMatcherPreservesOtherFields is fix round 1
// review's requested browser-faithful test: fill Start/Duration/Comment,
// click "Add matcher" (reshaping the draft, re-rendering at 200), then click
// "Remove matcher" on the newly-added row -- every OTHER field (the first
// matcher's own tag, Start, Duration, Comment) must survive both round
// trips unchanged.
func TestFleetSilencesAddThenRemoveMatcherPreservesOtherFields(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/silences")

	values := formValuesForButton(t, body, "op", "add_matcher")
	values.Set("matcher_0_tag", "web")
	values.Set("start_at", "2030-01-01T08:00")
	values.Set("duration", "4h")
	values.Set("comment", "keep me")
	rr := postForm(h, "/fleet/silences", values, cookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("op=add_matcher status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body = rr.Body.String()
	for _, want := range []string{`value="2030-01-01T08:00"`, `value="keep me"`, `value="web"`, `value="4h" selected`} {
		if !strings.Contains(body, want) {
			t.Fatalf("after add_matcher, missing %q, body:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `name="matcher_1_tag"`) {
		t.Fatalf("add_matcher did not add a second row, body:\n%s", body)
	}

	values2 := formValuesForButton(t, body, "op", "remove_matcher:1")
	rr2 := postForm(h, "/fleet/silences", values2, cookie, "")
	if rr2.Code != http.StatusOK {
		t.Fatalf("op=remove_matcher status = %d, want 200, body: %s", rr2.Code, rr2.Body.String())
	}
	body2 := rr2.Body.String()
	for _, want := range []string{`value="2030-01-01T08:00"`, `value="keep me"`, `value="web"`, `value="4h" selected`} {
		if !strings.Contains(body2, want) {
			t.Errorf("after remove_matcher, missing %q -- another field did not survive, body:\n%s", want, body2)
		}
	}
	if strings.Contains(body2, `name="matcher_1_tag"`) {
		t.Errorf("remove_matcher did not remove the second row, body:\n%s", body2)
	}
	if len(fleet.silences) != 0 {
		t.Errorf("add/remove ops must never call CreateSilence, got %d silences", len(fleet.silences))
	}
}

// TestFleetMaintenanceAddThenRemoveMatcherPreservesOtherFields is the same
// browser-faithful add-then-remove test for the maintenance form: name,
// weekdays, From/To, and a custom TZ must all survive both round trips.
func TestFleetMaintenanceAddThenRemoveMatcherPreservesOtherFields(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/silences")

	values := formValuesForButton(t, body, "op", "add_mnt_matcher")
	values.Set("mnt_matcher_0_tag", "web")
	values.Set("name", "keep-me-too")
	values.Add("weekday", "2")
	values.Add("weekday", "4")
	values.Set("from", "01:00")
	values.Set("to", "02:00")
	values.Set("tz_custom", "Asia/Kathmandu")
	rr := postForm(h, "/fleet/maintenance", values, cookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("op=add_mnt_matcher status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body = rr.Body.String()
	for _, want := range []string{
		`value="keep-me-too"`, `value="Asia/Kathmandu"`, `value="web"`, `value="01:00"`, `value="02:00"`,
		`name="weekday" value="2" checked`, `name="weekday" value="4" checked`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("after add_mnt_matcher, missing %q, body:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `name="mnt_matcher_1_tag"`) {
		t.Fatalf("add_mnt_matcher did not add a second row, body:\n%s", body)
	}

	values2 := formValuesForButton(t, body, "op", "remove_mnt_matcher:1")
	rr2 := postForm(h, "/fleet/maintenance", values2, cookie, "")
	if rr2.Code != http.StatusOK {
		t.Fatalf("op=remove_mnt_matcher status = %d, want 200, body: %s", rr2.Code, rr2.Body.String())
	}
	body2 := rr2.Body.String()
	for _, want := range []string{
		`value="keep-me-too"`, `value="Asia/Kathmandu"`, `value="web"`, `value="01:00"`, `value="02:00"`,
		`name="weekday" value="2" checked`, `name="weekday" value="4" checked`,
	} {
		if !strings.Contains(body2, want) {
			t.Errorf("after remove_mnt_matcher, missing %q -- another field did not survive, body:\n%s", want, body2)
		}
	}
	if strings.Contains(body2, `name="mnt_matcher_1_tag"`) {
		t.Errorf("remove_mnt_matcher did not remove the second row, body:\n%s", body2)
	}
	if len(fleet.maintenances) != 0 {
		t.Errorf("add/remove ops must never call SaveMaintenance, got %d maintenances", len(fleet.maintenances))
	}
}

// ---- expire -----------------------------------------------------------------

// TestFleetSilenceExpireWithConfirm drives the real two-step confirm control
// (style.css's .confirm-toggle, the same CSS-only pattern fleet_admin.html's
// revoke/remove use) through formValuesForButton, and pins that the actor
// recorded is the SIGNED-IN web user.
func TestFleetSilenceExpireWithConfirm(t *testing.T) {
	fleet := &fakeFleet{silences: []core.Silence{
		{ID: "sil1", Matchers: []core.Matcher{{Tag: "web"}}, Start: 1000, End: 9999999999, Author: "root"},
	}}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/silences")

	values := formValuesForButton(t, body, "", "")
	rr := postForm(h, "/fleet/silences/sil1/expire", values, cookie, "")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST expire status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.expiredSilenceID != "sil1" {
		t.Errorf("expired silence id = %q, want sil1", fleet.expiredSilenceID)
	}
	if fleet.expiredSilenceActor != "root" {
		t.Errorf("expired silence actor = %q, want the signed-in admin's name (root)", fleet.expiredSilenceActor)
	}
	if fleet.silences[0].End > time.Now().Unix() {
		t.Errorf("silence End = %d, want pulled back to now", fleet.silences[0].End)
	}
}

// ---- tabs -------------------------------------------------------------------

// TestFleetSilencesTabs pins the Active/Upcoming/Expired classification.
func TestFleetSilencesTabs(t *testing.T) {
	now := time.Now().Unix()
	fleet := &fakeFleet{silences: []core.Silence{
		{ID: "active1", Matchers: []core.Matcher{{Tag: "web"}}, Start: now - 100, End: now + 100, Comment: "the-active-one"},
		{ID: "upcoming1", Matchers: []core.Matcher{{Tag: "web"}}, Start: now + 1000, End: now + 2000, Comment: "the-upcoming-one"},
		{ID: "expired1", Matchers: []core.Matcher{{Tag: "web"}}, Start: now - 2000, End: now - 1000, Comment: "the-expired-one"},
	}}
	d := fleetAdminDeps(t, fleet)

	cases := []struct {
		tab      string
		wantIn   string
		wantOut1 string
		wantOut2 string
	}{
		{"active", "the-active-one", "the-upcoming-one", "the-expired-one"},
		{"upcoming", "the-upcoming-one", "the-active-one", "the-expired-one"},
		{"expired", "the-expired-one", "the-active-one", "the-upcoming-one"},
	}
	for _, c := range cases {
		rr := fleetGetAsViewer(t, d, "/fleet/silences?tab="+c.tab)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /fleet/silences?tab=%s status = %d, want 200", c.tab, rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, c.wantIn) {
			t.Errorf("tab=%s: missing %q, body:\n%s", c.tab, c.wantIn, body)
		}
		if strings.Contains(body, c.wantOut1) || strings.Contains(body, c.wantOut2) {
			t.Errorf("tab=%s: must not show %q/%q, body:\n%s", c.tab, c.wantOut1, c.wantOut2, body)
		}
	}
}

// TestFleetSilencesPagination pins global-constraints.md's "Lists are
// paginated (50 per page)" ruling for the (tab-filtered) silences list.
func TestFleetSilencesPagination(t *testing.T) {
	now := time.Now().Unix()
	sils := make([]core.Silence, 0, 60)
	for i := 0; i < 60; i++ {
		sils = append(sils, core.Silence{
			ID:       "sil" + strconv.Itoa(i),
			Matchers: []core.Matcher{{Tag: "web"}}, Start: now - 100, End: now + 100,
			Comment: "silence-" + strconv.Itoa(i),
		})
	}
	fleet := &fakeFleet{silences: sils}
	d := fleetAdminDeps(t, fleet)

	rr := fleetGetAsViewer(t, d, "/fleet/silences?tab=active")
	if rr.Code != http.StatusOK {
		t.Fatalf("page 1 status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "page 1 of 2") {
		t.Errorf("page 1: missing 'page 1 of 2', body:\n%s", body)
	}
	if strings.Contains(body, "silence-50") {
		t.Errorf("page 1 shows a row past the 50-item page size, body:\n%s", body)
	}

	rr2 := fleetGetAsViewer(t, d, "/fleet/silences?tab=active&page=2")
	if rr2.Code != http.StatusOK {
		t.Fatalf("page 2 status = %d, want 200", rr2.Code)
	}
	body2 := rr2.Body.String()
	if !strings.Contains(body2, "silence-50") {
		t.Errorf("page 2: missing row 50, body:\n%s", body2)
	}
	if strings.Contains(body2, "silence-0<") {
		t.Errorf("page 2: must not show page-1 rows, body:\n%s", body2)
	}
}

// ---- maintenance ------------------------------------------------------------

// TestFleetMaintenanceNextOccurrenceRendering pins the ruling: the list
// shows each window's next occurrence, computed server-side from the SAME
// helper the engine uses (core.NextMaintenanceOccurrence). A window covering
// every weekday, all day, is always "active now" regardless of when this
// test runs.
func TestFleetMaintenanceNextOccurrenceRendering(t *testing.T) {
	fleet := &fakeFleet{maintenances: []core.Maintenance{
		{ID: "mnt1", Name: "always-on", Matchers: []core.Matcher{{Tag: "web"}}, Weekdays: []int{0, 1, 2, 3, 4, 5, 6}, From: "00:00", To: "23:59", TZ: "UTC", Author: "root"},
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/silences")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet/silences status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "always-on") {
		t.Fatalf("missing maintenance window name, body:\n%s", body)
	}
	if !strings.Contains(body, "active now") {
		t.Errorf("missing next-occurrence 'active now' text for an always-on window, body:\n%s", body)
	}
}

// TestFleetMaintenanceCreateRoundTripsWithAuthor drives the real rendered
// form and pins Author == the signed-in web user, plus that the weekday
// checkboxes/From/To/TZ all round-trip.
func TestFleetMaintenanceCreateRoundTripsWithAuthor(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/silences")

	values := formValuesForButton(t, body, "op", "save_maintenance")
	values.Set("name", "nightly backups")
	values.Set("mnt_matcher_0_tag", "web")
	values.Add("weekday", "1")
	values.Add("weekday", "3")
	values.Set("from", "22:00")
	values.Set("to", "23:00")
	values.Set("tz_select", "UTC")
	rr := postForm(h, "/fleet/maintenance", values, cookie, "")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST /fleet/maintenance status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.maintenances) != 1 {
		t.Fatalf("maintenances created = %d, want 1", len(fleet.maintenances))
	}
	m := fleet.maintenances[0]
	if m.Author != "root" {
		t.Errorf("Maintenance Author = %q, want the signed-in admin's name (root)", m.Author)
	}
	if m.Name != "nightly backups" || m.From != "22:00" || m.To != "23:00" || m.TZ != "UTC" {
		t.Errorf("Maintenance = %+v, want name/from/to/tz to round-trip", m)
	}
	if len(m.Weekdays) != 2 || m.Weekdays[0] != 1 || m.Weekdays[1] != 3 {
		t.Errorf("Maintenance Weekdays = %+v, want [1 3]", m.Weekdays)
	}
}

// TestFleetMaintenanceCreateValidationErrorKeepsInput pins "validation
// errors render inline ... ALL input preserved" for the maintenance form: no
// weekday checked.
func TestFleetMaintenanceCreateValidationErrorKeepsInput(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/silences")

	values := formValuesForButton(t, body, "op", "save_maintenance")
	values.Set("name", "no weekday chosen")
	values.Set("mnt_matcher_0_tag", "web")
	values.Set("from", "22:00")
	values.Set("to", "23:00")
	rr := postForm(h, "/fleet/maintenance", values, cookie, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /fleet/maintenance with no weekday status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if len(fleet.maintenances) != 0 {
		t.Errorf("SaveMaintenance must not be called on a validation failure, got %d maintenances", len(fleet.maintenances))
	}
	got := rr.Body.String()
	if !strings.Contains(got, "at least one weekday") {
		t.Errorf("missing inline weekday validation message, body:\n%s", got)
	}
	if !strings.Contains(got, `value="no weekday chosen"`) {
		t.Errorf("posted name not preserved, body:\n%s", got)
	}
}

// TestFleetMaintenanceDeleteWithConfirm drives the real two-step confirm
// control and pins that the actor recorded is the signed-in web user.
func TestFleetMaintenanceDeleteWithConfirm(t *testing.T) {
	fleet := &fakeFleet{maintenances: []core.Maintenance{
		{ID: "mnt1", Name: "nightly", Matchers: []core.Matcher{{Tag: "web"}}, Weekdays: []int{1}, From: "22:00", To: "23:00", TZ: "UTC", Author: "root"},
	}}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/silences")

	values := formValuesForButton(t, body, "", "")
	rr := postForm(h, "/fleet/maintenance/mnt1/delete", values, cookie, "")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST delete status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.deletedMaintenanceID != "mnt1" {
		t.Errorf("deleted maintenance id = %q, want mnt1", fleet.deletedMaintenanceID)
	}
	if fleet.deletedMaintenanceActor != "root" {
		t.Errorf("deleted maintenance actor = %q, want the signed-in admin's name (root)", fleet.deletedMaintenanceActor)
	}
	if len(fleet.maintenances) != 0 {
		t.Errorf("maintenances after delete = %d, want 0", len(fleet.maintenances))
	}
}

// ---- 404 / escaping ---------------------------------------------------------

// TestFleetSilencesUnknownIDsAre404 pins that expiring/deleting an id this
// fake doesn't recognize surfaces as EXACTLY 404 (core.ErrNotFound,
// fleetAPIErrStatus), never a 500 -- fix round 1: the fake's "no such X"
// errors now wrap core.ErrNotFound (deps_api_test.go), matching the real
// backend, so this asserts the precise status rather than "any 4xx".
func TestFleetSilencesUnknownIDsAre404(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/silences/nosuch/expire", url.Values{})
	if rr.Code != http.StatusNotFound {
		t.Errorf("expire unknown id status = %d, want 404", rr.Code)
	}
	rr2 := fleetAdminPost(t, d, "/fleet/maintenance/nosuch/delete", url.Values{})
	if rr2.Code != http.StatusNotFound {
		t.Errorf("delete unknown maintenance id status = %d, want 404", rr2.Code)
	}
}

// TestFleetSilencesNamesEscaped pins that a maintenance name/comment
// containing HTML is escaped, never rendered raw (html/template's default
// auto-escaping -- this just pins that nothing here bypasses it, e.g. via a
// "safeHTML"-style funcMap helper).
func TestFleetSilencesNamesEscaped(t *testing.T) {
	fleet := &fakeFleet{
		silences: []core.Silence{
			{ID: "sil1", Matchers: []core.Matcher{{Tag: "web"}}, Start: 1000, End: 9999999999, Comment: "<script>alert(1)</script>"},
		},
		maintenances: []core.Maintenance{
			{ID: "mnt1", Name: "<script>alert(2)</script>", Matchers: []core.Matcher{{Tag: "web"}}, Weekdays: []int{1}, From: "22:00", To: "23:00", TZ: "UTC"},
		},
	}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/silences")
	body := rr.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") || strings.Contains(body, "<script>alert(2)</script>") {
		t.Errorf("raw <script> found unescaped in body:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("expected escaped silence comment, body:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(2)&lt;/script&gt;") {
		t.Errorf("expected escaped maintenance name, body:\n%s", body)
	}
}
