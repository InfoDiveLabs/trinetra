package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// ---- RBAC / gating ---------------------------------------------------------

// TestFleetAuditSoloDaemon404s pins "master only, else 404" for the page.
func TestFleetAuditSoloDaemon404s(t *testing.T) {
	d := fleetSoloDeps(t)
	rr := fleetAdminGetAsRole(t, d, RoleAdmin, "/fleet/audit")
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /fleet/audit on solo status = %d, want 404", rr.Code)
	}
}

// TestFleetAuditAnonymousRedirectsToLogin pins the anonymous case: no
// session at all gets a 302 to the login page, never a 403/404.
func TestFleetAuditAnonymousRedirectsToLogin(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	rr := fleetGetAnonymous(d, "/fleet/audit")
	if rr.Code != http.StatusFound {
		t.Errorf("GET /fleet/audit anonymous status = %d, want 302", rr.Code)
	}
}

// TestFleetAuditViewerDenied pins admin-only end to end: unlike Managed
// config/Alerting, a viewer gets no read access at all -- the route list has a
// single admin GET, no viewer-readable variant.
func TestFleetAuditViewerDenied(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	rr := fleetGetAsViewer(t, d, "/fleet/audit")
	if rr.Code != http.StatusForbidden {
		t.Errorf("GET /fleet/audit as viewer status = %d, want 403", rr.Code)
	}
}

// ---- rendering / filters / pagination / escaping ---------------------------

func fakeAuditEntries(n int) []core.AuditEntry {
	out := make([]core.AuditEntry, 0, n)
	for i := 0; i < n; i++ {
		actor := "root"
		action := "fleet.node.rename"
		if i%2 == 0 {
			actor = "cli"
			action = "fleet.managed.save"
		}
		out = append(out, core.AuditEntry{
			TS: int64(2000000000 - i), Actor: actor, Action: action,
			Target: fmt.Sprintf("node-%d", i), Detail: fmt.Sprintf("detail-%d", i),
		})
	}
	return out
}

// TestFleetAuditListsEntriesAndCallsWithBriefLimit pins the basic render plus
// the "Audit(limit=5000)" call.
func TestFleetAuditListsEntriesAndCallsWithBriefLimit(t *testing.T) {
	fleet := &fakeFleet{auditEntries: fakeAuditEntries(3)}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminGetAsRole(t, d, RoleAdmin, "/fleet/audit")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.lastAuditLimit != fleetAuditQueryLimit {
		t.Errorf("Audit called with limit %d, want %d", fleet.lastAuditLimit, fleetAuditQueryLimit)
	}
	body := rr.Body.String()
	for _, want := range []string{"node-0", "node-1", "node-2", "detail-0", "fleet.node.rename", "fleet.managed.save"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

// TestFleetAuditFilterByActor pins the ?actor= GET filter.
func TestFleetAuditFilterByActor(t *testing.T) {
	fleet := &fakeFleet{auditEntries: fakeAuditEntries(4)}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminGetAsRole(t, d, RoleAdmin, "/fleet/audit?actor=cli")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "node-1") || strings.Contains(body, "node-3") {
		t.Errorf("actor=cli filter leaked a root-actor entry, body:\n%s", body)
	}
	if !strings.Contains(body, "node-0") || !strings.Contains(body, "node-2") {
		t.Errorf("actor=cli filter dropped a matching entry, body:\n%s", body)
	}
}

// TestFleetAuditFilterByAction pins the ?action= GET filter, and that both
// filters AND together.
func TestFleetAuditFilterByAction(t *testing.T) {
	fleet := &fakeFleet{auditEntries: fakeAuditEntries(4)}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminGetAsRole(t, d, RoleAdmin, "/fleet/audit?action=fleet.node.rename&actor=root")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "node-0") || strings.Contains(body, "node-2") {
		t.Errorf("action+actor filter leaked a non-matching entry, body:\n%s", body)
	}
	if !strings.Contains(body, "node-1") || !strings.Contains(body, "node-3") {
		t.Errorf("action+actor filter dropped a matching entry, body:\n%s", body)
	}
}

// TestFleetAuditPagination pins 50/page pagination over the filtered set.
func TestFleetAuditPagination(t *testing.T) {
	fleet := &fakeFleet{auditEntries: fakeAuditEntries(120)}
	d := fleetAdminDeps(t, fleet)

	rr := fleetAdminGetAsRole(t, d, RoleAdmin, "/fleet/audit")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "node-0") || strings.Contains(body, "node-50") {
		t.Errorf("page 1 should show the first 50 entries only, body:\n%s", body)
	}
	if !strings.Contains(body, "page 1 of 3") {
		t.Errorf("missing 'page 1 of 3', body:\n%s", body)
	}

	rr2 := fleetAdminGetAsRole(t, d, RoleAdmin, "/fleet/audit?page=2")
	if rr2.Code != http.StatusOK {
		t.Fatalf("page 2 status = %d, body: %s", rr2.Code, rr2.Body.String())
	}
	body2 := rr2.Body.String()
	if !strings.Contains(body2, "node-50") || strings.Contains(body2, "node-0") {
		t.Errorf("page 2 should show entries 50-99 only, body:\n%s", body2)
	}
}

// TestFleetAuditDetailEscaped pins that Detail is rendered escaped, never as
// live markup.
func TestFleetAuditDetailEscaped(t *testing.T) {
	fleet := &fakeFleet{auditEntries: []core.AuditEntry{
		{TS: 1000, Actor: "root", Action: "fleet.managed.save", Target: "f1", Detail: `<script>alert(1)</script>`},
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminGetAsRole(t, d, RoleAdmin, "/fleet/audit")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("Detail rendered unescaped, body:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("Detail not escaped as expected, body:\n%s", body)
	}
}

// TestFleetAuditTimeInMasterLocalZone pins that times are shown in the master's
// own local zone with its abbreviation, via silenceTimeText -- exactly like
// every other datetime on the fleet pages.
func TestFleetAuditTimeInMasterLocalZone(t *testing.T) {
	withLocalTZ(t, "Asia/Kolkata")
	fleet := &fakeFleet{auditEntries: []core.AuditEntry{
		{TS: 1893456000, Actor: "root", Action: "fleet.node.rename", Target: "n1"},
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminGetAsRole(t, d, RoleAdmin, "/fleet/audit")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	want := silenceTimeText(1893456000)
	if !strings.Contains(body, want) {
		t.Errorf("time not shown in master local zone with abbreviation (want %q), body:\n%s", want, body)
	}
	if !strings.Contains(body, "IST") {
		t.Errorf("missing IST zone abbreviation, body:\n%s", body)
	}
}
