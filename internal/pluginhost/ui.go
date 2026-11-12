package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

// UI renders plugin slots and runs plugin actions after checking the manifest's declarations.
type UI struct {
	inventory func() plugin.Inventory
	caller    *Caller
	reg       *Registrar
}

var _ plugin.UI = (*UI)(nil)

// NewUI returns a UI over the inventory, caller and registrar.
func NewUI(inv func() plugin.Inventory, c *Caller, r *Registrar) *UI {
	return &UI{inventory: inv, caller: c, reg: r}
}

func (u *UI) enabled(id string) (plugin.Installed, error) {
	var inv plugin.Inventory
	if u.inventory != nil {
		inv = u.inventory()
	}
	if inv == nil {
		return plugin.Installed{}, plugin.ErrUnavailable
	}
	p, ok := inv.Get(id)
	if !ok || p.State != plugin.StateEnabled {
		return plugin.Installed{}, fmt.Errorf("%w: %s is not enabled", plugin.ErrPermissionDenied, id)
	}
	return p, nil
}

func roleOK(have, need plugin.Role) bool {
	if need == "" {
		need = plugin.RoleViewer
	}
	return plugin.RoleRank(have) >= plugin.RoleRank(need)
}

// Slots lists the enabled plugins' declarations for slot.
func (u *UI) Slots(slot plugin.Slot) ([]plugin.SlotOffer, error) {
	var inv plugin.Inventory
	if u.inventory != nil {
		inv = u.inventory()
	}
	if inv == nil {
		return nil, plugin.ErrUnavailable
	}
	var out []plugin.SlotOffer
	for _, p := range inv.Enabled() {
		for _, d := range p.Manifest.Contributes.Slots {
			if d.Slot != slot {
				continue
			}
			min := d.MinRole
			if min == "" {
				min = plugin.RoleViewer
			}
			out = append(out, plugin.SlotOffer{PluginID: p.ID, PluginName: p.Manifest.Name, Slot: d.Slot, SlotID: d.ID, Title: d.Title, MinRole: min})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PluginID != out[j].PluginID {
			return out[i].PluginID < out[j].PluginID
		}
		return out[i].SlotID < out[j].SlotID
	})
	return out, nil
}

// RenderSlot asks the plugin to render a slot it declared, for a user whose role suffices.
func (u *UI) RenderSlot(r plugin.UIRequest) (plugin.RenderResult, error) {
	if r.Request.Slot.HostRendered() {
		return plugin.RenderResult{}, fmt.Errorf("slot %s is rendered by the host, not by the plugin", r.Request.Slot)
	}
	p, err := u.enabled(r.PluginID)
	if err != nil {
		return plugin.RenderResult{}, err
	}
	for _, d := range p.Manifest.Contributes.Slots {
		if d.Slot == r.Request.Slot && d.ID == r.Request.SlotID {
			if !roleOK(r.Request.User.Role, d.MinRole) {
				return plugin.RenderResult{}, fmt.Errorf("%w: slot %s needs role %s", plugin.ErrPermissionDenied, d.ID, d.MinRole)
			}
			return u.caller.RenderSlot(context.Background(), r.PluginID, r.Request)
		}
	}
	return plugin.RenderResult{}, fmt.Errorf("%w: %s does not declare slot %s/%s", plugin.ErrPermissionDenied, r.PluginID, r.Request.Slot, r.Request.SlotID)
}

// InvokeAction runs a declared action after role, permission and confirmation checks.
func (u *UI) InvokeAction(r plugin.UIActionRequest) (plugin.ActionResult, error) {
	p, err := u.enabled(r.PluginID)
	if err != nil {
		return plugin.ActionResult{}, err
	}
	var decl *plugin.ActionDecl
	for i := range p.Manifest.Contributes.Actions {
		if p.Manifest.Contributes.Actions[i].ID == r.Request.Action {
			decl = &p.Manifest.Contributes.Actions[i]
		}
	}
	if decl == nil {
		return plugin.ActionResult{}, fmt.Errorf("%w: %s does not declare action %s", plugin.ErrPermissionDenied, r.PluginID, r.Request.Action)
	}
	if !roleOK(r.Request.User.Role, decl.MinRole) {
		return plugin.ActionResult{}, fmt.Errorf("%w: action %s needs role %s", plugin.ErrPermissionDenied, decl.ID, decl.MinRole)
	}
	if decl.Permission != "" {
		var granted []plugin.Permission
		if u.reg != nil {
			granted, _, _ = u.reg.Granted(r.PluginID)
		}
		if len(plugin.Missing(granted, []plugin.Permission{decl.Permission})) > 0 {
			return plugin.ActionResult{}, fmt.Errorf("%w: action %s needs permission %s", plugin.ErrPermissionDenied, decl.ID, decl.Permission)
		}
	}
	if decl.Mutating && !r.Request.Confirmed {
		return plugin.ActionResult{}, errors.New("confirmation required")
	}
	return u.caller.InvokeAction(context.Background(), r.PluginID, r.Request)
}
