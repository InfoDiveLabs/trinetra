package pluginrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

const (
	maxFileBytes   = 1 << 20
	fetchTimeout   = 5 * time.Minute
	pluginSockRoot = "/run/trinetra-plugin"
)

// PluginSocketPath is where a sandboxed plugin serves core calls.
func PluginSocketPath(id string) string { return pluginSockRoot + "/" + id + "/plugin.sock" }

// Supervisor runs plugin processes (implemented in internal/trinetra).
type Supervisor interface {
	Start(rec plugin.Installed, binPath, token string) error
	Stop(id string) error
	Running(id string) (running bool, restarts int)
}

// Deps are the Manager's collaborators.
type Deps struct {
	Store           *Store
	Registry        func() plugin.Registry
	Host            func() plugin.HostRegistry
	Supervisor      Supervisor
	Fetch           func(ctx context.Context, url string) (io.ReadCloser, error)
	AllowUnverified func() bool
	CoreVersion     string
	GOOS, GOARCH    string
	Now             func() time.Time
	Validate        func(*plugin.ConfigSchema, map[string]json.RawMessage) plugin.FieldErrors
	Coerce          func(*plugin.ConfigSchema, map[string]string) (map[string]json.RawMessage, plugin.FieldErrors)
	WithDefaults    func(*plugin.ConfigSchema, map[string]json.RawMessage) map[string]json.RawMessage
	Redact          func(*plugin.ConfigSchema, map[string]json.RawMessage) (map[string]json.RawMessage, []string)
	Logf            func(string, ...any)
}

// Manager implements plugin.Lifecycle; Inventory() is its read view.
type Manager struct {
	d      Deps
	opMu   sync.Mutex
	rmu    sync.RWMutex
	tokens map[string]string
	dev    map[string]devSession
}

var (
	_ plugin.Lifecycle = (*Manager)(nil)
)

