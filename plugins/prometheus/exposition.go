package main

import (
	"bufio"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

type sample struct {
	labels []string
	value  float64
}

type family struct {
	name, help string
	samples    []sample
}

func (f *family) add(v float64, labels ...string) {
	f.samples = append(f.samples, sample{labels: labels, value: v})
}

func scalar(name, help string, v float64) family {
	return family{name: name, help: help, samples: []sample{{value: v}}}
}

func buildFamilies(v core.DashboardView, nodes []core.NodeSummary, includeFleet bool) []family {
	fams := []family{
		scalar("trinetra_snapshot_timestamp_seconds", "Unix time of the snapshot the values come from.", float64(v.TS)),
		scalar("trinetra_cpu_usage_percent", "CPU utilisation in percent.", v.CPU),
		scalar("trinetra_memory_usage_percent", "Memory utilisation in percent.", v.MemPct),
		scalar("trinetra_swap_usage_percent", "Swap utilisation in percent.", v.SwapPct),
		scalar("trinetra_load1", "Load average over 1 minute.", v.Load1),
		scalar("trinetra_load5", "Load average over 5 minutes.", v.Load5),
		scalar("trinetra_load15", "Load average over 15 minutes.", v.Load15),
	}
	if v.TempC != 0 {
		fams = append(fams, scalar("trinetra_temperature_celsius", "Hottest thermal zone in degrees Celsius.", v.TempC))
	}
	if len(v.Disks) > 0 {
		use := family{name: "trinetra_disk_usage_percent", help: "Filesystem utilisation in percent."}
		free := family{name: "trinetra_disk_free_bytes", help: "Filesystem free space in bytes."}
		size := family{name: "trinetra_disk_size_bytes", help: "Filesystem size in bytes."}
		for _, d := range v.Disks {
			use.add(d.UsagePct, "mount", d.Mount)
			free.add(float64(d.FreeBytes), "mount", d.Mount)
			size.add(float64(d.SizeBytes), "mount", d.Mount)
		}
		fams = append(fams, use, free, size)
	}
	if len(v.NetIfaces) > 0 {
		rx := family{name: "trinetra_network_receive_bytes_per_second", help: "Receive throughput per interface."}
		tx := family{name: "trinetra_network_transmit_bytes_per_second", help: "Transmit throughput per interface."}
		for _, n := range v.NetIfaces {
			rx.add(n.RxBps, "interface", n.Name)
			tx.add(n.TxBps, "interface", n.Name)
		}
		fams = append(fams, rx, tx)
	}
	if v.ContainersTotal > 0 {
		fams = append(fams,
			scalar("trinetra_containers_running", "Running containers.", float64(v.ContainersRunning)),
			scalar("trinetra_containers_total", "Containers known to Docker.", float64(v.ContainersTotal)))
	}
	fams = append(fams, containerFamilies(v)...)
	if includeFleet {
		fams = append(fams, fleetFamilies(nodes)...)
	}
	return fams
}

func containerFamilies(v core.DashboardView) []family {
	byName := map[string]core.ContainerView{}
	for _, list := range [][]core.ContainerView{v.TopMemContainers, v.TopCPUContainers} {
		for _, c := range list {
			byName[c.Name] = c
		}
	}
	if len(byName) == 0 {
		return nil
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	cpu := family{name: "trinetra_container_cpu_usage_percent", help: "Container CPU utilisation in percent."}
	mem := family{name: "trinetra_container_memory_mebibytes", help: "Container memory use in MiB."}
	for _, n := range names {
		cpu.add(byName[n].CPUPct, "name", n)
		mem.add(byName[n].MemMiB, "name", n)
	}
	return []family{cpu, mem}
}

func fleetFamilies(nodes []core.NodeSummary) []family {
	if len(nodes) == 0 {
		return nil
	}
	up := family{name: "trinetra_node_up", help: "1 when the fleet node is online."}
	cpu := family{name: "trinetra_node_cpu_usage_percent", help: "Fleet node CPU utilisation in percent."}
	mem := family{name: "trinetra_node_memory_usage_percent", help: "Fleet node memory utilisation in percent."}
	disk := family{name: "trinetra_node_worst_disk_usage_percent", help: "Fullest filesystem on the fleet node in percent."}
	for _, n := range nodes {
		l := []string{"node", n.Name, "id", n.ID}
		var u float64
		if n.State == "online" {
			u = 1
		}
		up.add(u, l...)
		cpu.add(n.CPU, l...)
		mem.add(n.MemPct, l...)
		disk.add(n.WorstDiskPct, l...)
	}
	return []family{up, cpu, mem, disk}
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func writeExposition(w io.Writer, fams []family) error {
	bw := bufio.NewWriter(w)
	for _, f := range fams {
		bw.WriteString("# HELP " + f.name + " " + helpEscaper.Replace(f.help) + "\n")
		bw.WriteString("# TYPE " + f.name + " gauge\n")
		for _, s := range f.samples {
			bw.WriteString(f.name)
			if len(s.labels) > 0 {
				bw.WriteByte('{')
				for i := 0; i+1 < len(s.labels); i += 2 {
					if i > 0 {
						bw.WriteByte(',')
					}
					bw.WriteString(s.labels[i] + `="` + labelEscaper.Replace(s.labels[i+1]) + `"`)
				}
				bw.WriteByte('}')
			}
			bw.WriteString(" " + formatValue(s.value) + "\n")
		}
	}
	return bw.Flush()
}
