package trinetra

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

func newTestRuntime(t *testing.T, role string, in *statusInputs) (*statusPageRuntime, *[]string) {
	t.Helper()
	var echoed []string
	cfg := config.Default()
	_ = cfg.Set("status.echo_channels", "tg")
	r := newStatusPageRuntime(statusPageDir(t.TempDir()), func() string { return role },
		func() *config.Config { return cfg },
		func(now time.Time) statusInputs { x := *in; x.Now = now; return x },
		func(text string, ch []string) { echoed = append(echoed, text) }, t.Logf)
	return r, &echoed
}

func TestStatusPageRefusedOnChild(t *testing.T) {
	r, _ := newTestRuntime(t, config.RoleChild, &statusInputs{})
	if _, err := r.Services(); !errors.Is(err, core.ErrStatusPageOnChild) {
		t.Fatalf("Services on child: %v", err)
	}
	if _, err := r.Public(); !errors.Is(err, core.ErrStatusPageOnChild) {
		t.Fatalf("Public on child: %v", err)
	}
	r.Tick(evalT0) // must not panic or write
	if _, err := r.SetService(core.StatusService{ID: "a", Name: "A", Targets: []core.StatusTarget{{Kind: core.TargetHost}}}, "x"); !errors.Is(err, core.ErrStatusPageOnChild) {
		t.Errorf("SetService: %v", err)
	}
	if _, err := r.CreateIncident(core.NewIncident{}, "x"); !errors.Is(err, core.ErrStatusPageOnChild) {
		t.Errorf("CreateIncident: %v", err)
	}
	if _, err := r.PostUpdate("i", core.NewUpdate{}, "x"); !errors.Is(err, core.ErrStatusPageOnChild) {
		t.Errorf("PostUpdate: %v", err)
	}
	if err := r.DeleteIncident("i", "x"); !errors.Is(err, core.ErrStatusPageOnChild) {
		t.Errorf("DeleteIncident: %v", err)
	}
	if _, err := r.Evaluation(); !errors.Is(err, core.ErrStatusPageOnChild) {
		t.Errorf("Evaluation: %v", err)
	}
	if _, err := r.Incidents(true); !errors.Is(err, core.ErrStatusPageOnChild) {
		t.Errorf("Incidents: %v", err)
	}
}

func TestRuntimeTickOpensIncidentAndEchoes(t *testing.T) {
	in := &statusInputs{Tags: map[string][]string{"n1": {"api"}}}
	r, echoed := newTestRuntime(t, config.RoleMaster, in)
	if _, err := r.SetService(core.StatusService{ID: "api", Name: "API", HoldDownSec: 0,
		Targets: []core.StatusTarget{{Kind: core.TargetTag, Value: "api"}}}, "alice"); err != nil {
		t.Fatal(err)
	}
	r.Tick(evalT0)
	in.Down = map[string]bool{"n1": true}
	r.Tick(evalT0.Add(time.Minute))
	incs, _ := r.Incidents(false)
	if len(incs) != 1 || incs[0].Impact != core.StateOutage {
		t.Fatalf("incidents %+v", incs)
	}
	if len(*echoed) != 1 || !strings.Contains((*echoed)[0], "Outage: API") {
		t.Fatalf("echo %v", *echoed)
	}
	// persisted
	r2 := newStatusPageRuntime(r.dir, func() string { return config.RoleMaster }, r.getCfg, r.gather, nil, t.Logf)
	if incs, _ := r2.Incidents(false); len(incs) != 1 {
		t.Fatal("incident not persisted")
	}
}

func TestPublicNeverLeaksInternals(t *testing.T) {
	secrets := []string{"db-prod-07", "10.20.0.14", "region:fra", "docker:payments-pg", "payments-pg", "nginx.service", "/var/lib/postgresql", "disk:/var/lib/postgresql", "alice-oncall"}
	in := &statusInputs{Tags: map[string][]string{"db-prod-07": {"region:fra"}},
		Signals: []statusSignal{{Node: "db-prod-07", Key: "docker:payments-pg", Severity: "critical"}}}
	r, _ := newTestRuntime(t, config.RoleMaster, in)
	_, _ = r.SetService(core.StatusService{ID: "pay", Name: "Payments", HoldDownSec: 0, Targets: []core.StatusTarget{
		{Kind: core.TargetTag, Value: "region:fra"},
		{Kind: core.TargetContainer, Node: "db-prod-07", Value: "payments-pg"},
		{Kind: core.TargetUnit, Node: "10.20.0.14", Value: "nginx.service"},
		{Kind: core.TargetMount, Node: "db-prod-07", Value: "/var/lib/postgresql"},
	}}, "alice-oncall")
	r.Tick(evalT0)
	inc, _ := r.Incidents(false)
	if _, err := r.PostUpdate(inc[0].ID, core.NewUpdate{Status: core.IncidentIdentified, Message: "Found it"}, "alice-oncall"); err != nil {
		t.Fatal(err)
	}
	pub, err := r.Public()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(pub)
	for _, s := range secrets {
		if strings.Contains(string(b), s) {
			t.Errorf("public status leaks %q: %s", s, b)
		}
	}
	if pub.Overall != core.OverallMajor || len(pub.Active) != 1 || pub.Active[0].Services[0] != "Payments" {
		t.Fatalf("public %+v", pub)
	}
	if pub.Active[0].Updates[0].Message != "Found it" {
		t.Fatal("updates must be newest first")
	}
	if ups := pub.Active[0].Updates; ups[0].ID == "" || ups[0].ID == ups[len(ups)-1].ID {
		t.Fatalf("public updates need distinct ids: %+v", ups)
	}
}