// NewManager returns a Manager over d.
func NewManager(d Deps) *Manager {
	if d.GOOS == "" {
		d.GOOS = goos()
	}
	if d.GOARCH == "" {
		d.GOARCH = goarch()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.AllowUnverified == nil {
		d.AllowUnverified = func() bool { return true }
	}
	if d.Registry == nil {
		d.Registry = func() plugin.Registry { return nil }
	}
	if d.Host == nil {
		d.Host = func() plugin.HostRegistry { return nil }
	}
	d.Store.SetPlatform(d.GOOS, d.GOARCH)
	return &Manager{d: d, tokens: map[string]string{}, dev: map[string]devSession{}}
}

func (m *Manager) reg() plugin.Registry {
	if m.d.Registry == nil {
		return nil
	}
	return m.d.Registry()
}

func (m *Manager) host() plugin.HostRegistry {
	if m.d.Host == nil {
		return nil
	}
	return m.d.Host()
}

func (m *Manager) now() int64 { return m.d.Now().Unix() }

func (m *Manager) audit(actor, via, action, id, version, detail string) {
	e := plugin.AuditEntry{TS: m.now(), Actor: actor, Via: via, Action: action, Plugin: id, Version: version, Detail: detail}
	if err := m.d.Store.AppendAudit(e); err != nil {
		m.d.Logf("plugins: audit %s %s: %v", action, id, err)
	}
}

func (m *Manager) binPath(rec plugin.Installed) string {
	return filepath.Join(m.d.Store.VersionDir(rec.ID, rec.Version), filepath.FromSlash(plugin.BinaryPath(m.d.GOOS, m.d.GOARCH, rec.ID)))
}

func (m *Manager) setToken(id, tok string) {
	m.rmu.Lock()
	m.tokens[id] = tok
	m.rmu.Unlock()
}

func (m *Manager) token(id string) (string, bool) {
	m.rmu.RLock()
	defer m.rmu.RUnlock()
	t, ok := m.tokens[id]
	return t, ok
}

func (m *Manager) startLocked(rec plugin.Installed) error {
	h := m.host()
	if h == nil {
		return plugin.ErrUnavailable
	}
	if m.d.Supervisor == nil {
		return plugin.ErrUnavailable
	}
	tok, err := h.Register(rec.ID, rec.Granted, false)
	if err != nil {
		return err
	}
	if _, err := m.d.Store.WriteToken(rec.ID, tok); err != nil {
		h.Unregister(rec.ID)
		return err
	}
	if err := m.d.Supervisor.Start(rec, m.binPath(rec), tok); err != nil {
		h.Unregister(rec.ID)
		return err
	}
	m.setToken(rec.ID, tok)
	return nil
}

func (m *Manager) stopLocked(id string) {
	if m.d.Supervisor != nil {
		_ = m.d.Supervisor.Stop(id)
	}
	if h := m.host(); h != nil {
		h.Unregister(id)
	}
	m.rmu.Lock()
	delete(m.tokens, id)
	m.rmu.Unlock()
}

// StartEnabled starts every enabled plugin (daemon boot): fresh token via Host().Register, token file, Supervisor.Start.
func (m *Manager) StartEnabled() {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	recs, err := m.d.Store.List()
	if err != nil {
		m.d.Logf("plugins: list: %v", err)
		return
	}
	for _, rec := range recs {
		if rec.State != plugin.StateEnabled {
			continue
		}
		if err := plugin.HostAPICompatible(rec.Manifest.HostAPI); err != nil {
			m.setStateLocked(rec, plugin.StateIncompatible, err.Error(), "plugin.failed")
			continue
		}
		if r := m.reg(); r != nil {
			if reason, revoked := r.Revocation(rec.ID, rec.Version); revoked {
				m.setStateLocked(rec, plugin.StateRevoked, reason, "plugin.revoked")
				continue
			}
		}
		if err := m.startLocked(rec); err != nil {
			m.d.Logf("plugins: start %s: %v", rec.ID, err)
			m.setStateLocked(rec, plugin.StateFailed, "start: "+err.Error(), "plugin.failed")
		}
	}
}

func (m *Manager) setStateLocked(rec plugin.Installed, st plugin.State, reason, action string) {
	rec.State, rec.Reason, rec.UpdatedAt = st, reason, m.now()
	if err := m.d.Store.Save(rec); err != nil {
		m.d.Logf("plugins: save %s: %v", rec.ID, err)
	}
	m.audit("system", "system", action, rec.ID, rec.Version, reason)
}

// StopAll stops every running plugin and releases their tokens.
func (m *Manager) StopAll() {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.rmu.RLock()
	ids := make([]string, 0, len(m.tokens))
	for id := range m.tokens {
		ids = append(ids, id)
	}
	m.rmu.RUnlock()
	for _, id := range ids {
		m.stopLocked(id)
	}
}

// ApplyRevocations disables matching plugins (state revoked, reason) and audits.
func (m *Manager) ApplyRevocations(rv []plugin.Revocation) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	recs, err := m.d.Store.List()
	if err != nil {
		return
	}
	for _, rec := range recs {
		for _, r := range rv {
			if r.ID != rec.ID || !(len(r.Versions) == 0 || containsStr(r.Versions, rec.Version)) {
				continue
			}
			if rec.State == plugin.StateRevoked {
				break
			}
			m.stopLocked(rec.ID)
			if h := m.host(); h != nil {
				h.ResolveAll(rec.ID, "plugin revoked")
			}
			rec.Granted = nil
			m.setStateLocked(rec, plugin.StateRevoked, r.Reason, "plugin.revoked")
			break
		}
	}
}

// MarkFailed sets state failed with reason (breaker trip).
func (m *Manager) MarkFailed(id, reason string) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	rec, err := m.d.Store.Load(id)
	if err != nil || rec.State != plugin.StateEnabled {
		return
	}
	if h := m.host(); h != nil {
		h.Unregister(id)
	}
	m.rmu.Lock()
	delete(m.tokens, id)
	m.rmu.Unlock()
	m.setStateLocked(rec, plugin.StateFailed, reason, "plugin.failed")
}

