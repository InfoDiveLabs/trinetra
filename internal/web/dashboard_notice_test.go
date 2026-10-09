package web

import (
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestDashboardNoChannelNotice(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Channels = nil
	_, body := getAsRole(t, d, RoleAdmin, "/")
	if !strings.Contains(body, `data-dismiss="no-channel"`) || !strings.Contains(body, `href="/channels"`) {
		t.Fatal("admin dashboard lacks the no-channel notice")
	}
	_, body = getAsRole(t, d, RoleViewer, "/")
	if strings.Contains(body, `data-dismiss="no-channel"`) {
		t.Fatal("viewer sees an admin-only notice")
	}
	(*cfg).Channels = []config.ChannelConfig{{Name: "ops", Type: "slack", Enabled: true}}
	_, body = getAsRole(t, d, RoleAdmin, "/")
	if strings.Contains(body, `data-dismiss="no-channel"`) {
		t.Fatal("notice shown with a channel enabled")
	}
}
