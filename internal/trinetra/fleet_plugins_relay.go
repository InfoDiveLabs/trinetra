package trinetra

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/InfoDiveLabs/trinetra/internal/fleet"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const (
	rpcPluginSlots  = "plugin_slots"
	rpcPluginRender = "plugin_render"
	rpcPluginAction = "plugin_action"

	rpcPluginMaxBody = (1 << 20) - 1024
)

type rpcPluginSlotsArgs struct {
	Slot plugin.Slot `json:"slot"`
}

type pluginUIHolder struct{ ui plugin.UI }

var localPluginUI atomic.Pointer[pluginUIHolder]

func setLocalPluginUI(ui plugin.UI) {
	if ui == nil {
		localPluginUI.Store(nil)
		return
	}
	localPluginUI.Store(&pluginUIHolder{ui: ui})
}

func rpcTooLarge() rpcResultData { return rpcResultData{OK: false, Error: "too large"} }

func rpcJSONResult(v any) rpcResultData {
	b, err := json.Marshal(v)
	if err != nil {
		return rpcResultData{OK: false, Error: err.Error()}
	}
	res := rpcResultData{OK: true, Output: string(b)}
	if len(b) > plugin.MaxMessageBytes {
		return rpcTooLarge()
	}
	if enc, err := json.Marshal(res); err != nil || len(enc) > rpcPluginMaxBody {
		return rpcTooLarge()
	}
	return res
}

func rpcPluginError(err error) rpcResultData {
	return rpcResultData{OK: false, Error: err.Error()}
}

// dispatchPluginRPC runs a plugin_* relay method against this node's local plugin UI.
func dispatchPluginRPC(req rpcFrameData) (rpcResultData, bool) {
	switch req.Method {
	case rpcPluginSlots, rpcPluginRender, rpcPluginAction:
	default:
		return rpcResultData{}, false
	}
	h := localPluginUI.Load()
	if h == nil {
		return rpcResultData{OK: false, Error: plugin.ErrUnavailable.Error()}, true
	}
	switch req.Method {
	case rpcPluginSlots:
		var a rpcPluginSlotsArgs
		if json.Unmarshal(req.Args, &a) != nil {
			return rpcResultData{OK: false, Error: "bad args"}, true
		}
		offers, err := h.ui.Slots(a.Slot)
		if err != nil {
			return rpcPluginError(err), true
		}
		return rpcJSONResult(offers), true
	case rpcPluginRender:
		var r plugin.UIRequest
		if json.Unmarshal(req.Args, &r) != nil {
			return rpcResultData{OK: false, Error: "bad args"}, true
		}
		r.Node = ""
		res, err := h.ui.RenderSlot(r)
		if err != nil {
			return rpcPluginError(err), true
		}
		return rpcJSONResult(res), true
	default:
		var r plugin.UIActionRequest
		if json.Unmarshal(req.Args, &r) != nil {
			return rpcResultData{OK: false, Error: "bad args"}, true
		}
		r.Node = ""
		res, err := h.ui.InvokeAction(r)
		if err != nil {
			return rpcPluginError(err), true
		}
		return rpcJSONResult(res), true
	}
}

type pluginRelay struct {
	hub     *fleet.Hub
	rpc     *rpcRegistry
	resolve func(node string) (string, bool)
}

func (r *pluginRelay) call(node, method string, args any, out any) error {
	id := node
	if r.resolve != nil {
		resolved, ok := r.resolve(node)
		if !ok {
			return fmt.Errorf("node %s: unknown node", node)
		}
		id = resolved
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	res, err := r.rpc.Call(r.hub, id, method, raw)
	if err != nil {
		return fmt.Errorf("node %s: %w", id, err)
	}
	if !res.OK {
		msg := res.Error
		if msg == "" {
			msg = "failed"
		}
		return fmt.Errorf("node %s: %s", id, msg)
	}
	if err := json.Unmarshal([]byte(res.Output), out); err != nil {
		return fmt.Errorf("node %s: %w", id, errors.New("malformed answer"))
	}
	return nil
}

// RelaySlots asks node which slots of the given kind its plugins offer.
func (r *pluginRelay) RelaySlots(node string, slot plugin.Slot) ([]plugin.SlotOffer, error) {
	var out []plugin.SlotOffer
	if err := r.call(node, rpcPluginSlots, rpcPluginSlotsArgs{Slot: slot}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RelayRender renders a slot on a child node.
func (r *pluginRelay) RelayRender(req plugin.UIRequest) (plugin.RenderResult, error) {
	node := req.Node
	req.Node = ""
	var out plugin.RenderResult
	if err := r.call(node, rpcPluginRender, req, &out); err != nil {
		return plugin.RenderResult{}, err
	}
	return out, nil
}

// RelayAction runs a plugin action on a child node.
func (r *pluginRelay) RelayAction(req plugin.UIActionRequest) (plugin.ActionResult, error) {
	node := req.Node
	req.Node = ""
	var out plugin.ActionResult
	if err := r.call(node, rpcPluginAction, req, &out); err != nil {
		return plugin.ActionResult{}, err
	}
	return out, nil
}

func nodeResolver(nodes func() []fleet.Node) func(string) (string, bool) {
	return func(want string) (string, bool) {
		for _, n := range nodes() {
			if n.ID == want || strings.EqualFold(n.Name, want) {
				return n.ID, true
			}
		}
		return "", false
	}
}
