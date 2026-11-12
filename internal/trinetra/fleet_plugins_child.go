package trinetra

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/InfoDiveLabs/trinetra/internal/fleet"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const (
	phaseOffered     = "offered"
	phaseDownloading = "downloading"
	phaseInstalled   = "installed"
	phaseEnabled     = "enabled"
	phaseFailed      = "failed"
	phaseRemoved     = "removed"
)

type pluginChild struct {
	lifecycle       func() plugin.Lifecycle
	registry        func() plugin.Registry
	allowUnverified func() bool
	masterID        string
	dir             string
	logf            func(string, ...any)

	opMu sync.Mutex
	wg   sync.WaitGroup

	mu       sync.Mutex
	reports  map[string]fleet.PluginReport
	inflight map[string]inflightDownload
}

type inflightDownload struct {
	id     string
	cancel context.CancelFunc
}

var activePluginChild atomic.Pointer[pluginChild]

func handleChildPluginFrame(fetch chunkFetcher, f fleet.Frame) {
	if !strings.HasPrefix(f.Type, "plugin_") {
		return
	}
	if c := activePluginChild.Load(); c != nil {
		c.HandleFrame(f, fetch)
	}
}

func childPluginReports() []fleet.PluginReport {
	if c := activePluginChild.Load(); c != nil {
		return c.Reports()
	}
	return nil
}

func newPluginChild(lifecycle func() plugin.Lifecycle, registry func() plugin.Registry, allowUnverified func() bool, masterID, dir string, logf func(string, ...any)) *pluginChild {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &pluginChild{lifecycle: lifecycle, registry: registry, allowUnverified: allowUnverified, masterID: masterID, dir: dir, logf: logf,
		reports: map[string]fleet.PluginReport{}, inflight: map[string]inflightDownload{}}
}

func (c *pluginChild) via() string { return "fleet:" + c.masterID }

func (c *pluginChild) report(r fleet.PluginReport) {
	c.mu.Lock()
	c.reports[r.ID] = r
	c.mu.Unlock()
}

// Reports returns the per-plugin push progress, sorted by plugin id.
func (c *pluginChild) Reports() []fleet.PluginReport {
	c.mu.Lock()
	out := make([]fleet.PluginReport, 0, len(c.reports))
	for _, r := range c.reports {
		out = append(out, r)
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// HandleFrame processes a plugin_* frame from the master; slow work runs in goroutines so the stream read loop never blocks.
func (c *pluginChild) HandleFrame(f fleet.Frame, fetch chunkFetcher) {
	switch f.Type {
	case fleet.FramePluginOffer:
		var o fleet.PluginOffer
		if json.Unmarshal(f.Data, &o) == nil {
			c.handleOffer(o, fetch)
		}
	case fleet.FramePluginRemove:
		var r fleet.PluginRemove
		if json.Unmarshal(f.Data, &r) == nil {
			c.handleRemove(r)
		}
	case fleet.FramePluginRevoke:
		var r fleet.PluginRevoke
		if json.Unmarshal(f.Data, &r) == nil {
			c.handleRevoke(r)
		}
	}
}

func (c *pluginChild) handleOffer(o fleet.PluginOffer, fetch chunkFetcher) {
	rep := fleet.PluginReport{ID: o.ID, Version: o.Version, Phase: phaseOffered, Size: o.Size}
	switch o.Trust {
	case plugin.TrustVerified:
	case plugin.TrustUnverified:
		if c.allowUnverified == nil || !c.allowUnverified() {
			rep.Phase, rep.Error = phaseFailed, "unverified plugins are disabled on this node"
			c.report(rep)
			return
		}
	default:
		rep.Phase, rep.Error = phaseFailed, "unsupported trust level for a pushed plugin"
		c.report(rep)
		return
	}
	sha := strings.ToLower(o.SHA256)
	if !sha256Pattern.MatchString(sha) {
		rep.Phase, rep.Error = phaseFailed, "invalid package digest"
		c.report(rep)
		return
	}
	if lc := c.lifecycle(); lc != nil {
		if st, err := lc.Get(o.ID); err == nil && st.PackageSHA256 == sha {
			rep.Phase, rep.State = phaseInstalled, st.State
			if st.State == plugin.StateEnabled {
				rep.Phase = phaseEnabled
			}
			c.report(rep)
			return
		}
	}
	c.mu.Lock()
	if _, busy := c.inflight[sha]; busy {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.inflight[sha] = inflightDownload{id: o.ID, cancel: cancel}
	c.reports[o.ID] = rep
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			cancel()
			c.mu.Lock()
			delete(c.inflight, sha)
			c.mu.Unlock()
		}()
		c.install(ctx, o, sha, fetch)
	}()
}

func (c *pluginChild) install(ctx context.Context, o fleet.PluginOffer, sha string, fetch chunkFetcher) {
	rep := fleet.PluginReport{ID: o.ID, Version: o.Version, Phase: phaseDownloading, Size: o.Size}
	fail := func(err error) {
		rep.Phase, rep.Error = phaseFailed, err.Error()
		c.report(rep)
		c.logf("plugins: push of %s from %s failed: %v", o.ID, c.via(), err)
	}
	c.report(rep)
	path, err := downloadPackage(ctx, fetch, c.dir, sha, o.Size, func(n int64) {
		rep.Received = n
		c.report(rep)
	})
	if err != nil {
		fail(err)
		return
	}
	defer os.Remove(path)

	c.opMu.Lock()
	defer c.opMu.Unlock()
	if ctx.Err() != nil {
		fail(ctx.Err())
		return
	}
	lc := c.lifecycle()
	if lc == nil {
		fail(plugin.ErrUnavailable)
		return
	}
	consent := o.Consent
	consent.Via = c.via()
	res, err := lc.Install(plugin.InstallRequest{
		Ref: path, Actor: "fleet", Via: c.via(), ExpectSHA256: sha, Consent: &consent,
		RequireVerified: o.Trust == plugin.TrustVerified,
	})
	if err != nil {
		fail(err)
		return
	}
	rep.Version, rep.Phase, rep.State = res.Version, phaseInstalled, plugin.StateInstalled
	if res.Enabled {
		rep.State = plugin.StateEnabled
	}
	c.report(rep)
	if len(o.Config) > 0 {
		if err := lc.SetConfig(plugin.SetConfigRequest{ID: o.ID, Values: o.Config, Actor: "fleet", Via: c.via()}); err != nil {
			fail(err)
			return
		}
	}
	if !res.Enabled {
		err := lc.Enable(plugin.EnableRequest{ID: o.ID, Actor: "fleet", Via: c.via(), Accepted: consent.Permissions, AcknowledgedRisk: consent.Unverified})
		if err != nil {
			fail(err)
			return
		}
		rep.State = plugin.StateEnabled
	}
	rep.Phase = phaseEnabled
	rep.Received = rep.Size
	c.report(rep)
}

func (c *pluginChild) handleRemove(r fleet.PluginRemove) {
	c.mu.Lock()
	for _, d := range c.inflight {
		if d.id == r.ID {
			d.cancel()
		}
	}
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.opMu.Lock()
		defer c.opMu.Unlock()
		rep := fleet.PluginReport{ID: r.ID, Phase: phaseRemoved}
		lc := c.lifecycle()
		if lc == nil {
			rep.Phase, rep.Error = phaseFailed, plugin.ErrUnavailable.Error()
		} else if err := lc.Remove(r.ID, "fleet", c.via()); err != nil && !errors.Is(err, plugin.ErrNotFound) {
			rep.Phase, rep.Error = phaseFailed, err.Error()
		}
		c.report(rep)
	}()
}

