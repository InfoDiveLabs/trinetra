package trinetra

import (
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestCPUAndMemCulprit(t *testing.T) {
	snap := Snapshot{
		Processes: ProcSnapshot{Top: []ProcInfo{
			{Name: "ffmpeg", CPUPct: 82.3, MemMiB: 120},
			{Name: "chrome", CPUPct: 15, MemMiB: 900},
		}},
		ContainerStats: map[string]ContainerStat{
			"web": {Name: "web", CPUPct: 30.5, MemMiB: 512},
			"db":  {Name: "db", CPUPct: 2, MemMiB: 2048},
		},
	}
	// CPU picks the biggest CPU consumers.
	if got := cpuCulprit(snap); !strings.Contains(got, "ffmpeg 82%") || !strings.Contains(got, "container web 30%") {
		t.Errorf("cpuCulprit = %q", got)
	}
	// Memory picks the biggest memory consumers (different winners than CPU).
	if got := memCulprit(snap); !strings.Contains(got, "chrome 900 MiB") || !strings.Contains(got, "container db 2048 MiB") {
		t.Errorf("memCulprit = %q", got)
	}
	// No process/container data -> no suffix.
	if got := cpuCulprit(Snapshot{}); got != "" {
		t.Errorf("cpuCulprit(empty) = %q, want empty", got)
	}
}

func TestBuildFastChecksNamesCulpritInFireMsg(t *testing.T) {
	c := config.Default() // Thresholds.CPUPct = 95
	snap := Snapshot{
		CPU:       96,
		Processes: ProcSnapshot{Top: []ProcInfo{{Name: "ffmpeg", CPUPct: 82}}},
	}
	checks := buildFastChecks(snap, c)
	var cpu *Check
	for i := range checks {
		if checks[i].Key == "cpu" {
			cpu = &checks[i]
		}
	}
	if cpu == nil {
		t.Fatal("no cpu check built")
	}
	if !strings.Contains(cpu.FireMsg, "ffmpeg 82%") {
		t.Errorf("cpu FireMsg = %q, want the culprit named", cpu.FireMsg)
	}
	// And the breach reason carries it through.
	fired, msg := cpu.breach(nil, 3, 0.15, false)
	if !fired || !strings.Contains(msg, "≥ threshold 95.0") || !strings.Contains(msg, "ffmpeg 82%") {
		t.Errorf("breach = %v, %q; want a firing message naming the culprit", fired, msg)
	}
}
