package main

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

func testView() core.DashboardView {
	return core.DashboardView{
		TS: 1700000000, CPU: 12.5, MemPct: 40, SwapPct: 0, Load1: 0.5, Load5: 0.25, Load15: 0.125, TempC: 55.5,
		ContainersRunning: 2, ContainersTotal: 3,
		Disks: []core.DiskView{
			{Mount: "/", UsagePct: 61.5, FreeBytes: 1000, SizeBytes: 4000},
			{Mount: "/data", UsagePct: 10, FreeBytes: 9000, SizeBytes: 10000},
		},
		NetIfaces: []core.NetIfaceView{{Name: "eth0", RxBps: 100, TxBps: 50}},
		TopCPUContainers: []core.ContainerView{
			{Name: "web", State: "running", CPUPct: 3, MemMiB: 120},
			{Name: "db", State: "running", CPUPct: 1, MemMiB: 512},
		},
		TopMemContainers: []core.ContainerView{
			{Name: "db", State: "running", CPUPct: 1, MemMiB: 512},
		},
	}
}

func render(t *testing.T, v core.DashboardView, nodes []core.NodeSummary, fleet bool) string {
	t.Helper()
	var b bytes.Buffer
	if err := writeExposition(&b, buildFamilies(v, nodes, fleet)); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

const goldenHost = `# HELP trinetra_snapshot_timestamp_seconds Unix time of the snapshot the values come from.
# TYPE trinetra_snapshot_timestamp_seconds gauge
trinetra_snapshot_timestamp_seconds 1700000000
# HELP trinetra_cpu_usage_percent CPU utilisation in percent.
# TYPE trinetra_cpu_usage_percent gauge
trinetra_cpu_usage_percent 12.5
# HELP trinetra_memory_usage_percent Memory utilisation in percent.
# TYPE trinetra_memory_usage_percent gauge
trinetra_memory_usage_percent 40
# HELP trinetra_swap_usage_percent Swap utilisation in percent.
# TYPE trinetra_swap_usage_percent gauge
trinetra_swap_usage_percent 0
# HELP trinetra_load1 Load average over 1 minute.
# TYPE trinetra_load1 gauge
trinetra_load1 0.5
# HELP trinetra_load5 Load average over 5 minutes.
# TYPE trinetra_load5 gauge
trinetra_load5 0.25
# HELP trinetra_load15 Load average over 15 minutes.
# TYPE trinetra_load15 gauge
trinetra_load15 0.125
# HELP trinetra_temperature_celsius Hottest thermal zone in degrees Celsius.
# TYPE trinetra_temperature_celsius gauge
trinetra_temperature_celsius 55.5
# HELP trinetra_disk_usage_percent Filesystem utilisation in percent.
# TYPE trinetra_disk_usage_percent gauge
trinetra_disk_usage_percent{mount="/"} 61.5
trinetra_disk_usage_percent{mount="/data"} 10
# HELP trinetra_disk_free_bytes Filesystem free space in bytes.
# TYPE trinetra_disk_free_bytes gauge
trinetra_disk_free_bytes{mount="/"} 1000
trinetra_disk_free_bytes{mount="/data"} 9000
# HELP trinetra_disk_size_bytes Filesystem size in bytes.
# TYPE trinetra_disk_size_bytes gauge
trinetra_disk_size_bytes{mount="/"} 4000
trinetra_disk_size_bytes{mount="/data"} 10000
# HELP trinetra_network_receive_bytes_per_second Receive throughput per interface.
# TYPE trinetra_network_receive_bytes_per_second gauge
trinetra_network_receive_bytes_per_second{interface="eth0"} 100
# HELP trinetra_network_transmit_bytes_per_second Transmit throughput per interface.
# TYPE trinetra_network_transmit_bytes_per_second gauge
trinetra_network_transmit_bytes_per_second{interface="eth0"} 50
# HELP trinetra_containers_running Running containers.
# TYPE trinetra_containers_running gauge
trinetra_containers_running 2
# HELP trinetra_containers_total Containers known to Docker.
# TYPE trinetra_containers_total gauge
trinetra_containers_total 3
# HELP trinetra_container_cpu_usage_percent Container CPU utilisation in percent.
# TYPE trinetra_container_cpu_usage_percent gauge
trinetra_container_cpu_usage_percent{name="db"} 1
trinetra_container_cpu_usage_percent{name="web"} 3
# HELP trinetra_container_memory_mebibytes Container memory use in MiB.
# TYPE trinetra_container_memory_mebibytes gauge
trinetra_container_memory_mebibytes{name="db"} 512
trinetra_container_memory_mebibytes{name="web"} 120
`

func TestExpositionGolden(t *testing.T) {
	if got := render(t, testView(), nil, false); got != goldenHost {
		t.Fatalf("exposition mismatch\n--- got ---\n%s\n--- want ---\n%s", got, goldenHost)
	}
}

func TestExpositionOmitsMissingSensors(t *testing.T) {
	got := render(t, core.DashboardView{TS: 1, CPU: 1}, nil, false)
	for _, absent := range []string{"temperature", "disk", "network", "container"} {
		if strings.Contains(got, absent) {
			t.Errorf("output contains %q for an empty view:\n%s", absent, got)
		}
	}
}

func TestExpositionEscapesLabels(t *testing.T) {
	v := core.DashboardView{Disks: []core.DiskView{{Mount: "/a\\b\"c\nd", UsagePct: 1}}}
	want := `trinetra_disk_usage_percent{mount="/a\\b\"c\nd"} 1`
	if got := render(t, v, nil, false); !strings.Contains(got, want+"\n") {
		t.Fatalf("missing %q in\n%s", want, got)
	}
}

func TestExpositionNaNAndInf(t *testing.T) {
	v := core.DashboardView{CPU: math.NaN(), MemPct: math.Inf(1), SwapPct: math.Inf(-1)}
	got := render(t, v, nil, false)
	for _, want := range []string{"trinetra_cpu_usage_percent NaN\n", "trinetra_memory_usage_percent +Inf\n", "trinetra_swap_usage_percent -Inf\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestExpositionFleet(t *testing.T) {
	nodes := []core.NodeSummary{
		{ID: "n1", Name: "web-1", State: "online", CPU: 7},
		{ID: "n2", Name: "db\"1", State: "down"},
	}
	got := render(t, core.DashboardView{}, nodes, true)
	for _, want := range []string{
		`trinetra_node_up{node="web-1",id="n1"} 1`,
		`trinetra_node_up{node="db\"1",id="n2"} 0`,
		`trinetra_node_cpu_usage_percent{node="web-1",id="n1"} 7`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if off := render(t, core.DashboardView{}, nodes, false); strings.Contains(off, "trinetra_node_") {
		t.Errorf("fleet metrics present with include_fleet off:\n%s", off)
	}
}