func (c *pluginChild) handleRevoke(r fleet.PluginRevoke) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.opMu.Lock()
		defer c.opMu.Unlock()
		lc := c.lifecycle()
		if lc == nil {
			return
		}
		list, err := lc.List()
		if err != nil {
			return
		}
		for _, rv := range r.Revocations {
			for _, st := range list {
				if st.ID != rv.ID || (len(rv.Versions) > 0 && !slices.Contains(rv.Versions, st.Version)) {
					continue
				}
				if err := lc.Disable(st.ID, "fleet", c.via()); err != nil {
					c.logf("plugins: revoke of %s %s from %s failed: %v", st.ID, st.Version, c.via(), err)
					continue
				}
				c.report(fleet.PluginReport{ID: st.ID, Version: st.Version, State: plugin.StateRevoked, Phase: phaseFailed, Error: "revoked: " + rv.Reason})
			}
		}
	}()
}

type chunkFetcher func(ctx context.Context, sha string, off int64) ([]byte, int64, error)

// downloadPackage resumes <dir>/<sha>.part from its current size, verifies sha256 at the end, and returns the final path (<dir>/<sha>.tar.gz).
func downloadPackage(ctx context.Context, fetch chunkFetcher, dir, sha string, size int64, progress func(received int64)) (string, error) {
	sha = strings.ToLower(sha)
	if !sha256Pattern.MatchString(sha) {
		return "", errors.New("invalid package digest")
	}
	if size <= 0 || size > fleet.MaxPluginPackage {
		return "", fmt.Errorf("package size %d is outside the allowed range", size)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	part := filepath.Join(dir, sha+".part")
	final := filepath.Join(dir, sha+".tar.gz")
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(part)
		return "", err
	}
	off, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return "", err
	}
	if off > size {
		if err := f.Truncate(0); err != nil {
			return fail(err)
		}
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	if progress != nil {
		progress(off)
	}
	for off < size {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		chunk, total, err := fetch(ctx, sha, off)
		if err != nil {
			return "", err
		}
		if total != size {
			return fail(fmt.Errorf("package size changed from %d to %d during transfer", size, total))
		}
		if len(chunk) == 0 {
			return "", errors.New("master ended the transfer early")
		}
		if off+int64(len(chunk)) > size {
			return fail(errors.New("package is larger than offered"))
		}
		if _, err := f.Write(chunk); err != nil {
			return "", err
		}
		off += int64(len(chunk))
		if progress != nil {
			progress(off)
		}
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != sha {
		return fail(errors.New("package digest mismatch"))
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	return final, nil
}
