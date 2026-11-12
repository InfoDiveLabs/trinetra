package plugin

import (
	"errors"
	"testing"
)

type fakeUI struct{ rendered, acted, slots []string }

func (f *fakeUI) Slots(Slot) ([]SlotOffer, error) {
	f.slots = append(f.slots, "local")
	return nil, nil
}
func (f *fakeUI) RenderSlot(r UIRequest) (RenderResult, error) {
	f.rendered = append(f.rendered, r.Node)
	return RenderResult{}, nil
}
func (f *fakeUI) InvokeAction(r UIActionRequest) (ActionResult, error) {
	f.acted = append(f.acted, r.Node)
	return ActionResult{}, nil
}

type fakeFleet struct{ rendered, acted, slots []string }

func (f *fakeFleet) Push(PushRequest) (PushResult, error)   { return PushResult{}, nil }
func (f *fakeFleet) NodeStates(string) ([]NodeState, error) { return nil, nil }
func (f *fakeFleet) RelaySlots(n string, _ Slot) ([]SlotOffer, error) {
	f.slots = append(f.slots, n)
	return nil, nil
}
func (f *fakeFleet) RelayRender(r UIRequest) (RenderResult, error) {
	f.rendered = append(f.rendered, r.Node)
	return RenderResult{}, nil
}
func (f *fakeFleet) RelayAction(r UIActionRequest) (ActionResult, error) {
	f.acted = append(f.acted, r.Node)
	return ActionResult{}, nil
}

func TestComposeNilPartsReturnUnavailable(t *testing.T) {
	m := Compose(nil, nil, nil, nil)
	calls := map[string]func() error{
		"List":         func() error { _, e := m.List(); return e },
		"Get":          func() error { _, e := m.Get("x"); return e },
		"Install":      func() error { _, e := m.Install(InstallRequest{}); return e },
		"Enable":       func() error { return m.Enable(EnableRequest{}) },
		"Disable":      func() error { return m.Disable("x", "a", "cli") },
		"Remove":       func() error { return m.Remove("x", "a", "cli") },
		"Update":       func() error { _, e := m.Update(UpdateRequest{}); return e },
		"SetConfig":    func() error { return m.SetConfig(SetConfigRequest{}) },
		"Config":       func() error { _, e := m.Config("x"); return e },
		"File":         func() error { _, _, e := m.File("x", "y"); return e },
		"PackageFile":  func() error { _, e := m.PackageFile("x"); return e },
		"DevAttach":    func() error { _, e := m.DevAttach(DevRequest{}); return e },
		"DevDetach":    func() error { return m.DevDetach("x") },
		"Catalog":      func() error { _, e := m.Catalog(false); return e },
		"Slots":        func() error { _, e := m.Slots("", SlotNavPage); return e },
		"SlotsRemote":  func() error { _, e := m.Slots("abc", SlotNavPage); return e },
		"RenderSlot":   func() error { _, e := m.RenderSlot(UIRequest{}); return e },
		"RenderRemote": func() error { _, e := m.RenderSlot(UIRequest{Node: "abc"}); return e },
		"InvokeAction": func() error { _, e := m.InvokeAction(UIActionRequest{}); return e },
		"ActionRemote": func() error { _, e := m.InvokeAction(UIActionRequest{Node: "abc"}); return e },
		"Push":         func() error { _, e := m.Push(PushRequest{}); return e },
		"NodeStates":   func() error { _, e := m.NodeStates("x"); return e },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestComposeRoutesRemoteNodeToFleet(t *testing.T) {
	ui, fl := &fakeUI{}, &fakeFleet{}
	m := Compose(nil, ui, nil, fl)
	for _, n := range []string{"", "self", "abc"} {
		if _, err := m.RenderSlot(UIRequest{Node: n}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.InvokeAction(UIActionRequest{Node: n}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Slots(n, SlotNavPage); err != nil {
			t.Fatal(err)
		}
	}
	if len(ui.rendered) != 2 || ui.rendered[0] != "" || ui.rendered[1] != "self" {
		t.Fatalf("ui render: %v", ui.rendered)
	}
	if len(ui.acted) != 2 || len(ui.slots) != 2 {
		t.Fatalf("ui acted=%v slots=%v", ui.acted, ui.slots)
	}
	if len(fl.rendered) != 1 || fl.rendered[0] != "abc" || len(fl.acted) != 1 || len(fl.slots) != 1 || fl.slots[0] != "abc" {
		t.Fatalf("fleet: %+v", fl)
	}
}

func TestIsLocalNode(t *testing.T) {
	if !IsLocalNode("") || !IsLocalNode("self") || IsLocalNode("x") {
		t.Fatal("IsLocalNode")
	}
}
