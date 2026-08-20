package serverwatch

import (
	"testing"

	"serverwatch/internal/config"
)

func TestSwarmServiceName(t *testing.T) {
	cases := []struct {
		name    string
		service string
		ok      bool
	}{
		// Real replicated task from the issue: service is stable across the
		// changing task id.
		{"analyzer-frontend-reeta-wspwrb.1.khre0c0w4ryoiuq0igz5f8whu", "analyzer-frontend-reeta-wspwrb", true},
		{"web.1.abcdefghij0123456789xy", "web", true},
		{"stack_api.5.zyxwvutsrqponmlkjihgfedcb", "stack_api", true},
		// Global service: slot is a node id (alphanumeric), still a task; the
		// service is the first segment.
		{"mon.abcdef0123456789node.qwertyuiopasdfghjklzxcvb", "mon", true},
		// Plain containers: not tasks, kept as-is.
		{"postgres", "", false},
		{"my-app", "", false},
		{"my.app", "", false},                  // dotted but no task id
		{"web.1.short", "", false},              // task id too short
		{"web..khre0c0w4ryoiuq0igz5f8whu", "", false}, // empty slot
	}
	for _, c := range cases {
		svc, ok := swarmServiceName(c.name)
		if svc != c.service || ok != c.ok {
			t.Errorf("swarmServiceName(%q) = (%q,%v), want (%q,%v)", c.name, svc, ok, c.service, c.ok)
		}
	}
}

func TestCollapseSwarmTasks(t *testing.T) {
	// Two tasks of service "web" (a rolling deploy in progress: old task exited,
	// new task running) plus a plain container "db".
	containers := map[string]string{
		"web.1.aaaaaaaaaaaaaaaaaaaaaaaaa": "exited",  // old task
		"web.1.bbbbbbbbbbbbbbbbbbbbbbbbb": "running", // new task
		"db":                             "running",
	}
	stats := map[string]ContainerStat{
		"web.1.aaaaaaaaaaaaaaaaaaaaaaaaa": {Name: "web.1.aaaaaaaaaaaaaaaaaaaaaaaaa", CPUPct: 5, MemMiB: 100},
		"web.1.bbbbbbbbbbbbbbbbbbbbbbbbb": {Name: "web.1.bbbbbbbbbbbbbbbbbbbbbbbbb", CPUPct: 7, MemMiB: 150},
		"db":                             {Name: "db", CPUPct: 2, MemMiB: 50},
	}
	gotStates, gotStats := collapseSwarmTasks(containers, stats)

	// The service collapses to ONE key, "running" because a task is running
	// (rolling deploy must not report the service down).
	if len(gotStates) != 2 {
		t.Fatalf("collapsed states = %v, want 2 keys (web, db)", gotStates)
	}
	if gotStates["web"] != "running" {
		t.Errorf("web state = %q, want running (a task is up during rolling deploy)", gotStates["web"])
	}
	if gotStates["db"] != "running" {
		t.Errorf("db state = %q, want running", gotStates["db"])
	}

	// Stats SUM across the service's tasks; db passes through.
	if s := gotStats["web"]; s.CPUPct != 12 || s.MemMiB != 250 || s.Name != "web" {
		t.Errorf("web stats = %+v, want summed cpu=12 mem=250 name=web", s)
	}
	if s := gotStats["db"]; s.CPUPct != 2 || s.MemMiB != 50 {
		t.Errorf("db stats = %+v, want passthrough cpu=2 mem=50", s)
	}
}

func TestCollectSlowCollapsesSwarmTasks(t *testing.T) {
	psOut := "web.1.aaaaaaaaaaaaaaaaaaaaaaaaa\trunning\tUp 2 hours\n" +
		"web.1.bbbbbbbbbbbbbbbbbbbbbbbbb\texited\tExited (0)\n" +
		"plain\trunning\tUp 1 day\n"
	statsOut := "web.1.aaaaaaaaaaaaaaaaaaaaaaaaa\t5.0%\t100MiB / 1GiB\t1MB / 2MB\n" +
		"web.1.bbbbbbbbbbbbbbbbbbbbbbbbb\t7.0%\t150MiB / 1GiB\t1MB / 2MB\n"
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "docker" && len(args) > 0 && args[0] == "ps" {
			return []byte(psOut), nil
		}
		if name == "docker" && len(args) > 0 && args[0] == "stats" {
			return []byte(statsOut), nil
		}
		return nil, errNotExist
	}}

	// Swarm active: task names collapse to the "web" service (running, since a
	// task is up), and the plain container is untouched.
	da := dockerAccess{available: true, method: "socket", swarm: true}
	snap := collectSlow(x, fakeFS{}, da, config.Default(), nil, 100, nil, 0)
	if snap.Containers["web"] != "running" {
		t.Errorf("swarm containers = %v, want web=running", snap.Containers)
	}
	if _, taskLeaked := snap.Containers["web.1.aaaaaaaaaaaaaaaaaaaaaaaaa"]; taskLeaked {
		t.Error("raw task name leaked into containers under Swarm")
	}
	if snap.Containers["plain"] != "running" {
		t.Errorf("plain container should pass through, got %v", snap.Containers)
	}
	if s := snap.ContainerStats["web"]; s.CPUPct != 12 {
		t.Errorf("web service stats not summed: %+v, want cpu=12", s)
	}

	// Plain docker (swarm=false): raw task names are kept unchanged.
	daPlain := dockerAccess{available: true, method: "socket", swarm: false}
	plainSnap := collectSlow(x, fakeFS{}, daPlain, config.Default(), nil, 100, nil, 0)
	if _, ok := plainSnap.Containers["web.1.aaaaaaaaaaaaaaaaaaaaaaaaa"]; !ok {
		t.Errorf("non-swarm host must keep raw task names: %v", plainSnap.Containers)
	}
}

func TestCollapseSwarmTasksAllDown(t *testing.T) {
	// A service whose every task has exited must report a non-running state so
	// it still fires a down alert.
	containers := map[string]string{
		"api.1.aaaaaaaaaaaaaaaaaaaaaaaaa": "exited",
		"api.2.bbbbbbbbbbbbbbbbbbbbbbbbb": "exited",
	}
	gotStates, _ := collapseSwarmTasks(containers, nil)
	if gotStates["api"] == "running" {
		t.Errorf("all-exited service reported running, want a non-running state; got %q", gotStates["api"])
	}
}
