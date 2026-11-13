package trinetra

import (
	"errors"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/fleet"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func installLocalPluginUI(t *testing.T, ui plugin.UI) {
	t.Helper()
	setLocalPluginUI(ui)
	t.Cleanup(func() { setLocalPluginUI(nil) })
}

func connectRelayChild(t *testing.T, m *rpcTestMaster, name string) string {
	t.Helper()
	var sh *fleet.Shipper
	nodeID, childSh, stop := m.connect(name, func(f fleet.Frame) { handleRPCFrame(fakeLogsAPI{}, sh, t.Logf, f) })
	sh = childSh
	t.Cleanup(stop)
	t.Cleanup(func() { drainRPCSemForTest(t) })
	return nodeID
}

func TestRelayRenderRoundTrip(t *testing.T) {
	m := newRPCTestMaster(t)
	ui := &fleetFakeUI{
		render: plugin.RenderResult{Widgets: []plugin.Widget{{Kind: "stat", Title: "Up", Stat: &plugin.StatWidget{Value: "99"}}}},
		slots:  []plugin.SlotOffer{{PluginID: "p", Slot: plugin.SlotNavPage, SlotID: "main", Title: "P"}},
		action: plugin.ActionResult{OK: true, Message: "done"},
	}
	installLocalPluginUI(t, ui)
	nodeID := connectRelayChild(t, m, "web-1")
	relay := &pluginRelay{hub: m.hub, rpc: m.reg}

	res, err := relay.RelayRender(plugin.UIRequest{PluginID: "p", Node: nodeID, Request: plugin.SlotRequest{Slot: plugin.SlotNavPage, SlotID: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Widgets) != 1 || res.Widgets[0].Stat == nil || res.Widgets[0].Stat.Value != "99" {
		t.Fatalf("render = %+v", res)
	}
	if len(ui.gotRend) != 1 || ui.gotRend[0].Node != "" || ui.gotRend[0].PluginID != "p" {
		t.Fatalf("child saw %+v", ui.gotRend)
	}

	offers, err := relay.RelaySlots(nodeID, plugin.SlotNavPage)
	if err != nil || len(offers) != 1 || offers[0].SlotID != "main" || ui.gotSlot[0] != plugin.SlotNavPage {
		t.Fatalf("slots = %+v err %v", offers, err)
	}

	act, err := relay.RelayAction(plugin.UIActionRequest{PluginID: "p", Node: nodeID, Request: plugin.ActionRequest{Action: "go"}})
	if err != nil || !act.OK || act.Message != "done" || ui.gotAct[0].Node != "" {
		t.Fatalf("action = %+v err %v", act, err)
	}
}

func TestRelayRenderChildErrorBecomesError(t *testing.T) {
	m := newRPCTestMaster(t)
	installLocalPluginUI(t, &fleetFakeUI{err: errors.New("plugin exploded")})
	nodeID := connectRelayChild(t, m, "web-1")
	relay := &pluginRelay{hub: m.hub, rpc: m.reg}
	_, err := relay.RelayRender(plugin.UIRequest{PluginID: "p", Node: nodeID})
	if err == nil || err.Error() != "node "+nodeID+": plugin exploded" {
		t.Fatalf("err = %v", err)
	}
	if _, err := relay.RelayRender(plugin.UIRequest{PluginID: "p", Node: "ghost"}); err == nil {
		t.Fatal("render on a disconnected node succeeded")
	}
}

func TestRelayResolvesNodeNames(t *testing.T) {
	m := newRPCTestMaster(t)
	installLocalPluginUI(t, &fleetFakeUI{render: plugin.RenderResult{Widgets: []plugin.Widget{{Kind: "text"}}}})
	nodeID := connectRelayChild(t, m, "web-1")
	relay := &pluginRelay{hub: m.hub, rpc: m.reg, resolve: nodeResolver(func() []fleet.Node { return []fleet.Node{{ID: nodeID, Name: "web-1"}} })}
	if _, err := relay.RelayRender(plugin.UIRequest{PluginID: "p", Node: "WEB-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := relay.RelayRender(plugin.UIRequest{PluginID: "p", Node: "nope"}); err == nil {
		t.Fatal("unknown node resolved")
	}
}

func TestChildRPCRenderRespectsCap(t *testing.T) {
	big := plugin.RenderResult{Widgets: []plugin.Widget{{Kind: "text", Text: &plugin.TextWidget{Text: strings.Repeat("a", plugin.MaxMessageBytes+10)}}}}
	installLocalPluginUI(t, &fleetFakeUI{render: big})
	res := dispatchRPC(nil, rpcFrameData{ID: "1", Method: rpcPluginRender, Args: []byte(`{"plugin_id":"p"}`)})
	if res.OK || res.Error != "too large" {
		t.Fatalf("res = %+v", res)
	}
	quoteHeavy := plugin.RenderResult{Widgets: []plugin.Widget{{Kind: "text", Text: &plugin.TextWidget{Text: strings.Repeat("\"", plugin.MaxMessageBytes/2+100)}}}}
	installLocalPluginUI(t, &fleetFakeUI{render: quoteHeavy})
	res = dispatchRPC(nil, rpcFrameData{ID: "2", Method: rpcPluginRender, Args: []byte(`{"plugin_id":"p"}`)})
	if res.OK || res.Error != "too large" {
		t.Fatalf("escaped payload over the post cap accepted: ok=%v err=%q", res.OK, res.Error)
	}
}

func TestChildRPCUnknownStillUnknown(t *testing.T) {
	res := dispatchRPC(nil, rpcFrameData{ID: "1", Method: "plugin_nope"})
	if res.OK || res.Error != "unknown method" {
		t.Fatalf("res = %+v", res)
	}
	setLocalPluginUI(nil)
	res = dispatchRPC(nil, rpcFrameData{ID: "1", Method: rpcPluginRender, Args: []byte(`{}`)})
	if res.OK || res.Error != plugin.ErrUnavailable.Error() {
		t.Fatalf("res without local UI = %+v", res)
	}
}
