package fleet

import (
	"io"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

// Plugin push frame types and the package transfer endpoint.
const (
	FramePluginOffer  = "plugin_offer"
	FramePluginRemove = "plugin_remove"
	FramePluginRevoke = "plugin_revoke"
	PathPluginPackage = "/fleet/v1/plugin-pkg/" // GET {sha256}?offset=N
	PluginChunkSize   = 256 << 10
	MaxPluginPackage  = 64 << 20
)

// PluginOffer asks a child to fetch and install a package the master already consented to.
type PluginOffer struct {
	ID      string               `json:"id"`
	Version string               `json:"version"`
	SHA256  string               `json:"sha256"`
	Size    int64                `json:"size"`
	Trust   plugin.Trust         `json:"trust"`
	Consent plugin.ConsentRecord `json:"consent"`
	Config  map[string]string    `json:"config,omitempty"`
}

// PluginRemove asks a child to remove a plugin.
type PluginRemove struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// PluginRevoke tells a child which plugin versions are revoked.
type PluginRevoke struct {
	Revocations []plugin.Revocation `json:"revocations"`
}

// PluginReport is a child's per-plugin push progress, carried on LiveUpdate.
type PluginReport struct {
	ID       string       `json:"id"`
	Version  string       `json:"version,omitempty"`
	State    plugin.State `json:"state,omitempty"`
	Phase    string       `json:"phase"`
	Received int64        `json:"received,omitempty"`
	Size     int64        `json:"size,omitempty"`
	Error    string       `json:"error,omitempty"`
}

// PluginPackageSource serves offered packages; Open fails unless nodeID has an outstanding offer for sha.
type PluginPackageSource interface {
	Open(nodeID, sha string) (io.ReadSeekCloser, int64, error)
}
