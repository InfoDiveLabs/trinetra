// internal/trinetra/statuspage_eval_test.go
package trinetra

import (
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

var evalT0 = time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)

func svcTag(id, tag string, hold int) core.StatusService {
	return core.StatusService{ID: id, Name: id, HoldDownSec: hold,
		Targets: []core.StatusTarget{{Kind: core.TargetTag, Value: tag}}}
}

func TestComputeServiceStateTable(t *testing.T) {
	tags := map[string][]string{"n1": {"api"}, "n2": {"web"}}
	cases := []struct {
		name string
		svc  core.StatusService
		in   statusInputs
		want core.ServiceState
	}{
		{"no signals", svcTag("a", "api", 0), statusInputs{Tags: tags}, core.StateOperational},
		{"warning on tagged node", svcTag("a", "api", 0),
			statusInputs{Tags: tags, Signals: []statusSignal{{Node: "n1", Key: "cpu", Severity: "warning"}}}, core.StateDegraded},
		{"critical on tagged node", svcTag("a", "api", 0),
			statusInputs{Tags: tags, Signals: []statusSignal{{Node: "n1", Key: "disk:/", Severity: "critical"}}}, core.StateOutage},
		{"alert on other node ignored", svcTag("a", "api", 0),
			statusInputs{Tags: tags, Signals: []statusSignal{{Node: "n2", Key: "cpu", Severity: "critical"}}}, core.StateOperational},
		{"node down", svcTag("a", "api", 0), statusInputs{Tags: tags, Down: map[string]bool{"n1": true}}, core.StateOutage},
		{"maintenance", svcTag("a", "api", 0), statusInputs{Tags: tags, Maint: map[string]bool{"n1": true}}, core.StateMaintenance},
		{"warning beats maintenance", svcTag("a", "api", 0),
			statusInputs{Tags: tags, Maint: map[string]bool{"n1": true}, Signals: []statusSignal{{Node: "n1", Key: "cpu", Severity: "warning"}}}, core.StateDegraded},
		{"container target matches only its key",
			core.StatusService{ID: "a", Name: "a", Targets: []core.StatusTarget{{Kind: core.TargetContainer, Value: "pg"}}},
			statusInputs{Signals: []statusSignal{{Key: "cpu", Severity: "critical"}, {Key: "docker:pg", Severity: "warning"}}}, core.StateDegraded},
		{"unit target", core.StatusService{ID: "a", Name: "a", Targets: []core.StatusTarget{{Kind: core.TargetUnit, Value: "nginx.service"}}},
			statusInputs{Signals: []statusSignal{{Key: "service:nginx.service", Severity: "critical"}}}, core.StateOutage},
		{"mount target", core.StatusService{ID: "a", Name: "a", Targets: []core.StatusTarget{{Kind: core.TargetMount, Value: "/data"}}},
			statusInputs{Signals: []statusSignal{{Key: "disk:/", Severity: "critical"}}}, core.StateOperational},
		{"host target sees any local alert", core.StatusService{ID: "a", Name: "a", Targets: []core.StatusTarget{{Kind: core.TargetHost}}},
			statusInputs{Signals: []statusSignal{{Key: "mem", Severity: "warning"}}}, core.StateDegraded},
		{"container on down node is outage",
			core.StatusService{ID: "a", Name: "a", Targets: []core.StatusTarget{{Kind: core.TargetContainer, Node: "n1", Value: "pg"}}},
			statusInputs{Down: map[string]bool{"n1": true}}, core.StateOutage},
		{"unknown target gives no signal",
			core.StatusService{ID: "a", Name: "a", Targets: []core.StatusTarget{{Kind: core.TargetContainer, Value: "gone"}}},
			statusInputs{Known: func(core.StatusTarget) bool { return false }, Signals: []statusSignal{{Key: "docker:gone", Severity: "critical"}}}, core.StateOperational},
	}
	for _, c := range cases {
		got, _, _ := computeServiceState(c.svc, c.in)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestComputeServiceStateReportsMissing(t *testing.T) {
	svc := core.StatusService{ID: "a", Name: "a", Targets: []core.StatusTarget{{Kind: core.TargetContainer, Value: "gone"}}}
	_, _, missing := computeServiceState(svc, statusInputs{Known: func(core.StatusTarget) bool { return false }})
	if len(missing) != 1 {
		t.Fatalf("missing=%v", missing)
	}
}

func runEval(svcs []core.StatusService, st map[string]*serviceRuntimeState, now time.Time, sig ...statusSignal) []stateChange {
	ch, _ := evaluateServices(svcs, st, statusInputs{Now: now, Tags: map[string][]string{"n1": {"api"}}, Signals: sig})
	return ch
}

func TestEvalNewServiceStartsOperational(t *testing.T) {
	st := map[string]*serviceRuntimeState{}
	svcs := []core.StatusService{svcTag("api", "api", 60)}
	if ch := runEval(svcs, st, evalT0); len(ch) != 0 {
		t.Fatalf("changes on first eval: %+v", ch)
	}
	if st["api"].State != core.StateOperational {
		t.Fatalf("state %q", st["api"].State)
	}
}

func TestEvalHoldDownBothDirections(t *testing.T) {
	st := map[string]*serviceRuntimeState{}
	svcs := []core.StatusService{svcTag("api", "api", 60)}
	warn := statusSignal{Node: "n1", Key: "cpu", Severity: "warning"}
	runEval(svcs, st, evalT0)
	if ch := runEval(svcs, st, evalT0.Add(10*time.Second), warn); len(ch) != 0 {
		t.Fatal("changed before hold-down")
	}
	if ch := runEval(svcs, st, evalT0.Add(69*time.Second), warn); len(ch) != 0 {
		t.Fatal("changed at 59s")
	}
	ch := runEval(svcs, st, evalT0.Add(70*time.Second), warn)
	if len(ch) != 1 || ch[0].To != core.StateDegraded || ch[0].From != core.StateOperational {
		t.Fatalf("want operational->degraded, got %+v", ch)
	}
	if ch := runEval(svcs, st, evalT0.Add(100*time.Second)); len(ch) != 0 {
		t.Fatal("recovered before hold-down")
	}
	ch = runEval(svcs, st, evalT0.Add(160*time.Second))
	if len(ch) != 1 || ch[0].To != core.StateOperational {
		t.Fatalf("want recovery, got %+v", ch)
	}
}

func TestEvalFlapInsideHoldDownNeverChanges(t *testing.T) {
	st := map[string]*serviceRuntimeState{}
	svcs := []core.StatusService{svcTag("api", "api", 60)}
	warn := statusSignal{Node: "n1", Key: "cpu", Severity: "warning"}
	runEval(svcs, st, evalT0)
	for i := 1; i <= 20; i++ {
		var ch []stateChange
		if i%2 == 1 {
			ch = runEval(svcs, st, evalT0.Add(time.Duration(i*30)*time.Second), warn)
		} else {
			ch = runEval(svcs, st, evalT0.Add(time.Duration(i*30)*time.Second))
		}
		if len(ch) != 0 {
			t.Fatalf("flap tick %d changed state: %+v", i, ch)
		}
	}
}

func TestEvalWorseOverridesPendingAndRestartsTimer(t *testing.T) {
	st := map[string]*serviceRuntimeState{}
	svcs := []core.StatusService{svcTag("api", "api", 60)}
	runEval(svcs, st, evalT0)
	runEval(svcs, st, evalT0.Add(10*time.Second), statusSignal{Node: "n1", Key: "cpu", Severity: "warning"})
	runEval(svcs, st, evalT0.Add(50*time.Second), statusSignal{Node: "n1", Key: "cpu", Severity: "critical"})
	if ch := runEval(svcs, st, evalT0.Add(80*time.Second), statusSignal{Node: "n1", Key: "cpu", Severity: "critical"}); len(ch) != 0 {
		t.Fatal("outage applied before its own hold-down")
	}
	ch := runEval(svcs, st, evalT0.Add(110*time.Second), statusSignal{Node: "n1", Key: "cpu", Severity: "critical"})
	if len(ch) != 1 || ch[0].To != core.StateOutage {
		t.Fatalf("got %+v", ch)
	}
}

func TestEvalMaintenanceAppliesImmediately(t *testing.T) {
	st := map[string]*serviceRuntimeState{}
	svcs := []core.StatusService{svcTag("api", "api", 600)}
	runEval(svcs, st, evalT0)
	ch, _ := evaluateServices(svcs, st, statusInputs{Now: evalT0.Add(time.Second), Tags: map[string][]string{"n1": {"api"}}, Maint: map[string]bool{"n1": true}})
	if len(ch) != 1 || ch[0].To != core.StateMaintenance {
		t.Fatalf("got %+v", ch)
	}
}

func TestEvalHoldDownSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	d := loadStatusPage(dir, t.Logf)
	d.Services = []core.StatusService{svcTag("api", "api", 60)}
	warn := statusSignal{Node: "n1", Key: "cpu", Severity: "warning"}
	runEval(d.Services, d.State, evalT0)
	runEval(d.Services, d.State, evalT0.Add(10*time.Second), warn)
	if err := d.save(dir); err != nil {
		t.Fatal(err)
	}
	d2 := loadStatusPage(dir, t.Logf) // "restart"
	ch := runEval(d2.Services, d2.State, evalT0.Add(71*time.Second), warn)
	if len(ch) != 1 || ch[0].To != core.StateDegraded {
		t.Fatalf("hold-down did not continue across restart: %+v", ch)
	}
}

func TestEvalRecordsWorstDailyHistory(t *testing.T) {
	st := map[string]*serviceRuntimeState{}
	svcs := []core.StatusService{svcTag("api", "api", 0)}
	runEval(svcs, st, evalT0, statusSignal{Node: "n1", Key: "cpu", Severity: "critical"})
	runEval(svcs, st, evalT0.Add(time.Hour))
	if got := st["api"].History["2026-10-08"]; got != core.StateOutage {
		t.Fatalf("history %q, want outage", got)
	}
}
