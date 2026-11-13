package pluginhost

import (
	"errors"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func uiRig(t *testing.T) (*UI, *fakePlugin, *Registrar) {
	inv := newFakeInv()
	fp := startFakePlugin(t, inv, "probe")
	p := enabledPlugin("probe", plugin.PermMetricsRead)
	p.Manifest.Name = "Probe"
	p.Manifest.Contributes = plugin.Contributes{
		Slots: []plugin.SlotDecl{
			{Slot: plugin.SlotNavPage, ID: "main", Title: "Main"},
			{Slot: plugin.SlotNavPage, ID: "admin", Title: "Admin", MinRole: plugin.RoleAdmin},
			{Slot: plugin.SlotSettingsForm, ID: "settings", Title: "Settings"},
		},
		Actions: []plugin.ActionDecl{
			{ID: "read", Label: "Read", MinRole: plugin.RoleViewer},
			{ID: "restart", Label: "Restart", MinRole: plugin.RoleResponder, Mutating: true},
			{ID: "scan", Label: "Scan", MinRole: plugin.RoleViewer, Permission: plugin.PermDockerRead},
		},
	}
	inv.add(p)
	off := enabledPlugin("off")
	off.State = plugin.StateInstalled
	off.Manifest.Contributes.Slots = []plugin.SlotDecl{{Slot: plugin.SlotNavPage, ID: "main", Title: "Off"}}
	inv.add(off)
	reg := NewRegistrar(control.NewTokens())
	_, _ = reg.Register("probe", []plugin.Permission{plugin.PermMetricsRead}, false)
	return NewUI(func() plugin.Inventory { return inv }, NewCaller(func() plugin.Inventory { return inv }), reg), fp, reg
}

func slotReq(slot plugin.Slot, id string, role plugin.Role) plugin.UIRequest {
	return plugin.UIRequest{PluginID: "probe", Request: plugin.SlotRequest{Slot: slot, SlotID: id, User: plugin.UserContext{Name: "u", Role: role}}}
}

func TestRenderSlotRequiresDeclaredSlotAndRole(t *testing.T) {
	u, fp, _ := uiRig(t)
	if _, err := u.RenderSlot(slotReq(plugin.SlotNavPage, "main", plugin.RoleViewer)); err != nil {
		t.Fatal(err)
	}
	if fp.calls.Load() != 1 {
		t.Fatal("plugin not called")
	}
	for name, r := range map[string]plugin.UIRequest{
		"admin slot as responder": slotReq(plugin.SlotNavPage, "admin", plugin.RoleResponder),
		"undeclared id":           slotReq(plugin.SlotNavPage, "nope", plugin.RoleAdmin),
		"wrong slot kind":         slotReq(plugin.SlotDashboardPanel, "main", plugin.RoleAdmin),
		"no role":                 slotReq(plugin.SlotNavPage, "main", ""),
	} {
		if _, err := u.RenderSlot(r); !errors.Is(err, plugin.ErrPermissionDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := u.RenderSlot(slotReq(plugin.SlotNavPage, "admin", plugin.RoleAdmin)); err != nil {
		t.Fatal(err)
	}
	r := slotReq(plugin.SlotNavPage, "main", plugin.RoleAdmin)
	r.PluginID = "off"
	if _, err := u.RenderSlot(r); !errors.Is(err, plugin.ErrPermissionDenied) {
		t.Fatalf("disabled: %v", err)
	}
}

func TestRenderSlotRefusesHostRendered(t *testing.T) {
	u, fp, _ := uiRig(t)
	_, err := u.RenderSlot(slotReq(plugin.SlotSettingsForm, "settings", plugin.RoleAdmin))
	if err == nil || !strings.Contains(err.Error(), "rendered by the host") {
		t.Fatalf("%v", err)
	}
	if fp.calls.Load() != 0 {
		t.Fatal("plugin was called")
	}
}

func actReq(action string, role plugin.Role, confirmed bool) plugin.UIActionRequest {
	return plugin.UIActionRequest{PluginID: "probe", Request: plugin.ActionRequest{Action: action, Confirmed: confirmed, User: plugin.UserContext{Role: role}}}
}

func TestInvokeActionChecksRolePermissionConfirm(t *testing.T) {
	u, fp, reg := uiRig(t)
	if _, err := u.InvokeAction(actReq("read", plugin.RoleViewer, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := u.InvokeAction(actReq("ghost", plugin.RoleAdmin, true)); !errors.Is(err, plugin.ErrPermissionDenied) {
		t.Fatalf("undeclared: %v", err)
	}
	if _, err := u.InvokeAction(actReq("restart", plugin.RoleViewer, true)); !errors.Is(err, plugin.ErrPermissionDenied) {
		t.Fatalf("role: %v", err)
	}
	n := fp.calls.Load()
	if _, err := u.InvokeAction(actReq("restart", plugin.RoleResponder, false)); err == nil || err.Error() != "confirmation required" {
		t.Fatalf("confirm: %v", err)
	}
	if fp.calls.Load() != n {
		t.Fatal("called without confirmation")
	}
	if _, err := u.InvokeAction(actReq("restart", plugin.RoleResponder, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := u.InvokeAction(actReq("scan", plugin.RoleAdmin, false)); !errors.Is(err, plugin.ErrPermissionDenied) {
		t.Fatalf("permission: %v", err)
	}
	_, _ = reg.Register("probe", []plugin.Permission{plugin.PermDockerRead}, false)
	if _, err := u.InvokeAction(actReq("scan", plugin.RoleAdmin, false)); err != nil {
		t.Fatal(err)
	}
}

func TestSlotsListsEnabledOnly(t *testing.T) {
	u, _, _ := uiRig(t)
	offers, err := u.Slots(plugin.SlotNavPage)
	if err != nil || len(offers) != 2 {
		t.Fatalf("%+v %v", offers, err)
	}
	if offers[0].SlotID != "admin" || offers[1].SlotID != "main" || offers[0].PluginName != "Probe" ||
		offers[0].MinRole != plugin.RoleAdmin || offers[1].MinRole != plugin.RoleViewer {
		t.Fatalf("%+v", offers)
	}
	host, _ := u.Slots(plugin.SlotSettingsForm)
	if len(host) != 1 || host[0].SlotID != "settings" {
		t.Fatalf("host-rendered slots must be listed: %+v", host)
	}
	if none, _ := u.Slots(plugin.SlotAlertRules); len(none) != 0 {
		t.Fatal("unexpected offers")
	}
}