func TestPublicDropsDeletedService(t *testing.T) {
	in := &statusInputs{}
	r, _ := newTestRuntime(t, config.RoleMaster, in)
	_, _ = r.SetService(core.StatusService{ID: "api", Name: "API", Targets: []core.StatusTarget{{Kind: core.TargetHost}}}, "a")
	inc, err := r.CreateIncident(core.NewIncident{Title: "Slow", Services: []string{"api"}, Impact: core.StateDegraded,
		Update: core.NewUpdate{Status: core.IncidentInvestigating, Message: "m"}}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteService("api", "a"); err != nil {
		t.Fatal(err)
	}
	pub, err := r.Public()
	if err != nil {
		t.Fatal(err)
	}
	if len(pub.Active) != 1 || pub.Active[0].ID != inc.ID || len(pub.Active[0].Services) != 0 {
		t.Fatalf("%+v", pub.Active)
	}
}

func TestPublicHistoryHas90Days(t *testing.T) {
	in := &statusInputs{}
	r, _ := newTestRuntime(t, config.RoleMaster, in)
	_, _ = r.SetService(core.StatusService{ID: "api", Name: "API", Targets: []core.StatusTarget{{Kind: core.TargetHost}}}, "a")
	r.Tick(evalT0)
	pub := buildPublicStatus("Status", r.data, evalT0)
	h := pub.Services[0].History
	if len(h) != 90 || h[89].Date != "2026-10-08" || h[89].State != core.StateOperational || h[0].State != "" {
		t.Fatalf("history len %d last %+v first %+v", len(h), h[len(h)-1], h[0])
	}
}

func TestRuntimeReadsReturnCopies(t *testing.T) {
	in := &statusInputs{}
	r, _ := newTestRuntime(t, config.RoleMaster, in)
	_, _ = r.SetService(core.StatusService{ID: "api", Name: "API", Targets: []core.StatusTarget{{Kind: core.TargetHost}}}, "a")
	inc, err := r.CreateIncident(core.NewIncident{Title: "Slow", Services: []string{"api"}, Impact: core.StateDegraded,
		Update: core.NewUpdate{Status: core.IncidentInvestigating, Message: "m"}}, "a")
	if err != nil {
		t.Fatal(err)
	}
	svcs, _ := r.Services()
	svcs[0].Targets[0].Kind = "mutated"
	got, _ := r.Incident(inc.ID)
	got.Services[0] = "mutated"
	got.Updates[0].Message = "mutated"
	list, _ := r.Incidents(true)
	list[0].Services[0] = "mutated"
	list[0].Updates[0].Message = "mutated"

	svcs2, _ := r.Services()
	if svcs2[0].Targets[0].Kind != core.TargetHost {
		t.Error("Services leaks Targets")
	}
	again, _ := r.Incident(inc.ID)
	if again.Services[0] != "api" || again.Updates[0].Message != "m" {
		t.Errorf("incident leaks internal slices: %+v", again)
	}
}

func TestRuntimeTickSkipsUnchangedWrites(t *testing.T) {
	in := &statusInputs{}
	r, _ := newTestRuntime(t, config.RoleMaster, in)
	_, _ = r.SetService(core.StatusService{ID: "api", Name: "API", Targets: []core.StatusTarget{{Kind: core.TargetHost}}}, "a")
	r.Tick(evalT0)
	mt := func() map[string]time.Time {
		out := map[string]time.Time{}
		for _, f := range []string{"services.json", "incidents.json", "state.json"} {
			fi, err := os.Stat(filepath.Join(r.dir, f))
			if err != nil {
				t.Fatal(err)
			}
			out[f] = fi.ModTime()
		}
		return out
	}
	before := mt()
	time.Sleep(20 * time.Millisecond)
	r.Tick(evalT0.Add(time.Second))
	after := mt()
	for f, m := range before {
		if !after[f].Equal(m) {
			t.Errorf("%s rewritten with no change", f)
		}
	}
}
