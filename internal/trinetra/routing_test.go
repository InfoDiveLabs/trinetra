package trinetra

import (
	"testing"
	"time"
)

func TestTargetKind(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"disk:/", "disk"},
		{"cpu", "cpu"},
		{"docker:web", "docker"},
	}
	for _, c := range cases {
		if got := targetKind(c.key); got != c.want {
			t.Errorf("targetKind(%q) = %q, want %q", c.key, got, c.want)
		}
	}
}

func TestRouteMinSeverity(t *testing.T) {
	r := Route{MinSeverity: SevWarning}

	info := Alert{Key: "cpu", Severity: SevInfo}
	warning := Alert{Key: "cpu", Severity: SevWarning}
	critical := Alert{Key: "cpu", Severity: SevCritical}

	if r.Allows(info, false) {
		t.Error("expected info alert to be dropped by warning-minimum route")
	}
	if !r.Allows(warning, false) {
		t.Error("expected warning alert to pass warning-minimum route")
	}
	if !r.Allows(critical, false) {
		t.Error("expected critical alert to pass warning-minimum route")
	}
}

func TestRouteKindFilters(t *testing.T) {
	include := Route{IncludeKinds: []string{"disk"}}
	if !include.Allows(Alert{Key: "disk:/"}, false) {
		t.Error("expected disk:/ to pass IncludeKinds=[disk]")
	}
	if include.Allows(Alert{Key: "cpu"}, false) {
		t.Error("expected cpu to be dropped when IncludeKinds=[disk]")
	}

	exclude := Route{ExcludeKinds: []string{"cpu"}}
	if exclude.Allows(Alert{Key: "cpu"}, false) {
		t.Error("expected cpu to be dropped by ExcludeKinds=[cpu]")
	}
	if !exclude.Allows(Alert{Key: "disk:/"}, false) {
		t.Error("expected disk:/ to pass ExcludeKinds=[cpu]")
	}
}

func TestRouteQuietHours(t *testing.T) {
	r := Route{}
	warning := Alert{Key: "cpu", Severity: SevWarning}
	critical := Alert{Key: "cpu", Severity: SevCritical}

	if r.Allows(warning, true) {
		t.Error("expected warning alert to be dropped during quiet hours")
	}
	if r.Allows(critical, true) {
		t.Error("expected critical alert to be dropped during quiet hours when CriticalOverridesQuiet is false")
	}

	override := Route{CriticalOverridesQuiet: true}
	if !override.Allows(critical, true) {
		t.Error("expected critical alert to pass during quiet hours when CriticalOverridesQuiet is true")
	}
	if override.Allows(warning, true) {
		t.Error("expected warning alert to still be dropped during quiet hours even with CriticalOverridesQuiet")
	}

	if !r.Allows(warning, false) {
		t.Error("expected warning alert to pass normally when not quiet")
	}
	if !r.Allows(critical, false) {
		t.Error("expected critical alert to pass normally when not quiet")
	}
}

func TestDispatcherRespectsRoutes(t *testing.T) {
	diskOnly := &fakeNotifier{name: "disk-only"}
	criticalOnly := &fakeNotifier{name: "critical-only"}
	disabled := &fakeNotifier{name: "disabled"}
	catchAll := &fakeNotifier{name: "catch-all"}

	channels := []Channel{
		{N: diskOnly, Route: Route{IncludeKinds: []string{"disk"}}, Enabled: true},
		{N: criticalOnly, Route: Route{MinSeverity: SevCritical}, Enabled: true},
		{N: disabled, Route: Route{}, Enabled: false},
		{N: catchAll, Route: Route{}, Enabled: true},
	}
	d := NewDispatcher(channels, time.Second)

	a := Alert{Key: "disk:/", Severity: SevWarning, Kind: "fire"}
	results := d.Dispatch(a, false)

	byChannel := map[string]bool{}
	for _, r := range results {
		byChannel[r.Channel] = true
	}

	if !byChannel["disk-only"] {
		t.Error("expected disk-only channel to be attempted")
	}
	if len(diskOnly.received()) != 1 {
		t.Error("expected disk-only notifier to receive the alert")
	}

	if byChannel["critical-only"] {
		t.Error("expected critical-only channel to be skipped for a warning alert")
	}
	if len(criticalOnly.received()) != 0 {
		t.Error("expected critical-only notifier to never be called")
	}

	if byChannel["disabled"] {
		t.Error("expected disabled channel to be skipped")
	}
	if len(disabled.received()) != 0 {
		t.Error("expected disabled notifier to never be called")
	}

	if !byChannel["catch-all"] {
		t.Error("expected catch-all channel to be attempted")
	}
	if len(catchAll.received()) != 1 {
		t.Error("expected catch-all notifier to receive the alert")
	}

	if len(results) != 2 {
		t.Errorf("expected 2 results (only attempted channels: disk-only, catch-all), got %d", len(results))
	}
}
