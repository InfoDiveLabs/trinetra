package pluginrt

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func recWith(trust plugin.Trust, perms ...plugin.Permission) plugin.Installed {
	return plugin.Installed{ID: "demo", Version: "1.0.0", Trust: trust,
		Manifest: testManifest("demo", func(m *plugin.Manifest) { m.Permissions = perms })}
}

func TestEnableRegistryNeedsAllPermissions(t *testing.T) {
	rec := recWith(plugin.TrustVerified, plugin.PermMetricsRead, plugin.PermNetOutbound)
	_, err := CheckEnable(rec, plugin.EnableRequest{Accepted: []plugin.Permission{plugin.PermMetricsRead}}, true, 1)
	if !errors.Is(err, plugin.ErrConsentRequired) || !strings.Contains(err.Error(), "net.outbound") {
		t.Fatalf("err = %v", err)
	}
	if _, err := CheckEnable(rec, plugin.EnableRequest{Accepted: []plugin.Permission{plugin.PermMetricsRead, plugin.PermNetOutbound}}, true, 1); err != nil {
		t.Fatal(err)
	}
}

func TestEnableUnverifiedNeedsRiskAck(t *testing.T) {
	rec := recWith(plugin.TrustUnverified, plugin.PermMetricsRead)
	req := plugin.EnableRequest{Accepted: []plugin.Permission{plugin.PermMetricsRead}}
	if _, err := CheckEnable(rec, req, true, 1); !errors.Is(err, plugin.ErrConsentRequired) {
		t.Fatalf("err = %v", err)
	}
	req.AcknowledgedRisk = true
	c, err := CheckEnable(rec, req, true, 1)
	if err != nil || !c.Unverified {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestEnableUnverifiedRefusedWhenDisallowed(t *testing.T) {
	rec := recWith(plugin.TrustUnverified, plugin.PermMetricsRead)
	req := plugin.EnableRequest{Accepted: rec.Manifest.Permissions, AcknowledgedRisk: true}
	if _, err := CheckEnable(rec, req, false, 1); !errors.Is(err, plugin.ErrUnverifiedDisallowed) {
		t.Fatalf("err = %v", err)
	}
}

func TestEnableRecordsConsentFields(t *testing.T) {
	rec := recWith(plugin.TrustVerified, plugin.PermMetricsRead)
	c, err := CheckEnable(rec, plugin.EnableRequest{Actor: "root", Via: "cli", Accepted: rec.Manifest.Permissions}, true, 42)
	if err != nil {
		t.Fatal(err)
	}
	want := plugin.ConsentRecord{Time: 42, Actor: "root", Via: "cli", Version: "1.0.0", Permissions: []plugin.Permission{plugin.PermMetricsRead}}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("%+v", c)
	}
}

func TestEnablePendingUsesStagedManifest(t *testing.T) {
	rec := recWith(plugin.TrustVerified, plugin.PermMetricsRead)
	next := testManifest("demo", func(m *plugin.Manifest) {
		m.Version = "2.0.0"
		m.Permissions = []plugin.Permission{plugin.PermMetricsRead, plugin.PermAlertsRaise}
	})
	rec.Pending = &plugin.PendingUpdate{Version: "2.0.0", Manifest: next, Trust: plugin.TrustVerified}
	req := plugin.EnableRequest{Pending: true, Accepted: []plugin.Permission{plugin.PermMetricsRead}}
	if _, err := CheckEnable(rec, req, true, 1); !errors.Is(err, plugin.ErrConsentRequired) {
		t.Fatalf("err = %v", err)
	}
	req.Accepted = next.Permissions
	c, err := CheckEnable(rec, req, true, 1)
	if err != nil || c.Version != "2.0.0" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestPlanUpdateNoNewPermissions(t *testing.T) {
	cur := recWith(plugin.TrustVerified, plugin.PermMetricsRead)
	cur.Granted = cur.Manifest.Permissions
	if got := PlanUpdate(cur, cur.Manifest); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestPlanUpdateNewPermissions(t *testing.T) {
	cur := recWith(plugin.TrustVerified, plugin.PermMetricsRead)
	cur.Granted = cur.Manifest.Permissions
	next := testManifest("demo", func(m *plugin.Manifest) {
		m.Permissions = []plugin.Permission{plugin.PermMetricsRead, plugin.PermNetOutbound, plugin.PermAlertsRaise}
	})
	got := PlanUpdate(cur, next)
	if !reflect.DeepEqual(got, []plugin.Permission{plugin.PermAlertsRaise, plugin.PermNetOutbound}) {
		t.Fatalf("got %v", got)
	}
}
