package serverwatch

import (
	"strings"
	"testing"
)

func TestFormatBootReport(t *testing.T) {
	evs := []DownEvent{{Type: "power_down", Start: 1000, End: 5000, DurationSec: 4000}}
	s := formatBootReport(evs, "cpu 3%")
	if !strings.Contains(s, "back online") || !strings.Contains(s, "1h6m") && !strings.Contains(s, "66m") {
		t.Fatalf("boot report = %q", s)
	}
}

func TestFormatFire(t *testing.T) {
	s := formatFire(Event{Key: "disk:/", Text: "disk:/ = 95.0 ≥ threshold 90.0"})
	if !strings.Contains(s, "disk:/") {
		t.Fatalf("fire = %q", s)
	}
}

func TestFormatAlert(t *testing.T) {
	critical := Alert{Title: "Disk almost full", Body: "disk:/ at 95%, threshold 90%", Severity: SevCritical, Kind: "fire"}
	s := formatAlert(critical)
	if !strings.Contains(s, "🚨") {
		t.Errorf("critical fire missing 🚨: %q", s)
	}
	if !strings.Contains(s, critical.Title) || !strings.Contains(s, critical.Body) {
		t.Errorf("critical fire missing title/body: %q", s)
	}
	if !strings.Contains(s, critical.Title+"\n"+critical.Body) {
		t.Errorf("expected body on its own line after title: %q", s)
	}

	warning := Alert{Title: "CPU high", Body: "cpu at 96%", Severity: SevWarning, Kind: "fire"}
	if s := formatAlert(warning); !strings.Contains(s, "⚠️") {
		t.Errorf("warning fire missing ⚠️: %q", s)
	}

	info := Alert{Title: "Heads up", Body: "just fyi", Severity: SevInfo, Kind: "fire"}
	if s := formatAlert(info); !strings.Contains(s, "ℹ️") {
		t.Errorf("info fire missing ℹ️: %q", s)
	}

	recover := Alert{Title: "Disk recovered", Body: "disk:/ back to 40%", Severity: SevCritical, Kind: "recover"}
	rs := formatAlert(recover)
	if !strings.Contains(rs, "✅") {
		t.Errorf("recover missing ✅: %q", rs)
	}
	if strings.Contains(rs, "🚨") {
		t.Errorf("recover must not carry the fire severity marker: %q", rs)
	}
}
