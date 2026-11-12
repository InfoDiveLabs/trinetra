package registry

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

const refreshEvery = 24 * time.Hour

// Options configures a Registry.
type Options struct {
	Dir       string // <stateDir>/plugins/registry (index.json, index.json.sig, state.json)
	BaseURL   func() string
	Keys      []ed25519.PublicKey
	HTTP      *http.Client // 30s timeout
	Now       func() time.Time
	Installed func() map[string]string // id -> version, for catalog flags
	Logf      func(string, ...any)
}

// Registry serves the verified plugin catalog.
type Registry struct {
	o Options

	mu        sync.Mutex
	index     *Index
	state     State
	lastErr   string
	onRevoke  []func([]plugin.Revocation)
	refreshMu sync.Mutex
}

var _ plugin.Registry = (*Registry)(nil)

// New builds a Registry and loads the cached index, if one verifies.
func New(o Options) *Registry {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	r := &Registry{o: o}
	r.loadCache()
	return r
}

func (r *Registry) path(name string) string { return filepath.Join(r.o.Dir, name) }

func (r *Registry) loadCache() {
	if r.o.Dir == "" || len(r.o.Keys) == 0 {
		return
	}
	st, err := LoadState(r.path("state.json"))
	if err != nil {
		r.o.Logf("registry: state: %v", err)
		return
	}
	b, err := os.ReadFile(r.path("index.json"))
	if err != nil {
		return
	}
	sig, err := os.ReadFile(r.path("index.json.sig"))
	if err != nil {
		return
	}
	ix, nst, err := verifyIndex(r.o.Keys, b, sig, r.o.Now(), st, false)
	if err != nil {
		r.o.Logf("registry: cached index rejected: %v", err)
		return
	}
	nst.FetchedAt = st.FetchedAt
	r.index, r.state = &ix, nst
}

// Refresh fetches index.json(.sig) from BaseURL, verifies, persists, and fires OnRevoke callbacks with the full revoke list.
func (r *Registry) Refresh(ctx context.Context) error {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	err := r.refresh(ctx)
	r.mu.Lock()
	if err != nil {
		r.lastErr = err.Error()
	} else {
		r.lastErr = ""
	}
	r.mu.Unlock()
	if err != nil && !errors.Is(err, plugin.ErrNoRegistryKeys) {
		r.o.Logf("registry: refresh failed: %v", err)
	}
	return err
}

