package trinetra

import (
	"fmt"
	"sync"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type fleetFakeLifecycle struct {
	mu         sync.Mutex
	installs   []plugin.InstallRequest
	enables    []plugin.EnableRequest
	disables   []string
	removes    []string
	configs    []plugin.SetConfigRequest
	installed  map[string]plugin.Status
	pkgs       map[string]plugin.PackageInfo
	current    map[string]plugin.RedactedConfig
	installErr error
	enabled    bool
	vias       []string
}

func newFleetFakeLifecycle() *fleetFakeLifecycle {
	return &fleetFakeLifecycle{installed: map[string]plugin.Status{}, pkgs: map[string]plugin.PackageInfo{}, enabled: true}
}

func (f *fleetFakeLifecycle) List() ([]plugin.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []plugin.Status
	for _, s := range f.installed {
		out = append(out, s)
	}
	return out, nil
}

func (f *fleetFakeLifecycle) Get(id string) (plugin.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.installed[id]; ok {
		return s, nil
	}
	return plugin.Status{}, plugin.ErrNotFound
}

func (f *fleetFakeLifecycle) Install(r plugin.InstallRequest) (plugin.InstallResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs = append(f.installs, r)
	f.vias = append(f.vias, r.Via)
	if f.installErr != nil {
		return plugin.InstallResult{}, f.installErr
	}
	return plugin.InstallResult{ID: "x", Version: "1.0.0", Enabled: f.enabled}, nil
}

func (f *fleetFakeLifecycle) Enable(r plugin.EnableRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enables = append(f.enables, r)
	return nil
}

func (f *fleetFakeLifecycle) Disable(id, actor, via string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disables = append(f.disables, id)
	f.vias = append(f.vias, via)
	return nil
}

func (f *fleetFakeLifecycle) Remove(id, actor, via string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, fmt.Sprintf("%s/%s/%s", id, actor, via))
	return nil
}

func (f *fleetFakeLifecycle) Update(plugin.UpdateRequest) ([]plugin.UpdateResult, error) {
	return nil, nil
}

func (f *fleetFakeLifecycle) SetConfig(r plugin.SetConfigRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs = append(f.configs, r)
	f.vias = append(f.vias, r.Via)
	return nil
}

func (f *fleetFakeLifecycle) Config(id string) (plugin.RedactedConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.installed[id]; !ok {
		return plugin.RedactedConfig{}, plugin.ErrNotFound
	}
	return f.current[id], nil
}

func (f *fleetFakeLifecycle) File(string, string) (plugin.FileDecl, []byte, error) {
	return plugin.FileDecl{}, nil, plugin.ErrNotFound
}

func (f *fleetFakeLifecycle) PackageFile(id string) (plugin.PackageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.pkgs[id]; ok {
		return p, nil
	}
	return plugin.PackageInfo{}, plugin.ErrNotFound
}

func (f *fleetFakeLifecycle) DevAttach(plugin.DevRequest) (plugin.DevSession, error) {
	return plugin.DevSession{}, plugin.ErrUnavailable
}

func (f *fleetFakeLifecycle) DevDetach(string) error { return nil }

type fleetFakeUI struct {
	mu      sync.Mutex
	slots   []plugin.SlotOffer
	render  plugin.RenderResult
	action  plugin.ActionResult
	err     error
	gotRend []plugin.UIRequest
	gotAct  []plugin.UIActionRequest
	gotSlot []plugin.Slot
}

func (u *fleetFakeUI) Slots(s plugin.Slot) ([]plugin.SlotOffer, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.gotSlot = append(u.gotSlot, s)
	return u.slots, u.err
}

func (u *fleetFakeUI) RenderSlot(r plugin.UIRequest) (plugin.RenderResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.gotRend = append(u.gotRend, r)
	return u.render, u.err
}

func (u *fleetFakeUI) InvokeAction(r plugin.UIActionRequest) (plugin.ActionResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.gotAct = append(u.gotAct, r)
	return u.action, u.err
}
