package web

import (
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestDeliveryRows(t *testing.T) {
	rows := []channelRow{
		{Name: "pager", Enabled: true, MinSeverity: "critical"},
		{Name: "slack", Enabled: true, MinSeverity: "warning", Filtered: true},
		{Name: "mail", Enabled: false, MinSeverity: "info"},
	}
	got := map[string]string{}
	for _, d := range deliveryRows(rows) {
		var names []string
		for _, c := range d.Channels {
			names = append(names, c.Name)
		}
		got[d.Severity] = strings.Join(names, ",")
	}
	want := map[string]string{"critical": "pager,slack", "warning": "slack", "info": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s -> %q, want %q", k, got[k], v)
		}
	}
}

func TestQuietHoursText(t *testing.T) {
	for in, want := range map[string]string{"23-8": "23:00 to 08:00", "0-6": "00:00 to 06:00", "odd": "odd", "": ""} {
		if got := quietHoursText(in); got != want {
			t.Errorf("quietHoursText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChannelsPageShowsWhoGetsWhat(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Channels = []config.ChannelConfig{{Name: "ops", Type: "slack", Enabled: true, MinSeverity: "critical"}}
	(*cfg).QuietHours = "22-7"
	_, body := getAsRole(t, d, RoleAdmin, "/channels")
	for _, want := range []string{`data-tab="delivery"`, "Nobody: no enabled channel takes this level", "22:00 to 07:00", `<b>ops</b>`} {
		if !strings.Contains(body, want) {
			t.Errorf("channels page missing %q", want)
		}
	}
}