func (r *Registry) refresh(ctx context.Context) error {
	if len(r.o.Keys) == 0 {
		return plugin.ErrNoRegistryKeys
	}
	b, sig, err := r.fetchIndex(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	cur := r.state
	r.mu.Unlock()
	ix, st, err := VerifyIndex(r.o.Keys, b, sig, r.o.Now(), cur)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.index, r.state = &ix, st
	r.mu.Unlock()
	if err := r.persist(b, sig, st); err != nil {
		return err
	}
	r.fireRevoke(ix.Revoked)
	return nil
}

func (r *Registry) persist(index, sig []byte, st State) error {
	if r.o.Dir == "" {
		return nil
	}
	if err := os.MkdirAll(r.o.Dir, 0o700); err != nil {
		return err
	}
	if err := update.WriteFileAtomic(r.path("index.json"), index, 0o600); err != nil {
		return err
	}
	if err := update.WriteFileAtomic(r.path("index.json.sig"), sig, 0o600); err != nil {
		return err
	}
	return SaveState(r.path("state.json"), st)
}

func (r *Registry) fireRevoke(list []plugin.Revocation) {
	if len(list) == 0 {
		return
	}
	r.mu.Lock()
	cbs := append([]func([]plugin.Revocation){}, r.onRevoke...)
	r.mu.Unlock()
	for _, cb := range cbs {
		cb(append([]plugin.Revocation(nil), list...))
	}
}

// Run refreshes at start (if cache older than 24h) and every 24h ± 1h jitter until ctx ends.
func (r *Registry) Run(ctx context.Context) {
	r.mu.Lock()
	var cached []plugin.Revocation
	fresh := r.index != nil && r.o.Now().Sub(time.Unix(r.state.FetchedAt, 0)) < refreshEvery
	if r.index != nil {
		cached = r.index.Revoked
	}
	r.mu.Unlock()
	r.fireRevoke(cached)
	if !fresh {
		_ = r.Refresh(ctx)
	}
	for {
		jitter := time.Duration(rand.Int64N(int64(2*time.Hour))) - time.Hour
		t := time.NewTimer(refreshEvery + jitter)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		_ = r.Refresh(ctx)
	}
}

// OnRevoke registers a callback fired with the full revoke list after each refresh.
func (r *Registry) OnRevoke(f func([]plugin.Revocation)) {
	r.mu.Lock()
	r.onRevoke = append(r.onRevoke, f)
	r.mu.Unlock()
}

// VerifyManifest checks a plugin.json signature against the registry keys.
func (r *Registry) VerifyManifest(manifest, sig []byte) error {
	return VerifyManifest(r.o.Keys, manifest, sig)
}

func (r *Registry) stale() bool {
	return r.index == nil || !r.o.Now().Before(time.Unix(r.state.Expires, 0))
}

func (r *Registry) revocationLocked(id, version string) (string, bool) {
	if r.index == nil {
		return "", false
	}
	for _, rv := range r.index.Revoked {
		if rv.ID != id {
			continue
		}
		if len(rv.Versions) == 0 {
			return rv.Reason, true
		}
		for _, v := range rv.Versions {
			if sameVersion(v, version) {
				return rv.Reason, true
			}
		}
	}
	return "", false
}

func sameVersion(a, b string) bool {
	va, err1 := update.ParseVersion(a)
	vb, err2 := update.ParseVersion(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return update.CompareVersions(va, vb) == 0
}

// Revocation reports whether the registry revokes id at version.
func (r *Registry) Revocation(id, version string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revocationLocked(id, version)
}

func (r *Registry) findPlugin(id string) *IndexPlugin {
	if r.index == nil {
		return nil
	}
	for i := range r.index.Plugins {
		if r.index.Plugins[i].ID == id {
			return &r.index.Plugins[i]
		}
	}
	return nil
}

// latestLocked prefers the highest unrevoked version, falling back to the highest overall.
func (r *Registry) latestLocked(p *IndexPlugin) (IndexVersion, bool) {
	var best, bestAny *IndexVersion
	var bv, bav update.Version
	for i := range p.Versions {
		v := &p.Versions[i]
		pv, err := update.ParseVersion(v.Version)
		if err != nil {
			continue
		}
		if bestAny == nil || update.CompareVersions(pv, bav) > 0 {
			bestAny, bav = v, pv
		}
		if _, rev := r.revocationLocked(p.ID, v.Version); rev {
			continue
		}
		if best == nil || update.CompareVersions(pv, bv) > 0 {
			best, bv = v, pv
		}
	}
	if best != nil {
		return *best, true
	}
	if bestAny != nil {
		return *bestAny, true
	}
	return IndexVersion{}, false
}

// Latest returns the newest unrevoked version of id.
func (r *Registry) Latest(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.findPlugin(id)
	if p == nil {
		return "", false
	}
	v, ok := r.latestLocked(p)
	return v.Version, ok
}

// Resolve finds the package of id@version (empty = latest) for goos/goarch.
func (r *Registry) Resolve(id, version, goos, goarch string) (plugin.PackageRef, error) {
	if len(r.o.Keys) == 0 {
		return plugin.PackageRef{}, plugin.ErrNoRegistryKeys
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stale() {
		return plugin.PackageRef{}, plugin.ErrCatalogStale
	}
	p := r.findPlugin(id)
	if p == nil {
		return plugin.PackageRef{}, fmt.Errorf("%w: %q is not in the registry", plugin.ErrNotFound, id)
	}
	var v IndexVersion
	if version == "" {
		lv, ok := r.latestLocked(p)
		if !ok {
			return plugin.PackageRef{}, fmt.Errorf("%w: %q has no versions", plugin.ErrNotFound, id)
		}
		v = lv
	} else {
		found := false
		for _, c := range p.Versions {
			if sameVersion(c.Version, version) {
				v, found = c, true
				break
			}
		}
		if !found {
			return plugin.PackageRef{}, fmt.Errorf("%w: %s@%s is not in the registry", plugin.ErrNotFound, id, version)
		}
	}
	if reason, rev := r.revocationLocked(id, v.Version); rev {
		return plugin.PackageRef{}, fmt.Errorf("%w: %s@%s: %s", plugin.ErrRevoked, id, v.Version, reason)
	}
	for _, pk := range v.Packages {
		if pk.OS == goos && pk.Arch == goarch {
			return plugin.PackageRef{ID: id, Version: v.Version, URL: pk.URL, SHA256: pk.SHA256, Size: pk.Size}, nil
		}
	}
	return plugin.PackageRef{}, fmt.Errorf("%w: %s@%s has no package for %s/%s", plugin.ErrNotFound, id, v.Version, goos, goarch)
}

// Catalog lists the registry's plugins; with refresh it fetches a new index first.
func (r *Registry) Catalog(refresh bool) (plugin.Catalog, error) {
	if refresh {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		_ = r.Refresh(ctx)
		cancel()
	}
	var installed map[string]string
	if r.o.Installed != nil {
		installed = r.o.Installed()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c := plugin.Catalog{Stale: r.stale(), Error: r.lastErr, Entries: []plugin.CatalogEntry{}}
	if len(r.o.Keys) == 0 {
		c.Error = plugin.ErrNoRegistryKeys.Error()
		return c, nil
	}
	if r.index == nil {
		return c, nil
	}
	c.Serial, c.Fetched, c.Expires = r.index.Serial, r.state.FetchedAt, r.state.Expires
	for i := range r.index.Plugins {
		p := &r.index.Plugins[i]
		lv, ok := r.latestLocked(p)
		if !ok {
			continue
		}
		e := plugin.CatalogEntry{
			ID: p.ID, Name: p.Name, Publisher: p.Publisher, Description: p.Description, Homepage: p.Homepage,
			Latest: lv.Version, Permissions: lv.Permissions, Installed: installed[p.ID],
		}
		if e.Permissions == nil {
			e.Permissions = []plugin.Permission{}
		}
		if reason, rev := r.revocationLocked(p.ID, lv.Version); rev {
			e.Revoked = reason
		} else if e.Installed != "" {
			if reason, rev := r.revocationLocked(p.ID, e.Installed); rev {
				e.Revoked = reason
			}
		}
		if e.Installed != "" {
			iv, err1 := update.ParseVersion(e.Installed)
			lvv, err2 := update.ParseVersion(lv.Version)
			e.UpdateAvailable = err1 == nil && err2 == nil && update.CompareVersions(lvv, iv) > 0 && e.Revoked == ""
		}
		c.Entries = append(c.Entries, e)
	}
	sort.Slice(c.Entries, func(i, j int) bool { return c.Entries[i].ID < c.Entries[j].ID })
	return c, nil
}
