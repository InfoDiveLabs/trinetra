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