func containsStr(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

// Install implements plugin.Lifecycle.
func (m *Manager) Install(req plugin.InstallRequest) (plugin.InstallResult, error) {
	pk, err := m.fetchAndVerify(req)
	if err != nil {
		return plugin.InstallResult{}, err
	}
	defer pk.cleanup()
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.commitInstall(req, pk)
}

type verified struct {
	pkg     Package
	trust   plugin.Trust
	source  string
	cleanup func()
}

func parseRef(ref string) (id, ver string) {
	id, ver, _ = strings.Cut(ref, "@")
	return
}

func (m *Manager) open(req plugin.InstallRequest) (rc io.ReadCloser, source, wantSHA string, fromRegistry bool, cancel func(), err error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	ref := req.Ref
	switch {
	case strings.HasPrefix(ref, "http://"):
		cancel()
		return nil, "", "", false, nil, errors.New("plugin install: refusing plain http:// sources; use https://")
	case strings.HasPrefix(ref, "https://"):
		if m.d.Fetch == nil {
			cancel()
			return nil, "", "", false, nil, plugin.ErrUnavailable
		}
		rc, err = m.d.Fetch(ctx, ref)
		return rc, "url:" + ref, "", false, cancel, err
	case filepath.IsAbs(ref):
		f, ferr := os.Open(ref)
		if ferr != nil {
			cancel()
			return nil, "", "", false, nil, ferr
		}
		if st, serr := f.Stat(); serr != nil || !st.Mode().IsRegular() {
			f.Close()
			cancel()
			return nil, "", "", false, nil, fmt.Errorf("plugin install: %s is not a regular file", ref)
		}
		return f, "file:" + ref, "", false, cancel, nil
	}
	id, ver := parseRef(ref)
	if !plugin.IDPattern.MatchString(id) {
		cancel()
		return nil, "", "", false, nil, fmt.Errorf("plugin install: %q is not a plugin id, file path or https URL", ref)
	}
	r := m.reg()
	if r == nil {
		cancel()
		return nil, "", "", false, nil, plugin.ErrUnavailable
	}
	pr, rerr := r.Resolve(id, ver, m.d.GOOS, m.d.GOARCH)
	if rerr != nil {
		cancel()
		return nil, "", "", false, nil, rerr
	}
	if m.d.Fetch == nil {
		cancel()
		return nil, "", "", false, nil, plugin.ErrUnavailable
	}
	rc, err = m.d.Fetch(ctx, pr.URL)
	return rc, "registry", pr.SHA256, true, cancel, err
}

func (m *Manager) fetchAndVerify(req plugin.InstallRequest) (v *verified, err error) {
	rc, source, wantSHA, fromReg, cancel, err := m.open(req)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer rc.Close()
	stage, cleanup, err := m.d.Store.Staging()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	pkgDir := filepath.Join(stage, "pkg")
	if err = os.Mkdir(pkgDir, 0o755); err != nil {
		return nil, err
	}
	archive := filepath.Join(stage, ArchiveName)
	af, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	pkg, err := ReadPackage(io.TeeReader(rc, af), pkgDir)
	if cerr := af.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	pkg.Archive = archive
	for _, want := range []string{wantSHA, req.ExpectSHA256} {
		if want != "" && !strings.EqualFold(want, pkg.SHA256) {
			return nil, fmt.Errorf("plugin install: package sha256 %s does not match the expected %s", pkg.SHA256, want)
		}
	}
	if fromReg {
		if id, _ := parseRef(req.Ref); pkg.Manifest.ID != id {
			return nil, fmt.Errorf("plugin install: registry package contains %q, not %q", pkg.Manifest.ID, id)
		}
	}
	trust, err := Verify(pkg, m.reg(), m.d.GOOS, m.d.GOARCH, m.d.CoreVersion)
	if err != nil {
		return nil, err
	}
	if trust != plugin.TrustVerified {
		switch {
		case fromReg:
			return nil, fmt.Errorf("%w: registry package %s does not verify", plugin.ErrBadSignature, pkg.Manifest.ID)
		case req.RequireVerified:
			return nil, fmt.Errorf("%w: %s is not signed by a trusted key", plugin.ErrBadSignature, pkg.Manifest.ID)
		case !m.d.AllowUnverified():
			return nil, plugin.ErrUnverifiedDisallowed
		}
	}
	return &verified{pkg: pkg, trust: trust, source: source, cleanup: cleanup}, nil
}

func (m *Manager) commitInstall(req plugin.InstallRequest, v *verified) (plugin.InstallResult, error) {
	mf := v.pkg.Manifest
	res := plugin.InstallResult{ID: mf.ID, Version: mf.Version, Trust: v.trust, Permissions: mf.Permissions}
	if _, dev := m.devSession(mf.ID); dev {
		return res, fmt.Errorf("plugin %s is attached in dev mode", mf.ID)
	}
	cur, err := m.d.Store.Load(mf.ID)
	if err != nil && !errors.Is(err, plugin.ErrNotFound) {
		return res, err
	}
	known := err == nil
	if _, err := m.d.Store.Install(v.pkg, mf.Version); err != nil {
		return res, err
	}
	pending := false
	if !known {
		rec := plugin.Installed{ID: mf.ID, Version: mf.Version, Trust: v.trust, Manifest: mf, State: plugin.StateInstalled,
			Source: v.source, PackageSHA256: v.pkg.SHA256, InstalledAt: m.now(), UpdatedAt: m.now()}
		if err := m.d.Store.Save(rec); err != nil {
			return res, err
		}
		m.audit(req.Actor, req.Via, "plugin.install", rec.ID, rec.Version, string(v.trust)+" "+v.source)
	} else {
		pending, err = m.swapLocked(req, cur, v, &res)
		if err != nil {
			return res, err
		}
	}
	if req.Consent == nil {
		return res, nil
	}
	er := plugin.EnableRequest{ID: mf.ID, Actor: req.Actor, Via: req.Via, Accepted: req.Consent.Permissions,
		AcknowledgedRisk: req.Consent.Unverified, Pending: pending}
	if req.Consent.Actor != "" {
		er.Actor = req.Consent.Actor
	}
	if req.Consent.Via != "" {
		er.Via = req.Consent.Via
	}
	if err := m.enableLocked(er); err != nil {
		return res, err
	}
	res.Enabled = true
	return res, nil
}

func (m *Manager) swapLocked(req plugin.InstallRequest, cur plugin.Installed, v *verified, res *plugin.InstallResult) (pending bool, err error) {
	mf := v.pkg.Manifest
	res.Updated = true
	newPerms := PlanUpdate(cur, mf)
	downgrade := cur.Trust == plugin.TrustVerified && v.trust != plugin.TrustVerified
	if cur.State == plugin.StateEnabled && (len(newPerms) > 0 || downgrade) {
		cur.Pending = &plugin.PendingUpdate{Version: mf.Version, Manifest: mf, PackageSHA256: v.pkg.SHA256, Trust: v.trust, NewPermissions: newPerms}
		if err := m.d.Store.Save(cur); err != nil {
			return false, err
		}
		res.NewPermissions = newPerms
		res.Updated = false
		m.audit(req.Actor, req.Via, "plugin.update", cur.ID, mf.Version, "pending consent")
		return true, nil
	}
	wasEnabled := cur.State == plugin.StateEnabled
	old := cur.Version
	if wasEnabled {
		m.stopLocked(cur.ID)
	}
	cur.Version, cur.Manifest, cur.Trust, cur.Source = mf.Version, mf, v.trust, v.source
	cur.PackageSHA256, cur.UpdatedAt, cur.Pending = v.pkg.SHA256, m.now(), nil
	if !wasEnabled {
		cur.State, cur.Reason, cur.Granted = plugin.StateInstalled, "", nil
	}
	if wasEnabled {
		if err := m.startLocked(cur); err != nil {
			cur.Version, cur.Manifest = old, m.restoreManifest(cur.ID, old, cur.Manifest)
			m.d.Logf("plugins: update %s: start failed: %v", cur.ID, err)
			return false, fmt.Errorf("plugin %s: start after update: %w", cur.ID, err)
		}
	}
	if err := m.d.Store.Save(cur); err != nil {
		return false, err
	}
	_ = m.d.Store.PruneVersions(cur.ID, cur.Version)
	m.audit(req.Actor, req.Via, "plugin.update", cur.ID, mf.Version, "from "+old)
	return false, nil
}

func (m *Manager) restoreManifest(id, version string, fallback plugin.Manifest) plugin.Manifest {
	b, err := m.d.Store.ReadFile(id, version, "plugin.json", plugin.MaxManifestBytes)
	if err != nil {
		return fallback
	}
	if mf, err := plugin.DecodeManifest(b); err == nil {
		return mf
	}
	return fallback
}

// Enable implements plugin.Lifecycle.
func (m *Manager) Enable(req plugin.EnableRequest) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.enableLocked(req)
}

