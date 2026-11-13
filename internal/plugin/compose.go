package plugin

type composed struct {
	Lifecycle
	CatalogSource
	ui    UI
	fleet FleetOps
}

// Compose builds a Manager from parts; a nil part answers ErrUnavailable. UI calls whose Node is not "" or "self" go to f.Relay*.
func Compose(l Lifecycle, u UI, c CatalogSource, f FleetOps) Manager {
	m := &composed{ui: u, fleet: f}
	if l != nil {
		m.Lifecycle = l
	} else {
		m.Lifecycle = noLifecycle{}
	}
	if c != nil {
		m.CatalogSource = c
	} else {
		m.CatalogSource = noCatalog{}
	}
	return m
}

// IsLocalNode reports whether node is "" or "self".
func IsLocalNode(node string) bool { return node == "" || node == "self" }

func (m *composed) Slots(node string, slot Slot) ([]SlotOffer, error) {
	if IsLocalNode(node) {
		if m.ui == nil {
			return nil, ErrUnavailable
		}
		return m.ui.Slots(slot)
	}
	if m.fleet == nil {
		return nil, ErrUnavailable
	}
	return m.fleet.RelaySlots(node, slot)
}

func (m *composed) RenderSlot(r UIRequest) (RenderResult, error) {
	if IsLocalNode(r.Node) {
		if m.ui == nil {
			return RenderResult{}, ErrUnavailable
		}
		return m.ui.RenderSlot(r)
	}
	if m.fleet == nil {
		return RenderResult{}, ErrUnavailable
	}
	return m.fleet.RelayRender(r)
}

func (m *composed) InvokeAction(r UIActionRequest) (ActionResult, error) {
	if IsLocalNode(r.Node) {
		if m.ui == nil {
			return ActionResult{}, ErrUnavailable
		}
		return m.ui.InvokeAction(r)
	}
	if m.fleet == nil {
		return ActionResult{}, ErrUnavailable
	}
	return m.fleet.RelayAction(r)
}

func (m *composed) Push(r PushRequest) (PushResult, error) {
	if m.fleet == nil {
		return PushResult{}, ErrUnavailable
	}
	return m.fleet.Push(r)
}

func (m *composed) NodeStates(id string) ([]NodeState, error) {
	if m.fleet == nil {
		return nil, ErrUnavailable
	}
	return m.fleet.NodeStates(id)
}

type noCatalog struct{}

func (noCatalog) Catalog(bool) (Catalog, error) { return Catalog{}, ErrUnavailable }

type noLifecycle struct{}

func (noLifecycle) List() ([]Status, error)    { return nil, ErrUnavailable }
func (noLifecycle) Get(string) (Status, error) { return Status{}, ErrUnavailable }
func (noLifecycle) Install(InstallRequest) (InstallResult, error) {
	return InstallResult{}, ErrUnavailable
}
func (noLifecycle) Enable(EnableRequest) error                   { return ErrUnavailable }
func (noLifecycle) Disable(string, string, string) error         { return ErrUnavailable }
func (noLifecycle) Remove(string, string, string) error          { return ErrUnavailable }
func (noLifecycle) Update(UpdateRequest) ([]UpdateResult, error) { return nil, ErrUnavailable }
func (noLifecycle) SetConfig(SetConfigRequest) error             { return ErrUnavailable }
func (noLifecycle) Config(string) (RedactedConfig, error)        { return RedactedConfig{}, ErrUnavailable }
func (noLifecycle) File(string, string) (FileDecl, []byte, error) {
	return FileDecl{}, nil, ErrUnavailable
}
func (noLifecycle) PackageFile(string) (PackageInfo, error)  { return PackageInfo{}, ErrUnavailable }
func (noLifecycle) DevAttach(DevRequest) (DevSession, error) { return DevSession{}, ErrUnavailable }
func (noLifecycle) DevDetach(string) error                   { return ErrUnavailable }
