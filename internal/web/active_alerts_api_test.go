package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// selfReq is a plain unscoped request (no /n/{node} routing attached), for
// direct unit tests of functions node-scope.go's apiFor threads through
// (activeAlertsViaAPI, coreVersionViaAPI, ...): nodeFrom(selfReq()) is always
// the implicit self scope, so these calls go through the plain Deps.API path.
func selfReq() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/", nil)
}

// errTestActiveAlerts is a stand-in transport error for the degrade-to-nil
// path (a socket read failing), distinct from "no alerts fired yet".
var errTestActiveAlerts = errors.New("control: active alerts unavailable")

// TestActiveAlertsViaAPIMapsAndSorts pins the core.AlertRecord -> activeAlertView
// projection the web pages use in place of the old on-disk loadActiveAlerts.
func TestActiveAlertsViaAPIMapsAndSorts(t *testing.T) {
	d := Deps{API: fakeAPI{active: []core.AlertRecord{
		{Key: "cpu", Severity: "critical", Source: "cpu = 95 >= 90", Time: 1000, Acked: false},
		{Key: "mem", Severity: "warning", Source: "mem = 80 >= 75", Time: 3000, Acked: true},
		{Key: "disk", Severity: "warning", Source: "disk full", Time: 2000},
	}}}

	got := activeAlertsViaAPI(selfReq(), d)
	if len(got) != 3 {
		t.Fatalf("activeAlertsViaAPI len = %d, want 3", len(got))
	}

	wantKeys := []string{"mem", "disk", "cpu"} // most-recent-first by Since
	for i, k := range wantKeys {
		if got[i].Key != k {
			t.Errorf("got[%d].Key = %q, want %q (most-recent-first order)", i, got[i].Key, k)
		}
	}

	cpu := got[2]
	if cpu.Reason != "cpu = 95 >= 90" {
		t.Errorf("cpu Reason = %q, want the Source text", cpu.Reason)
	}
	if cpu.Since != 1000 {
		t.Errorf("cpu Since = %d, want 1000 (from Time)", cpu.Since)
	}
	if !cpu.Critical {
		t.Errorf("cpu Critical = false, want true (severity critical)")
	}
	if cpu.Acked {
		t.Errorf("cpu Acked = true, want false")
	}

	mem := got[0]
	if mem.Critical {
		t.Errorf("mem Critical = true, want false (severity warning)")
	}
	if !mem.Acked {
		t.Errorf("mem Acked = false, want true")
	}
}

// TestActiveAlertsViaAPIDegradesToNil pins the display-only tolerance: a nil API (or one
// whose ActiveAlerts errors) renders as "no active alerts" rather than failing the page.
func TestActiveAlertsViaAPIDegradesToNil(t *testing.T) {
	if got := activeAlertsViaAPI(selfReq(), Deps{}); got != nil {
		t.Errorf("activeAlertsViaAPI(nil API) = %v, want nil", got)
	}
	if got := activeAlertsViaAPI(selfReq(), Deps{API: fakeAPI{activeErr: errTestActiveAlerts}}); got != nil {
		t.Errorf("activeAlertsViaAPI(erroring API) = %v, want nil", got)
	}
}
