package control

import (
	"encoding/json"
	"fmt"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

// pluginsParams is the single request shape of every Plugins.* method.
type pluginsParams struct {
	ID        string                  `json:"id,omitempty"`
	Name      string                  `json:"name,omitempty"`
	Actor     string                  `json:"actor,omitempty"`
	Via       string                  `json:"via,omitempty"`
	Refresh   bool                    `json:"refresh,omitempty"`
	Slot      plugin.Slot             `json:"slot,omitempty"`
	Node      string                  `json:"node,omitempty"`
	Install   plugin.InstallRequest   `json:"install"`
	Enable    plugin.EnableRequest    `json:"enable"`
	Update    plugin.UpdateRequest    `json:"update"`
	SetConfig plugin.SetConfigRequest `json:"set_config"`
	Dev       plugin.DevRequest       `json:"dev"`
	UI        plugin.UIRequest        `json:"ui"`
	Action    plugin.UIActionRequest  `json:"action"`
	Push      plugin.PushRequest      `json:"push"`
}

// pluginFileResult is the Plugins.File result.
type pluginFileResult struct {
	Decl plugin.FileDecl `json:"decl"`
	Data []byte          `json:"data"`
}

// dispatchPlugins serves the Plugins.* management methods against api's plugin.Manager.
func dispatchPlugins(api core.API, method string, params json.RawMessage) (json.RawMessage, error) {
	prov, ok := api.(plugin.ManagerProvider)
	if !ok {
		return nil, plugin.ErrUnavailable
	}
	m := prov.Plugins()
	if m == nil {
		return nil, plugin.ErrUnavailable
	}
	var p pluginsParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	switch method {
	case "Plugins.List":
		return marshalResult(m.List())
	case "Plugins.Get":
		return marshalResult(m.Get(p.ID))
	case "Plugins.Install":
		return marshalResult(m.Install(p.Install))
	case "Plugins.Enable":
		return emptyOrErr(m.Enable(p.Enable))
	case "Plugins.Disable":
		return emptyOrErr(m.Disable(p.ID, p.Actor, p.Via))
	case "Plugins.Remove":
		return emptyOrErr(m.Remove(p.ID, p.Actor, p.Via))
	case "Plugins.Update":
		return marshalResult(m.Update(p.Update))
	case "Plugins.SetConfig":
		return emptyOrErr(m.SetConfig(p.SetConfig))
	case "Plugins.Config":
		return marshalResult(m.Config(p.ID))
	case "Plugins.File":
		decl, data, err := m.File(p.ID, p.Name)
		return marshalResult(pluginFileResult{Decl: decl, Data: data}, err)
	case "Plugins.PackageFile":
		return marshalResult(m.PackageFile(p.ID))
	case "Plugins.DevAttach":
		return marshalResult(m.DevAttach(p.Dev))
	case "Plugins.DevDetach":
		return emptyOrErr(m.DevDetach(p.ID))
	case "Plugins.Slots":
		return marshalResult(m.Slots(p.Node, p.Slot))
	case "Plugins.RenderSlot":
		return marshalResult(m.RenderSlot(p.UI))
	case "Plugins.InvokeAction":
		return marshalResult(m.InvokeAction(p.Action))
	case "Plugins.Catalog":
		return marshalResult(m.Catalog(p.Refresh))
	case "Plugins.Push":
		return marshalResult(m.Push(p.Push))
	case "Plugins.NodeStates":
		return marshalResult(m.NodeStates(p.ID))
	}
	return nil, fmt.Errorf("control: unknown method %q", method)
}
