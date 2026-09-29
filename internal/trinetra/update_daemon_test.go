package trinetra

import (
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

func TestUpdateResultAlert(t *testing.T) {
	a, ok := updateResultAlert(update.Result{Version: "0.5.0", From: "0.4.1", Outcome: "rolled_back", Detail: "reported 0.4.1"})
	if !ok || a.Severity != SevCritical || !strings.Contains(a.Title, "rolled back") {
		t.Fatalf("%+v %v", a, ok)
	}
	a, ok = updateResultAlert(update.Result{Version: "0.5.0", From: "0.4.1", Outcome: "committed"})
	if !ok || a.Severity != SevInfo || !strings.Contains(a.Title, "0.4.1 → 0.5.0") {
		t.Fatalf("%+v %v", a, ok)
	}
	if _, ok := updateResultAlert(update.Result{Outcome: "committed", Notified: true}); ok {
		t.Fatal("notified result re-alerted")
	}
}

func TestUpdateAvailableAlertOncePerVersion(t *testing.T) {
	if _, ok := updateAvailableAlert(update.State{Available: "0.5.0"}); !ok {
		t.Fatal("no alert for a new available version")
	}
	if _, ok := updateAvailableAlert(update.State{Available: "0.5.0", AvailableNotified: "0.5.0"}); ok {
		t.Fatal("re-alerted the same available version")
	}
}
