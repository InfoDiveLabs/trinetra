package fleet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

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

// HeaderPluginSize carries the total package size on a chunk response.
const HeaderPluginSize = "X-Trinetra-Size"

func (m *Master) handlePluginPackage(w http.ResponseWriter, r *http.Request, nodeID string) {
	src := m.cfg.PluginPackages
	if src == nil {
		http.NotFound(w, r)
		return
	}
	var offset int64
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "bad offset", http.StatusBadRequest)
			return
		}
		offset = n
	}
	rc, size, err := src.Open(nodeID, r.PathValue("sha"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer rc.Close()
	if offset > size {
		http.Error(w, "offset beyond end", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set(HeaderPluginSize, strconv.FormatInt(size, 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	if offset == size {
		return
	}
	if _, err := rc.Seek(offset, io.SeekStart); err != nil {
		http.Error(w, "seek failed", http.StatusInternalServerError)
		return
	}
	buf := make([]byte, min(int64(PluginChunkSize), size-offset))
	n, err := io.ReadFull(rc, buf)
	if err != nil && n == 0 {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(n))
	_, _ = w.Write(buf[:n])
}

// FetchPluginChunk reads one chunk of the offered package sha starting at offset and returns it with the package's total size.
func (s *Shipper) FetchPluginChunk(ctx context.Context, sha string, offset int64) ([]byte, int64, error) {
	u := s.cfg.MasterURL + PathPluginPackage + url.PathEscape(sha) + "?offset=" + strconv.FormatInt(offset, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("fleet: plugin package fetch returned %d", resp.StatusCode)
	}
	size, err := strconv.ParseInt(resp.Header.Get(HeaderPluginSize), 10, 64)
	if err != nil || size < 0 {
		return nil, 0, errors.New("fleet: plugin package response lacks a valid size")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, PluginChunkSize+1))
	if err != nil {
		return nil, 0, err
	}
	if len(b) > PluginChunkSize {
		return nil, 0, errors.New("fleet: plugin chunk exceeds the chunk size")
	}
	return b, size, nil
}
