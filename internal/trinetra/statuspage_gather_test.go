package trinetra

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// downFleet registers n nodes, and makes them all go down. With recent=true
// they were last seen just before going down (a mass disconnect); otherwise
// long ago (individually down).
func downFleet(t *testing.T, n int, recent bool) (*masterState, []string, time.Time) {
	t.Helper()
	m := newTestMasterState(t)
	base := evalT0
	var ids []string
	for i := 0; i < n; i++ {
		id, err := fleet.NewNodeID()
		if err != nil {
			t.Fatal(err)
		}
		if err := m.reg.Add(fleet.Node{ID: id, Name: "n" + id[:4], Joined: base.Unix(), Tags: []string{"web"}}); err != nil {
			t.Fatal(err)
		}
		m.tracker.Seen(id, base.Unix(), 0)
		ids = append(ids, id)
	}
	m.tracker.Evaluate(base.Unix())
	now := base.Add(10 * time.Minute)
	if recent {
		now = base.Add(100 * time.Second)
	}
	ev := m.tracker.Evaluate(now.Unix())
	m.loop.mu.Lock()
	m.loop.alerter.Plan(ev, now.Unix(), func(id string) string { return id })
	m.loop.mu.Unlock()
	return m, ids, now
}

func gatherDown(m *masterState, id string, now time.Time) statusInputs {
	return gatherMaster(m, map[string]ActiveAlert{}, Snapshot{}, now)
}

func newSilences(t *testing.T) *silenceStore {
	t.Helper()
	s, err := loadSilenceStore(filepath.Join(t.TempDir(), "silences.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGatherMasterNodeDownPlain(t *testing.T) {
	m, ids, now := downFleet(t, 1, false)
	if !gatherDown(m, ids[0], now).Down[ids[0]] {
		t.Fatal("individually down node should be down")
	}
}

func TestGatherMasterSilencedNodeDownNotDown(t *testing.T) {
	m, ids, now := downFleet(t, 1, false)
	m.silences = newSilences(t)
	if _, err := m.silences.Create(core.Silence{Matchers: []core.Matcher{{Rule: "fleet:node:*:down"}},
		Start: now.Unix() - 60, End: now.Unix() + 3600, Author: "a"}); err != nil {
		t.Fatal(err)
	}
	if m.silences.Suppressed(now.Unix(), "", "", nil, nodeDownKey(ids[0]), "critical") == nil {
		t.Fatal("test silence does not suppress the node-down alert")
	}
	if gatherDown(m, ids[0], now).Down[ids[0]] {
		t.Fatal("silenced node-down must not count as down")
	}
}

func TestGatherMasterMassDownNotDown(t *testing.T) {
	m, ids, now := downFleet(t, 3, true)
	if m.tracker.State(ids[0]) != fleet.StateDown {
		t.Fatalf("setup: state %q", m.tracker.State(ids[0]))
	}
	in := gatherDown(m, ids[0], now)
	for _, id := range ids {
		if in.Down[id] {
			t.Fatalf("node %s marked down during a fleet-wide connectivity drop", id)
		}
	}
}

func TestMaintenanceActiveOnlyForWholeNodeWindows(t *testing.T) {
	s := newSilences(t)
	win := func(name string, ms ...core.Matcher) {
		t.Helper()
		if _, err := s.SaveMaintenance(core.Maintenance{Name: name, Matchers: ms, Weekdays: []int{0, 1, 2, 3, 4, 5, 6},
			From: "00:00", To: "23:59", TZ: "UTC", Author: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	win("disk", core.Matcher{Tag: "web", Rule: "disk:*"})
	if maintenanceActiveForNode(s, evalT0, "n1", "n1", []string{"web"}) {
		t.Fatal("rule-scoped window counted as whole-node maintenance")
	}
	win("crit", core.Matcher{Tag: "web", Severity: "critical"})
	if maintenanceActiveForNode(s, evalT0, "n1", "n1", []string{"web"}) {
		t.Fatal("severity-scoped window counted as whole-node maintenance")
	}
	win("whole", core.Matcher{Tag: "web"})
	if !maintenanceActiveForNode(s, evalT0, "n1", "n1", []string{"web"}) {
		t.Fatal("whole-node window not counted")
	}
	if maintenanceActiveForNode(s, evalT0, "n2", "n2", []string{"db"}) {
		t.Fatal("window leaked to a node it does not match")
	}
}
