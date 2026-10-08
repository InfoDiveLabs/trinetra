package trinetra

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

func TestStatusPageStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "status")
	d := loadStatusPage(dir, t.Logf)
	d.Services = []core.StatusService{{ID: "api", Name: "API", HoldDownSec: 180}}
	d.Incidents = []core.StatusIncident{{ID: "inc1", Title: "x", Status: core.IncidentInvestigating}}
	d.State["api"] = &serviceRuntimeState{State: core.StateDegraded, History: map[string]core.ServiceState{"2026-10-08": core.StateDegraded}}
	if err := d.save(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"services.json", "incidents.json", "state.json"} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s perm %v, want 0600", f, fi.Mode().Perm())
		}
	}
	got := loadStatusPage(dir, t.Logf)
	if len(got.Services) != 1 || got.Services[0].Name != "API" {
		t.Fatalf("services: %+v", got.Services)
	}
	if len(got.Incidents) != 1 || got.Incidents[0].ID != "inc1" {
		t.Fatalf("incidents: %+v", got.Incidents)
	}
	if got.State["api"].State != core.StateDegraded {
		t.Fatalf("state: %+v", got.State["api"])
	}
}

func TestStatusPageStoreCorruptFileIsSetAside(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "status")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "incidents.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged []string
	d := loadStatusPage(dir, func(f string, a ...any) { logged = append(logged, f) })
	if len(d.Incidents) != 0 {
		t.Fatal("corrupt incidents should load empty")
	}
	ents, _ := os.ReadDir(dir)
	found := false
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "incidents.json.corrupt-") {
			found = true
		}
	}
	if !found {
		t.Fatal("corrupt file was not renamed aside")
	}
	if len(logged) == 0 {
		t.Fatal("corruption was not logged")
	}
}

func TestStatusPageStorePrune(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	d := &statusPageData{State: map[string]*serviceRuntimeState{
		"api": {History: map[string]core.ServiceState{
			"2026-10-08": core.StateOutage,
			"2026-07-09": core.StateDegraded, // 91 days old -> pruned
		}},
	}}
	d.Incidents = []core.StatusIncident{
		{ID: "old", Status: core.IncidentResolved, Resolved: now.AddDate(0, 0, -366).Unix()},
		{ID: "keep", Status: core.IncidentResolved, Resolved: now.AddDate(0, 0, -10).Unix()},
		{ID: "open", Status: core.IncidentInvestigating, Opened: now.AddDate(-2, 0, 0).Unix()},
	}
	d.prune(now)
	if len(d.Incidents) != 2 || d.Incidents[0].ID != "keep" || d.Incidents[1].ID != "open" {
		t.Fatalf("incidents after prune: %+v", d.Incidents)
	}
	if _, ok := d.State["api"].History["2026-07-09"]; ok {
		t.Fatal("91-day-old history kept")
	}
	if _, ok := d.State["api"].History["2026-10-08"]; !ok {
		t.Fatal("today's history dropped")
	}
}
