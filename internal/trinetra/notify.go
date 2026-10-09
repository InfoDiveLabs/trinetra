package trinetra

import (
	"fmt"
	"strings"
	"time"
)

func humanDur(sec int64) string {
	d := time.Duration(sec) * time.Second
	if d < time.Minute {
		return fmt.Sprintf("%ds", sec)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

func formatFire(e Event) string {
	prefix := "⚠️"
	if e.Critical {
		prefix = "🚨"
	}
	return fmt.Sprintf("%s ALERT: %s", prefix, e.Text)
}

// formatAlert renders a channel-agnostic Alert as a plain, phone-friendly message: a
// severity marker, the Title, then the Body on its own line.
func formatAlert(a Alert) string {
	return fmt.Sprintf("%s %s\n%s", alertMarker(a), a.Title, a.Body)
}

func alertMarker(a Alert) string {
	if a.Kind == "recover" {
		return "✅"
	}
	switch a.Severity {
	case SevCritical:
		return "🚨"
	case SevWarning:
		return "⚠️"
	default:
		return "ℹ️"
	}
}

func formatBootReport(evs []DownEvent, snap string) string {
	var b strings.Builder
	b.WriteString("🔌 trinetra back online")
	for _, e := range evs {
		if e.Type == "power_down" {
			start := time.Unix(e.Start, 0).Format("15:04")
			end := time.Unix(e.End, 0).Format("15:04")
			fmt.Fprintf(&b, "\nwas down %s→%s (%s)", start, end, humanDur(e.DurationSec))
		}
	}
	if snap != "" {
		b.WriteString("\n\n" + snap)
	}
	return b.String()
}

func formatDowntimeList(evs []DownEvent) string {
	if len(evs) == 0 {
		return "no downtime recorded in window ✅"
	}
	var b strings.Builder
	b.WriteString("downtime events:")
	for _, e := range evs {
		start := time.Unix(e.Start, 0).Format("01-02 15:04")
		fmt.Fprintf(&b, "\n• %s  %s  (%s)", e.Type, start, humanDur(e.DurationSec))
	}
	return b.String()
}