func (m *Manager) enableLocked(req plugin.EnableRequest) error {
	if _, dev := m.devSession(req.ID); dev {
		return fmt.Errorf("plugin %s is attached in dev mode", req.ID)
	}
	rec, err := m.d.Store.Load(req.ID)
	if err != nil {
		return err
	}
	switch rec.State {
	case plugin.StateRevoked:
		return fmt.Errorf("%w: %s", plugin.ErrRevoked, rec.Reason)
	case plugin.StateIncompatible:
		return fmt.Errorf("%w: %s", plugin.ErrIncompatible, rec.Reason)
	}
	consent, err := CheckEnable(rec, req, m.d.AllowUnverified(), m.now())
	if err != nil {
		return err
	}
	old := rec.Version
	next := rec
	if req.Pending {
		p := rec.Pending
		next.Version, next.Manifest, next.Trust, next.PackageSHA256 = p.Version, p.Manifest, p.Trust, p.PackageSHA256
		next.Pending = nil
	}
	if err := plugin.HostAPICompatible(next.Manifest.HostAPI); err != nil {
		return err
	}
	if r := m.reg(); r != nil {
		if reason, revoked := r.Revocation(next.ID, next.Version); revoked {
			return fmt.Errorf("%w: %s", plugin.ErrRevoked, reason)
		}
	}
	next.Granted = consent.Permissions
	next.Consents = append(append([]plugin.ConsentRecord(nil), rec.Consents...), consent)
	next.State, next.Reason, next.UpdatedAt = plugin.StateEnabled, "", m.now()
	m.stopLocked(next.ID)
	if err := m.startLocked(next); err != nil {
		return err
	}
	if err := m.d.Store.Save(next); err != nil {
		m.stopLocked(next.ID)
		return err
	}
	m.audit(req.Actor, req.Via, "plugin.consent", next.ID, next.Version, permList(consent.Permissions))
	m.audit(req.Actor, req.Via, "plugin.enable", next.ID, next.Version, "")
	if req.Pending && old != next.Version {
		_ = m.d.Store.PruneVersions(next.ID, next.Version)
		m.audit(req.Actor, req.Via, "plugin.update", next.ID, next.Version, "from "+old)
	}
	return nil
}

