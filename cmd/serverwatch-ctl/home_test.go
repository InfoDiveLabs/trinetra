package main

import (
	"strings"
	"testing"

	"serverwatch/internal/core"
)

// homeFixture builds a loaded Home model (not loading, no error) with a
// representative snapshot so the render tests exercise the real layout.
func homeFixture() model {
	return model{
		step:    stepHome,
		loading: false,
		snap: core.DashboardView{
			TS:                1_700_000_000,
			Online:            true,
			CPU:               77.7,
			Cores:             4,
			MemPct:            41.2,
			SwapPct:           3.0,
			Load1:             1.5, Load5: 1.4, Load15: 1.3,
			TempC:             54.0,
			Processes:         core.ProcessCounts{Total: 3728, Running: 2, Zombie: 1},
			ContainersRunning: 12, ContainersTotal: 48,
			UnitsFailed: 0, UnitsTotal: 212,
			Disks:         []core.DiskView{{}, {}, {}},
			DisksCritical: 1,
		},
		cpuHist: []float64{10, 20, 40, 30, 60, 80, 77.7},
	}
}

// TestHomeViewRendersDashboard: the redesigned Home shows the online state,
// the SYSTEM and ALERTS panels, the CPU value, and the inventory counts.
func TestHomeViewRendersDashboard(t *testing.T) {
	m := homeFixture()
	m.alerts = []core.AlertRecord{
		{Key: "docker:web.1.abcdef", Severity: "critical", Source: "docker"},
	}
	out := m.homeView()
	for _, want := range []string{"online", "SYSTEM", "ALERTS", "CPU", "77", "docker:web", "12", "212"} {
		if !strings.Contains(out, want) {
			t.Errorf("homeView missing %q\n---\n%s", want, out)
		}
	}
}

// TestHomeViewFiringCount: with alerts present the ALERTS panel shows the
// firing count.
func TestHomeViewFiringCount(t *testing.T) {
	m := homeFixture()
	m.alerts = []core.AlertRecord{
		{Key: "a", Severity: "critical"}, {Key: "b", Severity: "warning"},
	}
	out := m.homeView()
	if !strings.Contains(out, "2") || !strings.Contains(strings.ToLower(out), "firing") {
		t.Errorf("expected firing count in ALERTS panel:\n%s", out)
	}
}

// TestHomeViewNoAlerts: an empty alert set renders the reassuring empty state
// rather than an empty panel.
func TestHomeViewNoAlerts(t *testing.T) {
	m := homeFixture()
	m.alerts = nil
	out := m.homeView()
	if !strings.Contains(strings.ToLower(out), "no active alerts") {
		t.Errorf("expected 'no active alerts' empty state:\n%s", out)
	}
}

// TestHomeViewOffline: an offline snapshot renders the offline state.
func TestHomeViewOffline(t *testing.T) {
	m := homeFixture()
	m.snap.Online = false
	if !strings.Contains(m.homeView(), "offline") {
		t.Errorf("expected offline state")
	}
}

// TestSnapshotMsgAppendsCPUHistory: each snapshot pushes its CPU onto the
// rolling history (bounded to cpuHistCap) so the sparkline stays live and
// never grows without bound.
func TestSnapshotMsgAppendsCPUHistory(t *testing.T) {
	// feed cpuHistCap+5 snapshots
	cur := model{step: stepHome}
	for i := 0; i < cpuHistCap+5; i++ {
		next, _ := cur.Update(snapshotMsg{view: core.DashboardView{CPU: float64(i)}})
		cur = next.(model)
	}
	if len(cur.cpuHist) != cpuHistCap {
		t.Errorf("cpuHist len = %d, want capped at %d", len(cur.cpuHist), cpuHistCap)
	}
	// the last sample must be the most recent CPU value
	last := cur.cpuHist[len(cur.cpuHist)-1]
	if last != float64(cpuHistCap+4) {
		t.Errorf("last cpuHist = %v, want %v (most recent)", last, cpuHistCap+4)
	}
}

// TestAlertsMsgPopulatesModel: an alertsMsg lands in the model so the next
// Home render reflects it.
func TestAlertsMsgPopulatesModel(t *testing.T) {
	m := model{step: stepHome}
	next, _ := m.Update(alertsMsg{alerts: []core.AlertRecord{{Key: "x"}}})
	got := next.(model)
	if len(got.alerts) != 1 || got.alerts[0].Key != "x" {
		t.Errorf("alertsMsg not stored on model: %+v", got.alerts)
	}
}
