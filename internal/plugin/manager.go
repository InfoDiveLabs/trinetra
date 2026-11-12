package plugin

import "encoding/json"

type InstallRequest struct {
	Ref             string         `json:"ref"` // id[@version] | /abs/path.tar.gz | https://...
	Actor           string         `json:"actor"`
	Via             string         `json:"via"`
	ExpectSHA256    string         `json:"expect_sha256,omitempty"`
	Consent         *ConsentRecord `json:"consent,omitempty"`
	RequireVerified bool           `json:"require_verified,omitempty"`
}

type InstallResult struct {
	ID             string       `json:"id"`
	Version        string       `json:"version"`
	Trust          Trust        `json:"trust"`
	Permissions    []Permission `json:"permissions"`
	NewPermissions []Permission `json:"new_permissions,omitempty"`
	Enabled        bool         `json:"enabled"`
	Updated        bool         `json:"updated"`
}

type EnableRequest struct {
	ID               string       `json:"id"`
	Actor            string       `json:"actor"`
	Via              string       `json:"via"`
	Accepted         []Permission `json:"accepted"`
	AcknowledgedRisk bool         `json:"acknowledged_risk"`
	Pending          bool         `json:"pending,omitempty"` // consent applies to the staged update
}

type UpdateRequest struct {
	ID    string `json:"id"` // "" = all
	Actor string `json:"actor"`
	Via   string `json:"via"`
}

type UpdateResult struct {
	ID           string       `json:"id"`
	From         string       `json:"from"`
	To           string       `json:"to"`
	NeedsConsent []Permission `json:"needs_consent,omitempty"`
	Error        string       `json:"error,omitempty"`
}

type SetConfigRequest struct {
	ID     string            `json:"id"`
	Values map[string]string `json:"values"` // string form; array = newline-joined; empty secret = keep
	Unset  []string          `json:"unset,omitempty"`
	Actor  string            `json:"actor"`
	Via    string            `json:"via"`
}

type PackageInfo struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Trust   Trust  `json:"trust"`
}

type DevRequest struct {
	Dir   string `json:"dir"`
	Actor string `json:"actor"`
}

type DevSession struct {
	ID           string `json:"id"`
	Token        string `json:"token"`
	HostSocket   string `json:"host_socket"`
	PluginSocket string `json:"plugin_socket"`
}

type UIRequest struct {
	PluginID string      `json:"plugin_id"`
	Node     string      `json:"node,omitempty"`
	Request  SlotRequest `json:"request"`
}

type UIActionRequest struct {
	PluginID string        `json:"plugin_id"`
	Node     string        `json:"node,omitempty"`
	Request  ActionRequest `json:"request"`
}

type SlotOffer struct {
	PluginID   string `json:"plugin_id"`
	PluginName string `json:"plugin_name"`
	Slot       Slot   `json:"slot"`
	SlotID     string `json:"slot_id"`
	Title      string `json:"title"`
	MinRole    Role   `json:"min_role"`
}

type CatalogEntry struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	Publisher       string       `json:"publisher"`
	Description     string       `json:"description"`
	Homepage        string       `json:"homepage,omitempty"`
	Latest          string       `json:"latest"`
	Permissions     []Permission `json:"permissions"`
	Installed       string       `json:"installed,omitempty"`
	UpdateAvailable bool         `json:"update_available,omitempty"`
	Revoked         string       `json:"revoked,omitempty"`
}

type Catalog struct {
	Serial  int64          `json:"serial"`
	Fetched int64          `json:"fetched"`
	Expires int64          `json:"expires"`
	Stale   bool           `json:"stale"`
	Error   string         `json:"error,omitempty"`
	Entries []CatalogEntry `json:"entries"`
}

type PackageRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

type Revocation struct {
	ID       string   `json:"id"`
	Versions []string `json:"versions,omitempty"` // empty = every version
	Reason   string   `json:"reason"`
}