func permList(ps []plugin.Permission) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = string(p)
	}
	return strings.Join(s, ",")
}

// Disable implements plugin.Lifecycle.
func (m *Manager) Disable(id, actor, via string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.disableLocked(id, actor, via)
}

func (m *Manager) disableLocked(id, actor, via string) error {
	rec, err := m.d.Store.Load(id)
	if err != nil {
		return err
	}
	m.stopLocked(id)
	if h := m.host(); h != nil {
		h.ResolveAll(id, "plugin disabled")
	}
	if rec.State == plugin.StateEnabled || rec.State == plugin.StateFailed {
		rec.State, rec.Reason = plugin.StateInstalled, ""
	}
	rec.Granted, rec.UpdatedAt = nil, m.now()
	if err := m.d.Store.Save(rec); err != nil {
		return err
	}
	m.audit(actor, via, "plugin.disable", id, rec.Version, "")
	return nil
}

// Remove implements plugin.Lifecycle.
func (m *Manager) Remove(id, actor, via string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	rec, err := m.d.Store.Load(id)
	if err != nil {
		return err
	}
	if err := m.disableLocked(id, actor, via); err != nil {
		return err
	}
	if err := m.d.Store.Delete(id); err != nil {
		return err
	}
	m.audit(actor, via, "plugin.remove", id, rec.Version, "")
	return nil
}

// Update implements plugin.Lifecycle.
func (m *Manager) Update(req plugin.UpdateRequest) ([]plugin.UpdateResult, error) {
	r := m.reg()
	if r == nil {
		return nil, plugin.ErrUnavailable
	}
	var recs []plugin.Installed
	if req.ID != "" {
		rec, err := m.d.Store.Load(req.ID)
		if err != nil {
			return nil, err
		}
		recs = []plugin.Installed{rec}
	} else {
		all, err := m.d.Store.List()
		if err != nil {
			return nil, err
		}
		recs = all
	}
	var out []plugin.UpdateResult
	for _, rec := range recs {
		if rec.Source != "registry" {
			if req.ID != "" {
				out = append(out, plugin.UpdateResult{ID: rec.ID, From: rec.Version, To: rec.Version, Error: "not installed from the registry"})
			}
			continue
		}
		latest, ok := r.Latest(rec.ID)
		cmp := 0
		if ok {
			cmp = cmpVersion(latest, rec.Version)
		}
		if !ok || cmp <= 0 {
			if req.ID != "" {
				out = append(out, plugin.UpdateResult{ID: rec.ID, From: rec.Version, To: rec.Version})
			}
			continue
		}
		ur := plugin.UpdateResult{ID: rec.ID, From: rec.Version, To: latest}
		ir, err := m.Install(plugin.InstallRequest{Ref: rec.ID + "@" + latest, Actor: req.Actor, Via: req.Via, RequireVerified: true})
		if err != nil {
			ur.Error = err.Error()
		} else if !ir.Updated {
			ur.NeedsConsent = ir.NewPermissions
		}
		out = append(out, ur)
	}
	return out, nil
}

