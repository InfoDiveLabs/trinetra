package control

import (
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

// Plugins returns the daemon's plugin manager; Slots/RenderSlot/InvokeAction use a fresh connection per call.
func (c *Client) Plugins() plugin.Manager {
	return pluginsClient{c: &Client{clientConn: c.clientConn}}
}

type pluginsClient struct{ c *Client }

var _ plugin.Manager = pluginsClient{}

// fresh dials a short-lived connection so a slow plugin call never holds the shared client's mutex.
func (p pluginsClient) fresh() (*Client, error) {
	p.c.meta.Lock()
	tok, src := p.c.token, p.c.tokenSource
	p.c.meta.Unlock()
	nc := &Client{clientConn: &clientConn{path: p.c.path, token: tok, tokenSource: src}}
	if err := nc.connect(); err != nil {
		return nil, err
	}
	return nc, nil
}

func (p pluginsClient) callFresh(method string, params, result any) error {
	nc, err := p.fresh()
	if err != nil {
		return err
	}
	defer nc.Close()
	return nc.call(method, params, result)
}

func (p pluginsClient) List() ([]plugin.Status, error) {
	var out []plugin.Status
	err := p.c.call("Plugins.List", pluginsParams{}, &out)
	return out, err
}

func (p pluginsClient) Get(id string) (plugin.Status, error) {
	var out plugin.Status
	err := p.c.call("Plugins.Get", pluginsParams{ID: id}, &out)
	return out, err
}

func (p pluginsClient) Install(r plugin.InstallRequest) (plugin.InstallResult, error) {
	var out plugin.InstallResult
	err := p.c.call("Plugins.Install", pluginsParams{Install: r}, &out)
	return out, err
}

func (p pluginsClient) Enable(r plugin.EnableRequest) error {
	return p.c.call("Plugins.Enable", pluginsParams{Enable: r}, nil)
}

func (p pluginsClient) Disable(id, actor, via string) error {
	return p.c.call("Plugins.Disable", pluginsParams{ID: id, Actor: actor, Via: via}, nil)
}

func (p pluginsClient) Remove(id, actor, via string) error {
	return p.c.call("Plugins.Remove", pluginsParams{ID: id, Actor: actor, Via: via}, nil)
}

func (p pluginsClient) Update(r plugin.UpdateRequest) ([]plugin.UpdateResult, error) {
	var out []plugin.UpdateResult
	err := p.c.call("Plugins.Update", pluginsParams{Update: r}, &out)
	return out, err
}

func (p pluginsClient) SetConfig(r plugin.SetConfigRequest) error {
	return p.c.call("Plugins.SetConfig", pluginsParams{SetConfig: r}, nil)
}

func (p pluginsClient) Config(id string) (plugin.RedactedConfig, error) {
	var out plugin.RedactedConfig
	err := p.c.call("Plugins.Config", pluginsParams{ID: id}, &out)
	return out, err
}

func (p pluginsClient) File(id, name string) (plugin.FileDecl, []byte, error) {
	var out pluginFileResult
	err := p.c.call("Plugins.File", pluginsParams{ID: id, Name: name}, &out)
	return out.Decl, out.Data, err
}

func (p pluginsClient) PackageFile(id string) (plugin.PackageInfo, error) {
	var out plugin.PackageInfo
	err := p.c.call("Plugins.PackageFile", pluginsParams{ID: id}, &out)
	return out, err
}

func (p pluginsClient) DevAttach(r plugin.DevRequest) (plugin.DevSession, error) {
	var out plugin.DevSession
	err := p.c.call("Plugins.DevAttach", pluginsParams{Dev: r}, &out)
	return out, err
}

func (p pluginsClient) DevDetach(id string) error {
	return p.c.call("Plugins.DevDetach", pluginsParams{ID: id}, nil)
}

func (p pluginsClient) Catalog(refresh bool) (plugin.Catalog, error) {
	var out plugin.Catalog
	err := p.c.call("Plugins.Catalog", pluginsParams{Refresh: refresh}, &out)
	return out, err
}

func (p pluginsClient) Slots(node string, slot plugin.Slot) ([]plugin.SlotOffer, error) {
	var out []plugin.SlotOffer
	err := p.callFresh("Plugins.Slots", pluginsParams{Node: node, Slot: slot}, &out)
	return out, err
}

func (p pluginsClient) RenderSlot(r plugin.UIRequest) (plugin.RenderResult, error) {
	var out plugin.RenderResult
	err := p.callFresh("Plugins.RenderSlot", pluginsParams{UI: r}, &out)
	return out, err
}

func (p pluginsClient) InvokeAction(r plugin.UIActionRequest) (plugin.ActionResult, error) {
	var out plugin.ActionResult
	err := p.callFresh("Plugins.InvokeAction", pluginsParams{Action: r}, &out)
	return out, err
}

func (p pluginsClient) Push(r plugin.PushRequest) (plugin.PushResult, error) {
	var out plugin.PushResult
	err := p.c.call("Plugins.Push", pluginsParams{Push: r}, &out)
	return out, err
}

func (p pluginsClient) NodeStates(id string) ([]plugin.NodeState, error) {
	var out []plugin.NodeState
	err := p.c.call("Plugins.NodeStates", pluginsParams{ID: id}, &out)
	return out, err
}
