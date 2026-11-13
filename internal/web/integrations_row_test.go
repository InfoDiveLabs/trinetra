package web

import (
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func TestIntegrationRowHealthLabels(t *testing.T) {
	enabled := plugin.Installed{State: plugin.StateEnabled}
	cases := []struct {
		name string
		st   plugin.Status
		want string
	}{
		{"installed", plugin.Status{Installed: plugin.Installed{State: plugin.StateInstalled}}, "not running"},
		{"stopped", plugin.Status{Installed: enabled}, "stopped"},
		{"not connected yet", plugin.Status{Installed: enabled, Running: true}, "starting"},
		{"connected", plugin.Status{Installed: enabled, Running: true, Health: plugin.Health{Connected: true}}, "healthy"},
		{"error", plugin.Status{Installed: enabled, Running: true, Health: plugin.Health{Connected: true, Status: plugin.StatusReport{State: "error"}}}, "error"},
	}
	for _, c := range cases {
		if got := newIntegrationRow(c.st).HealthLabel; got != c.want {
			t.Errorf("%s: label %q, want %q", c.name, got, c.want)
		}
	}
}