type PushRequest struct {
	ID               string       `json:"id"`
	Nodes            []string     `json:"nodes,omitempty"`
	Tag              string       `json:"tag,omitempty"`
	Actor            string       `json:"actor"`
	Via              string       `json:"via"`
	Remove           bool         `json:"remove,omitempty"`
	AcknowledgedRisk bool         `json:"acknowledged_risk,omitempty"`
	Accepted         []Permission `json:"accepted,omitempty"`
}

type NodeState struct {
	Node     string `json:"node"`
	NodeName string `json:"node_name"`
	Version  string `json:"version,omitempty"`
	State    State  `json:"state,omitempty"`
	Phase    string `json:"phase"` // offered | downloading | installed | enabled | failed | removed
	Received int64  `json:"received,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Error    string `json:"error,omitempty"`
}

type PushResult struct {
	Targets []NodeState `json:"targets"`
}

// Lifecycle is install/consent/config management (runtime track).
type Lifecycle interface {
	List() ([]Status, error)
	Get(id string) (Status, error)
	Install(InstallRequest) (InstallResult, error)
	Enable(EnableRequest) error
	Disable(id, actor, via string) error
	Remove(id, actor, via string) error
	Update(UpdateRequest) ([]UpdateResult, error)
	SetConfig(SetConfigRequest) error
	Config(id string) (RedactedConfig, error)
	File(id, name string) (FileDecl, []byte, error)
	PackageFile(id string) (PackageInfo, error)
	DevAttach(DevRequest) (DevSession, error)
	DevDetach(id string) error
}

// UI renders slots and runs actions on this node (hostapi track).
type UI interface {
	Slots(slot Slot) ([]SlotOffer, error)
	RenderSlot(UIRequest) (RenderResult, error)
	InvokeAction(UIActionRequest) (ActionResult, error)
}

// CatalogSource reads the signed registry index (registry track).
type CatalogSource interface {
	Catalog(refresh bool) (Catalog, error)
}

// FleetOps pushes plugins to children and relays child UI (fleet track).
type FleetOps interface {
	Push(PushRequest) (PushResult, error)
	NodeStates(id string) ([]NodeState, error)
	RelaySlots(node string, slot Slot) ([]SlotOffer, error)
	RelayRender(UIRequest) (RenderResult, error)
	RelayAction(UIActionRequest) (ActionResult, error)
}

// Manager is the Plugins.* management surface.
type Manager interface {
	Lifecycle
	CatalogSource
	Slots(node string, slot Slot) ([]SlotOffer, error)
	RenderSlot(UIRequest) (RenderResult, error)
	InvokeAction(UIActionRequest) (ActionResult, error)
	Push(PushRequest) (PushResult, error)
	NodeStates(id string) ([]NodeState, error)
}

// ManagerProvider is implemented by a core.API that serves Plugins.*.
type ManagerProvider interface {
	Plugins() Manager
}

// Registry verifies registry signatures and resolves catalog packages (registry track).
type Registry interface {
	VerifyManifest(manifest, sig []byte) error
	Resolve(id, version, goos, goarch string) (PackageRef, error)
	Latest(id string) (version string, ok bool)
	Revocation(id, version string) (reason string, revoked bool)
	OnRevoke(func([]Revocation))
	CatalogSource
}

// Inventory is the runtime's read view of installed plugins.
type Inventory interface {
	Get(id string) (Installed, bool)
	Enabled() []Installed
	ConfigValues(id string) (ConfigResponse, error)
	Endpoint(id string) (socket, token string, ok bool)
}

// HostRegistry lets the runtime admit plugins to the host API (hostapi track).
type HostRegistry interface {
	Register(id string, granted []Permission, dev bool) (token string, err error)
	Unregister(id string)
	NotifyConfig(id string, cfg ConfigResponse)
	ValidateConfig(id string, values map[string]json.RawMessage) (FieldErrors, error)
	Health(id string) Health
	ResolveAll(id, reason string)
	HostSocket() string
}
