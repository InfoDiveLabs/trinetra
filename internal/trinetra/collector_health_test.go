package trinetra

import (
	"errors"
	"strings"
	"testing"
)

// TestCollectorHealthAlertsAfterThresholdAndRecovers pins #110's alerting contract.
func TestCollectorHealthAlertsAfterThresholdAndRecovers(t *testing.T) {
	h := newCollectorHealth()
	attempted := map[string]bool{"docker": true}
	fail := map[string]string{"docker": "docker daemon not responding"}

	firing := func() bool {
		checks := buildCollectorChecks(snapshotWith(h), 60)
		for _, c := range checks {
			if c.Key == "collector:docker" {
				got, _ := c.breach(nil, 3, 0.15, false)
				return got
			}
		}
		t.Fatal("no collector:docker check built")
		return false
	}

	// Below threshold: 1-2 failures carry forward silently, no alert.
	h.observe(attempted, fail, 10)
	if firing() {
		t.Error("collector alert fired after 1 failure, want silent below threshold")
	}
	h.observe(attempted, fail, 20)
	if firing() {
		t.Error("collector alert fired after 2 failures, want silent below threshold")
	}
	// Third consecutive failure crosses the threshold -> fire, with a message
	// naming the collector and last error.
	h.observe(attempted, fail, 30)
	if !firing() {
		t.Error("collector alert did not fire after 3 consecutive failures")
	}
	checks := buildCollectorChecks(snapshotWith(h), 60)
	for _, c := range checks {
		if c.Key == "collector:docker" {
			_, msg := c.breach(nil, 3, 0.15, false)
			if !strings.Contains(msg, "docker") || !strings.Contains(msg, "not responding") {
				t.Errorf("fire message = %q, want it to name the collector and error", msg)
			}
		}
	}

	// A success resets the streak -> the alert recovers.
	h.observe(attempted, nil, 40)
	if firing() {
		t.Error("collector alert still firing after a success, want recovery")
	}
	if st := h.stats["docker"]; st.Fails != 0 || st.LastSuccessUnix != 40 || st.LastError != "" {
		t.Errorf("after success stats = %+v, want fails=0 lastSuccess=40 no error", st)
	}
}

// snapshotWith wraps a health tracker's current state in a Snapshot for
// buildCollectorChecks.
func snapshotWith(h *collectorHealth) Snapshot {
	return Snapshot{CollectorHealth: h.snapshot()}
}

// TestCarryForwardFailedCollectors pins #110's carry-forward.
func TestCarryForwardFailedCollectors(t *testing.T) {
	prev := Snapshot{
		Containers:     map[string]string{"web": "running", "db": "running"},
		ContainerStats: map[string]ContainerStat{"web": {Name: "web", CPUPct: 5}},
	}
	// This cycle's docker collection errored, so Containers came back nil.
	cur := Snapshot{CollectorErrors: map[string]string{"docker": "timeout"}}
	carryForwardFailedCollectors(&prev, &cur)

	if len(cur.Containers) != 2 || cur.Containers["web"] != "running" {
		t.Errorf("containers not carried forward: %+v", cur.Containers)
	}
	if cur.ContainerStats["web"].CPUPct != 5 {
		t.Errorf("container stats not carried forward: %+v", cur.ContainerStats)
	}
	if !cur.SlowStale {
		t.Error("carried-forward snapshot must be marked SlowStale")
	}

	// A cycle with no collector errors carries nothing forward and stays fresh.
	fresh := Snapshot{Containers: map[string]string{"only": "running"}}
	carryForwardFailedCollectors(&prev, &fresh)
	if fresh.SlowStale || len(fresh.Containers) != 1 {
		t.Errorf("clean cycle should not carry forward or mark stale: %+v stale=%v", fresh.Containers, fresh.SlowStale)
	}
}

// TestCollectSlowRecordsCollectorError pins that a failed slow-tier command is recorded in
// CollectorErrors (attempted-and-failed).
func TestCollectSlowRecordsCollectorError(t *testing.T) {
	// docker is "available" but `docker ps -a` fails this cycle.
	da := dockerAccess{available: true, method: "socket"}
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "docker" {
			return nil, errors.New("Cannot connect to the Docker daemon")
		}
		return nil, errNotExist // df/systemctl/smartctl all "fail" too here
	}}
	snap := collectSlow(x, fakeFS{}, da, nil, nil, 100, nil, 0)

	if snap.CollectorErrors["docker"] == "" {
		t.Errorf("docker error not recorded: CollectorErrors=%v", snap.CollectorErrors)
	}
	if !snap.collectorsAttempted["docker"] || !snap.collectorsAttempted["disk"] {
		t.Errorf("attempted set wrong: %v", snap.collectorsAttempted)
	}
	// A failed docker ps must not fabricate an empty container set.
	if snap.Containers != nil {
		t.Errorf("failed docker collection should leave Containers nil (carry-forward decides), got %+v", snap.Containers)
	}
}