func cmpVersion(a, b string) int {
	va, ea := update.ParseVersion(a)
	vb, eb := update.ParseVersion(b)
	if ea != nil || eb != nil {
		return 0
	}
	return update.CompareVersions(va, vb)
}

func (m *Manager) withDefaults(s *plugin.ConfigSchema, v map[string]json.RawMessage) map[string]json.RawMessage {
	if m.d.WithDefaults == nil {
		return v
	}
	if r := m.d.WithDefaults(s, v); r != nil {
		return r
	}
	return v
}

// SetConfig implements plugin.Lifecycle.
func (m *Manager) SetConfig(req plugin.SetConfigRequest) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	rec, err := m.d.Store.Load(req.ID)
	if err != nil {
		return err
	}
	schema := rec.Manifest.ConfigSchema
	if schema == nil {
		return fmt.Errorf("plugin %s has no settings", req.ID)
	}
	in := make(map[string]string, len(req.Values))
	for k, v := range req.Values {
		if p := schema.Properties[k]; p != nil && p.Format == plugin.FormatSecret && v == "" {
			continue
		}
		in[k] = v
	}
	var typed map[string]json.RawMessage
	if m.d.Coerce != nil {
		var ferrs plugin.FieldErrors
		typed, ferrs = m.d.Coerce(schema, in)
		if len(ferrs) > 0 {
			return ferrs
		}
	}
	old, err := m.d.Store.ReadConfig(req.ID)
	if err != nil {
		return err
	}
	merged := make(map[string]json.RawMessage, len(old.Values)+len(typed))
	for k, v := range old.Values {
		merged[k] = v
	}
	for k, v := range typed {
		merged[k] = v
	}
	for _, k := range req.Unset {
		delete(merged, k)
	}
	if m.d.Validate != nil {
		if ferrs := m.d.Validate(schema, m.withDefaults(schema, merged)); len(ferrs) > 0 {
			return ferrs
		}
	}
	h := m.host()
	if running, _ := m.running(req.ID); running && h != nil {
		ferrs, err := h.ValidateConfig(req.ID, m.withDefaults(schema, merged))
		if err != nil && !errors.Is(err, plugin.ErrUnavailable) && !errors.Is(err, plugin.ErrNotRunning) {
			return err
		}
		if len(ferrs) > 0 {
			return ferrs
		}
	}
	if err := m.d.Store.WriteConfig(req.ID, plugin.ConfigResponse{Values: merged}); err != nil {
		return err
	}
	if h != nil {
		if cfg, err := m.ConfigValues(req.ID); err == nil {
			h.NotifyConfig(req.ID, cfg)
		}
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	detail := "set: " + strings.Join(keys, ",")
	if len(req.Unset) > 0 {
		un := append([]string(nil), req.Unset...)
		sort.Strings(un)
		detail += "; unset: " + strings.Join(un, ",")
	}
	m.audit(req.Actor, req.Via, "plugin.config", req.ID, rec.Version, detail)
	return nil
}

func (m *Manager) running(id string) (bool, int) {
	if m.d.Supervisor == nil {
		return false, 0
	}
	return m.d.Supervisor.Running(id)
}

// Config implements plugin.Lifecycle.
func (m *Manager) Config(id string) (plugin.RedactedConfig, error) {
	rec, ok := m.installed(id)
	if !ok {
		return plugin.RedactedConfig{}, plugin.ErrNotFound
	}
	cfg, err := m.d.Store.ReadConfig(id)
	if err != nil {
		return plugin.RedactedConfig{}, err
	}
	schema := rec.Manifest.ConfigSchema
	vals := m.withDefaults(schema, cfg.Values)
	var secrets []string
	if m.d.Redact != nil && schema != nil {
		if v, s := m.d.Redact(schema, vals); v != nil {
			vals, secrets = v, s
		}
	}
	if vals == nil {
		vals = map[string]json.RawMessage{}
	}
	return plugin.RedactedConfig{Schema: schema, Values: vals, SecretsSet: secrets, Revision: cfg.Revision}, nil
}

