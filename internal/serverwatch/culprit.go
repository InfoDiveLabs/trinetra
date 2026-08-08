// Package serverwatch: culprit.go enriches CPU and memory alerts by naming the
// top process and/or container consuming that resource (#103), so a "cpu high"
// alert reads "cpu = 96.0 >= threshold 95.0 (top: ffmpeg 82%, container web
// 30%)" instead of a bare number. It reads only the process/container data
// already on the snapshot (Snapshot.Processes.Top, Snapshot.ContainerStats), so
// it adds no new collection and degrades to no suffix when that data is absent
// (collect.processes off, no docker, or nothing consuming the resource).
package serverwatch

import (
	"fmt"
	"strings"
)

// cpuCulprit returns a " (top: ...)" suffix naming the biggest CPU consumers in
// snap, or "" when there is nothing worth naming.
func cpuCulprit(snap Snapshot) string {
	var parts []string
	if p, ok := topProcByCPU(snap.Processes.Top); ok {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", p.Name, p.CPUPct))
	}
	if c, ok := topContainerByCPU(snap.ContainerStats); ok {
		parts = append(parts, fmt.Sprintf("container %s %.0f%%", c.Name, c.CPUPct))
	}
	return culpritSuffix(parts)
}

// memCulprit returns a " (top: ...)" suffix naming the biggest memory consumers
// in snap, or "" when there is nothing worth naming.
func memCulprit(snap Snapshot) string {
	var parts []string
	if p, ok := topProcByMem(snap.Processes.Top); ok {
		parts = append(parts, fmt.Sprintf("%s %.0f MiB", p.Name, p.MemMiB))
	}
	if c, ok := topContainerByMem(snap.ContainerStats); ok {
		parts = append(parts, fmt.Sprintf("container %s %.0f MiB", c.Name, c.MemMiB))
	}
	return culpritSuffix(parts)
}

func culpritSuffix(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return " (top: " + strings.Join(parts, ", ") + ")"
}

func topProcByCPU(top []ProcInfo) (ProcInfo, bool) {
	best, found := ProcInfo{}, false
	for _, p := range top {
		if p.CPUPct > best.CPUPct {
			best, found = p, true
		}
	}
	if !found || best.CPUPct <= 0 {
		return ProcInfo{}, false
	}
	return best, true
}

func topProcByMem(top []ProcInfo) (ProcInfo, bool) {
	best, found := ProcInfo{}, false
	for _, p := range top {
		if p.MemMiB > best.MemMiB {
			best, found = p, true
		}
	}
	if !found || best.MemMiB <= 0 {
		return ProcInfo{}, false
	}
	return best, true
}

func topContainerByCPU(cs map[string]ContainerStat) (ContainerStat, bool) {
	best, found := ContainerStat{}, false
	for _, c := range cs {
		if c.CPUPct > best.CPUPct {
			best, found = c, true
		}
	}
	if !found || best.CPUPct <= 0 {
		return ContainerStat{}, false
	}
	return best, true
}

func topContainerByMem(cs map[string]ContainerStat) (ContainerStat, bool) {
	best, found := ContainerStat{}, false
	for _, c := range cs {
		if c.MemMiB > best.MemMiB {
			best, found = c, true
		}
	}
	if !found || best.MemMiB <= 0 {
		return ContainerStat{}, false
	}
	return best, true
}
