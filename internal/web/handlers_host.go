package web

import (
	"fmt"
	"html/template"
	"net/http"

	"serverwatch/internal/core"
)

// HostPageData is what templates/host.html renders against: the static host
// hardware/OS inventory (#100), fetched over the control socket via
// Deps.API.HostInfo and pre-formatted for display. It embeds PageData for the
// shared shell.
type HostPageData struct {
	PageData
	Available  bool // false when the API is unavailable or errored
	Hostname   string
	OS         string
	Kernel     string
	CPU        string // "model (N cores / M threads @ F MHz)"
	Memory     string // humanized bytes
	Uptime     string // "Nd Nh Nm"
	LocalIP    string
	PublicIP   string
	Disks      []hostDiskRow
}

type hostDiskRow struct {
	Device, Model, Kind, Size, FSType, Mount string
}

func buildHostPageData(r *http.Request, d Deps) HostPageData {
	data := HostPageData{PageData: newPageData(r, d, "Host", "Hardware and OS inventory")}
	if d.API == nil {
		return data
	}
	h, err := d.API.HostInfo()
	if err != nil {
		return data
	}
	data.Available = true
	data.Hostname = h.Hostname
	data.OS = h.OS
	data.Kernel = h.Kernel
	data.CPU = formatCPU(h)
	data.Memory = humanBytesIEC(h.MemTotalBytes)
	data.Uptime = humanUptime(h.UptimeSec)
	data.LocalIP = h.LocalIP
	data.PublicIP = h.PublicIP
	for _, disk := range h.Disks {
		kind := "SSD"
		if disk.Rotational {
			kind = "HDD"
		}
		data.Disks = append(data.Disks, hostDiskRow{
			Device: disk.Device, Model: disk.Model, Kind: kind,
			Size: humanBytesIEC(disk.SizeBytes), FSType: disk.FSType, Mount: disk.Mount,
		})
	}
	return data
}

func formatCPU(h core.HostInfoView) string {
	s := h.CPUModel
	if h.CPUThreads > 0 {
		s += fmt.Sprintf("  (%d cores / %d threads", h.CPUCores, h.CPUThreads)
		if h.CPUBaseMHz > 0 {
			s += fmt.Sprintf(" @ %.0f MHz", h.CPUBaseMHz)
		}
		s += ")"
	}
	return s
}

// humanBytesIEC renders a byte count in binary units (KiB/MiB/GiB/...).
func humanBytesIEC(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// humanUptime renders a duration in seconds as "Nd Nh Nm".
func humanUptime(sec int64) string {
	if sec <= 0 {
		return "unknown"
	}
	d := sec / 86400
	hh := (sec % 86400) / 3600
	mm := (sec % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh %dm", d, hh, mm)
	case hh > 0:
		return fmt.Sprintf("%dh %dm", hh, mm)
	case mm > 0:
		return fmt.Sprintf("%dm", mm)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

func renderHostPage(w http.ResponseWriter, data HostPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/host.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

func hostPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := renderHostPage(w, buildHostPageData(r, d)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