// File implements plugin.Lifecycle.
func (m *Manager) File(id, name string) (plugin.FileDecl, []byte, error) {
	rec, ok := m.installed(id)
	if !ok {
		return plugin.FileDecl{}, nil, plugin.ErrNotFound
	}
	for _, f := range rec.Manifest.Contributes.Files {
		if f.Name != name {
			continue
		}
		b, err := m.d.Store.ReadFile(id, rec.Version, "files/"+name, maxFileBytes)
		if err != nil {
			return plugin.FileDecl{}, nil, err
		}
		return f, b, nil
	}
	return plugin.FileDecl{}, nil, plugin.ErrNotFound
}

// PackageFile implements plugin.Lifecycle.
func (m *Manager) PackageFile(id string) (plugin.PackageInfo, error) {
	rec, err := m.d.Store.Load(id)
	if err != nil {
		return plugin.PackageInfo{}, err
	}
	p := filepath.Join(m.d.Store.VersionDir(id, rec.Version), ArchiveName)
	st, err := os.Stat(p)
	if err != nil {
		return plugin.PackageInfo{}, plugin.ErrNotFound
	}
	return plugin.PackageInfo{ID: id, Version: rec.Version, Path: p, SHA256: rec.PackageSHA256, Size: st.Size(), Trust: rec.Trust}, nil
}

// List implements plugin.Lifecycle.
func (m *Manager) List() ([]plugin.Status, error) {
	recs, err := m.d.Store.List()
	if err != nil {
		return nil, err
	}
	m.rmu.RLock()
	for _, s := range m.dev {
		recs = append(recs, s.rec)
	}
	m.rmu.RUnlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	out := make([]plugin.Status, len(recs))
	for i, rec := range recs {
		out[i] = m.status(rec)
	}
	return out, nil
}

// Get implements plugin.Lifecycle.
func (m *Manager) Get(id string) (plugin.Status, error) {
	rec, ok := m.installed(id)
	if !ok {
		return plugin.Status{}, plugin.ErrNotFound
	}
	return m.status(rec), nil
}

func (m *Manager) status(rec plugin.Installed) plugin.Status {
	st := plugin.Status{Installed: rec}
	st.Running, st.Restarts = m.running(rec.ID)
	if h := m.host(); h != nil {
		st.Health = h.Health(rec.ID)
	}
	if r := m.reg(); r != nil {
		if latest, ok := r.Latest(rec.ID); ok && cmpVersion(latest, rec.Version) > 0 {
			st.UpdateAvailable = latest
		}
		if reason, revoked := r.Revocation(rec.ID, rec.Version); revoked {
			st.RevokedReason = reason
		}
	}
	if rec.State == plugin.StateRevoked && st.RevokedReason == "" {
		st.RevokedReason = rec.Reason
	}
	return st
}

// installed returns the record for id, dev sessions included.
func (m *Manager) installed(id string) (plugin.Installed, bool) {
	if s, ok := m.devSession(id); ok {
		return s.rec, true
	}
	rec, err := m.d.Store.Load(id)
	return rec, err == nil
}

// Enabled lists enabled plugins, dev sessions included.
func (m *Manager) Enabled() []plugin.Installed {
	recs, _ := m.d.Store.List()
	out := recs[:0]
	for _, r := range recs {
		if r.State == plugin.StateEnabled {
			out = append(out, r)
		}
	}
	m.rmu.RLock()
	for _, s := range m.dev {
		out = append(out, s.rec)
	}
	m.rmu.RUnlock()
	return out
}

// ConfigValues returns stored values with schema defaults, secrets included.
func (m *Manager) ConfigValues(id string) (plugin.ConfigResponse, error) {
	rec, ok := m.installed(id)
	if !ok {
		return plugin.ConfigResponse{}, plugin.ErrNotFound
	}
	cfg, err := m.d.Store.ReadConfig(id)
	if err != nil {
		return plugin.ConfigResponse{}, err
	}
	cfg.Values = m.withDefaults(rec.Manifest.ConfigSchema, cfg.Values)
	if cfg.Values == nil {
		cfg.Values = map[string]json.RawMessage{}
	}
	return cfg, nil
}
