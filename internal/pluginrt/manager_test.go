package pluginrt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type fakeHost struct {
	mu         sync.Mutex
	registered map[string][]plugin.Permission
	devFlag    map[string]bool
	seq        int
	notified   []plugin.ConfigResponse
	resolved   []string
	forgotten  []string
	validate   func(id string, v map[string]json.RawMessage) (plugin.FieldErrors, error)
}

func newFakeHost() *fakeHost {
	return &fakeHost{registered: map[string][]plugin.Permission{}, devFlag: map[string]bool{}}
}

func (h *fakeHost) Register(id string, granted []plugin.Permission, dev bool) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	h.registered[id] = granted
	h.devFlag[id] = dev
	return fmt.Sprintf("tok-%s-%d", id, h.seq), nil
}

func (h *fakeHost) Unregister(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.registered, id)
}

func (h *fakeHost) NotifyConfig(id string, cfg plugin.ConfigResponse) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notified = append(h.notified, cfg)
}

func (h *fakeHost) ValidateConfig(id string, v map[string]json.RawMessage) (plugin.FieldErrors, error) {
	if h.validate != nil {
		return h.validate(id, v)
	}
	return nil, nil
}

func (h *fakeHost) Health(id string) plugin.Health { return plugin.Health{Connected: true} }

func (h *fakeHost) ResolveAll(id, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resolved = append(h.resolved, id+":"+reason)
}

func (h *fakeHost) HostSocket() string { return "/run/trinetra-plugins/host.sock" }

func (h *fakeHost) Forget(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.forgotten = append(h.forgotten, id)
}

func (h *fakeHost) isRegistered(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.registered[id]
	return ok
}

type startCall struct {
	rec        plugin.Installed
	bin, token string
}

type fakeSup struct {
	mu      sync.Mutex
	running map[string]bool
	starts  []startCall
	stops   []string
}

func newFakeSup() *fakeSup { return &fakeSup{running: map[string]bool{}} }

func (s *fakeSup) Start(rec plugin.Installed, bin, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running[rec.ID] = true
	s.starts = append(s.starts, startCall{rec, bin, token})
	return nil
}

func (s *fakeSup) Stop(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, id)
	s.stops = append(s.stops, id)
	return nil
}

func (s *fakeSup) Running(id string) (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[id], 0
}

type env struct {
	t     *testing.T
	m     *Manager
	store *Store
	host  *fakeHost
	sup   *fakeSup
	reg   *fakeRegistry
	pkgs  map[string][]byte
	allow bool
	now   time.Time
}

func fakeCoerce(s *plugin.ConfigSchema, kv map[string]string) (map[string]json.RawMessage, plugin.FieldErrors) {
	out := map[string]json.RawMessage{}
	errs := plugin.FieldErrors{}
	for k, v := range kv {
		if s.Properties[k] == nil {
			errs[k] = "unknown setting"
			continue
		}
		b, _ := json.Marshal(v)
		out[k] = b
	}
	if len(errs) == 0 {
		errs = nil
	}
	return out, errs
}

func fakeValidate(s *plugin.ConfigSchema, v map[string]json.RawMessage) plugin.FieldErrors {
	errs := plugin.FieldErrors{}
	for _, r := range s.Required {
		if len(v[r]) == 0 {
			errs[r] = "required"
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func fakeDefaults(s *plugin.ConfigSchema, v map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if s != nil {
		for k, p := range s.Properties {
			if len(p.Default) > 0 {
				out[k] = p.Default
			}
		}
	}
	for k, x := range v {
		out[k] = x
	}
	return out
}

func fakeRedact(s *plugin.ConfigSchema, v map[string]json.RawMessage) (map[string]json.RawMessage, []string) {
	out := map[string]json.RawMessage{}
	var set []string
	for k, x := range v {
		if p := s.Properties[k]; p != nil && p.Format == plugin.FormatSecret {
			set = append(set, k)
			continue
		}
		out[k] = x
	}
	return out, set
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store, _, _ := newStore(t)
	e := &env{t: t, store: store, host: newFakeHost(), sup: newFakeSup(), reg: &fakeRegistry{latest: map[string]string{}},
		pkgs: map[string][]byte{}, allow: true, now: time.Unix(1_700_000_000, 0)}
	e.m = NewManager(Deps{
		Store: store, Registry: func() plugin.Registry { return e.reg }, Host: func() plugin.HostRegistry { return e.host },
		Supervisor: e.sup, AllowUnverified: func() bool { return e.allow }, CoreVersion: "1.0.0", GOOS: "linux", GOARCH: "amd64",
		Now: func() time.Time { return e.now },
		Fetch: func(ctx context.Context, url string) (io.ReadCloser, error) {
			b, ok := e.pkgs[url]
			if !ok {
				return nil, errors.New("404")
			}
			return io.NopCloser(bytes.NewReader(b)), nil
		},
		Validate: fakeValidate, Coerce: fakeCoerce, WithDefaults: fakeDefaults, Redact: fakeRedact,
	})
	e.reg.resolve = func(id, version string) (plugin.PackageRef, error) {
		if version == "" {
			version = e.reg.latest[id]
		}
		url := "https://reg.example/" + id + "-" + version + ".tgz"
		b, ok := e.pkgs[url]
		if !ok {
			return plugin.PackageRef{}, plugin.ErrNotFound
		}
		sum := sha256.Sum256(b)
		return plugin.PackageRef{ID: id, Version: version, URL: url, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))}, nil
	}
	return e
}

func (e *env) addRegistryPackage(id, version string, signed bool, perms ...plugin.Permission) string {
	e.t.Helper()
	m := testManifest(id, func(m *plugin.Manifest) {
		m.Version = version
		if perms != nil {
			m.Permissions = perms
		}
		m.ConfigSchema = &plugin.ConfigSchema{Type: "object", Properties: map[string]*plugin.SchemaProp{
			"url":    {Type: "string", Title: "URL"},
			"secret": {Type: "string", Title: "Secret", Format: plugin.FormatSecret},
			"mode":   {Type: "string", Title: "Mode", Default: json.RawMessage(`"fast"`)},
		}, Required: []string{"url"}}
		m.Contributes.Files = []plugin.FileDecl{{Name: "doc.txt", Title: "Doc", ContentType: "text/plain"}}
	})
	files := packageFiles(e.t, m, signed)
	files["files/doc.txt"] = []byte("hello doc")
	url := "https://reg.example/" + id + "-" + version + ".tgz"
	e.pkgs[url] = buildPackage(e.t, files)
	if cur, ok := e.reg.latest[id]; !ok || cmpVersion(version, cur) > 0 {
		e.reg.latest[id] = version
	}
	return url
}

func allPerms(m plugin.Manifest) []plugin.Permission { return m.Permissions }

func (e *env) installEnabled(id string, perms ...plugin.Permission) {
	e.t.Helper()
	e.addRegistryPackage(id, "1.0.0", true, perms...)
	res, err := e.m.Install(plugin.InstallRequest{Ref: id, Actor: "root", Via: "cli"})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.m.Enable(plugin.EnableRequest{ID: id, Actor: "root", Via: "cli", Accepted: res.Permissions}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) auditActions() []string {
	b, _ := os.ReadFile(filepath.Join(e.store.root(), plugin.AuditFileName))
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var a plugin.AuditEntry
		_ = json.Unmarshal([]byte(l), &a)
		out = append(out, a.Action)
	}
	return out
}

func writeTar(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "demo.tar.gz")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallFromFileUnverified(t *testing.T) {
	e := newEnv(t)
	m := testManifest("demo")
	path := writeTar(t, buildPackage(t, packageFiles(t, m, false)))
	res, err := e.m.Install(plugin.InstallRequest{Ref: path, Actor: "root", Via: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Trust != plugin.TrustUnverified || res.Enabled {
		t.Fatalf("res = %+v", res)
	}
	rec, _ := e.store.Load("demo")
	if rec.State != plugin.StateInstalled || rec.Source != "file:"+path || rec.PackageSHA256 == "" {
		t.Fatalf("rec = %+v", rec)
	}
	if pi, err := e.m.PackageFile("demo"); err != nil || pi.Size == 0 {
		t.Fatalf("package file: %+v %v", pi, err)
	}
	if got := e.auditActions(); len(got) != 1 || got[0] != "plugin.install" {
		t.Fatalf("audit = %v", got)
	}
}

func TestInstallFromRegistryVerified(t *testing.T) {
	e := newEnv(t)
	e.addRegistryPackage("demo", "1.0.0", true)
	res, err := e.m.Install(plugin.InstallRequest{Ref: "demo", Actor: "root", Via: "cli"})
	if err != nil || res.Trust != plugin.TrustVerified {
		t.Fatalf("%+v %v", res, err)
	}
	rec, _ := e.store.Load("demo")
	if rec.Source != "registry" {
		t.Fatalf("source = %s", rec.Source)
	}
}

func TestInstallRegistryRefusedWhenUnsigned(t *testing.T) {
	e := newEnv(t)
	e.addRegistryPackage("demo", "1.0.0", false)
	_, err := e.m.Install(plugin.InstallRequest{Ref: "demo@1.0.0"})
	if !errors.Is(err, plugin.ErrBadSignature) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.store.Load("demo"); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatal("record written")
	}
}

func TestInstallRejectsHTTP(t *testing.T) {
	e := newEnv(t)
	if _, err := e.m.Install(plugin.InstallRequest{Ref: "http://evil.example/p.tgz"}); err == nil || !strings.Contains(err.Error(), "http://") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallExpectSHA256Mismatch(t *testing.T) {
	e := newEnv(t)
	path := writeTar(t, buildPackage(t, packageFiles(t, testManifest("demo"), false)))
	_, err := e.m.Install(plugin.InstallRequest{Ref: path, ExpectSHA256: strings.Repeat("0", 64)})
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallUnverifiedRefusedWhenDisallowed(t *testing.T) {
	e := newEnv(t)
	e.allow = false
	path := writeTar(t, buildPackage(t, packageFiles(t, testManifest("demo"), false)))
	if _, err := e.m.Install(plugin.InstallRequest{Ref: path}); !errors.Is(err, plugin.ErrUnverifiedDisallowed) {
		t.Fatalf("err = %v", err)
	}
	e.allow = true
	if _, err := e.m.Install(plugin.InstallRequest{Ref: path, RequireVerified: true}); !errors.Is(err, plugin.ErrBadSignature) {
		t.Fatalf("require_verified: %v", err)
	}
}

func TestInstallWithConsentEnablesImmediately(t *testing.T) {
	e := newEnv(t)
	e.addRegistryPackage("demo", "1.0.0", true)
	res, err := e.m.Install(plugin.InstallRequest{Ref: "demo", Actor: "a", Via: "fleet:master1",
		Consent: &plugin.ConsentRecord{Actor: "admin", Via: "fleet:master1", Permissions: []plugin.Permission{plugin.PermMetricsRead}}})
	if err != nil || !res.Enabled {
		t.Fatalf("%+v %v", res, err)
	}
	rec, _ := e.store.Load("demo")
	if rec.State != plugin.StateEnabled || len(rec.Consents) != 1 || rec.Consents[0].Via != "fleet:master1" || rec.Consents[0].Actor != "admin" {
		t.Fatalf("rec = %+v", rec)
	}
	if len(e.sup.starts) != 1 {
		t.Fatalf("starts = %d", len(e.sup.starts))
	}
}

func TestEnableRegistersStartsAndAudits(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	if !e.host.isRegistered("demo") || !e.sup.running["demo"] {
		t.Fatal("not registered/running")
	}
	st := e.sup.starts[0]
	wantBin := filepath.Join(e.store.VersionDir("demo", "1.0.0"), "bin", "linux-amd64", "trinetra-plugin-demo")
	if st.bin != wantBin || st.token == "" {
		t.Fatalf("start = %+v", st)
	}
	if b, _ := os.ReadFile(filepath.Join(e.store.root(), "demo", plugin.TokenFileName)); string(b) != st.token {
		t.Fatalf("token file = %q", b)
	}
	rec, _ := e.store.Load("demo")
	if rec.State != plugin.StateEnabled || len(rec.Granted) != 1 {
		t.Fatalf("rec = %+v", rec)
	}
	got := strings.Join(e.auditActions(), ",")
	if got != "plugin.install,plugin.consent,plugin.enable" {
		t.Fatalf("audit = %s", got)
	}
	if _, tok, ok := e.m.Inventory().Endpoint("demo"); !ok || tok != st.token {
		t.Fatalf("endpoint %v %v", tok, ok)
	}
}

func TestEnableWithoutConsentFails(t *testing.T) {
	e := newEnv(t)
	e.addRegistryPackage("demo", "1.0.0", true)
	_, _ = e.m.Install(plugin.InstallRequest{Ref: "demo"})
	if err := e.m.Enable(plugin.EnableRequest{ID: "demo"}); !errors.Is(err, plugin.ErrConsentRequired) {
		t.Fatalf("err = %v", err)
	}
	if len(e.sup.starts) != 0 {
		t.Fatal("started without consent")
	}
}

func TestDisableStopsUnregistersResolvesAlerts(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	if err := e.m.Disable("demo", "root", "cli"); err != nil {
		t.Fatal(err)
	}
	if e.host.isRegistered("demo") || e.sup.running["demo"] {
		t.Fatal("still registered/running")
	}
	if len(e.host.resolved) != 1 || !strings.HasPrefix(e.host.resolved[0], "demo:") {
		t.Fatalf("resolved = %v", e.host.resolved)
	}
	rec, _ := e.store.Load("demo")
	if rec.State != plugin.StateInstalled {
		t.Fatalf("state = %s", rec.State)
	}
}

func TestRemoveDeletesFiles(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	if err := e.m.Remove("demo", "root", "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Load("demo"); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatal("record remains")
	}
	if _, err := os.Stat(e.store.VersionDir("demo", "1.0.0")); !os.IsNotExist(err) {
		t.Fatal("binaries remain")
	}
	if len(e.host.forgotten) != 1 || e.host.forgotten[0] != "demo" {
		t.Fatalf("series state not forgotten: %v", e.host.forgotten)
	}
}

func TestUpdateSamePermissionsSwapsVersion(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	e.addRegistryPackage("demo", "1.1.0", true)
	res, err := e.m.Update(plugin.UpdateRequest{Actor: "root", Via: "cli"})
	if err != nil || len(res) != 1 || res[0].To != "1.1.0" || res[0].Error != "" || len(res[0].NeedsConsent) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	rec, _ := e.store.Load("demo")
	if rec.Version != "1.1.0" || rec.State != plugin.StateEnabled || rec.Pending != nil {
		t.Fatalf("rec = %+v", rec)
	}
	if last := e.sup.starts[len(e.sup.starts)-1]; !strings.Contains(last.bin, "1.1.0") {
		t.Fatalf("restarted bin = %s", last.bin)
	}
	if _, err := os.Stat(e.store.VersionDir("demo", "1.0.0")); !os.IsNotExist(err) {
		t.Fatal("old version not pruned")
	}
}

func TestUpdateNewPermissionsStaysPendingAndOldKeepsRunning(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	e.addRegistryPackage("demo", "2.0.0", true, plugin.PermMetricsRead, plugin.PermNetOutbound)
	res, err := e.m.Update(plugin.UpdateRequest{ID: "demo"})
	if err != nil || len(res) != 1 || len(res[0].NeedsConsent) != 1 || res[0].NeedsConsent[0] != plugin.PermNetOutbound {
		t.Fatalf("%+v %v", res, err)
	}
	rec, _ := e.store.Load("demo")
	if rec.Version != "1.0.0" || rec.Pending == nil || rec.Pending.Version != "2.0.0" || rec.State != plugin.StateEnabled {
		t.Fatalf("rec = %+v", rec)
	}
	if len(e.sup.starts) != 1 || !e.sup.running["demo"] {
		t.Fatal("old version not left running")
	}
}

func TestEnablePendingAppliesUpdate(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	e.addRegistryPackage("demo", "2.0.0", true, plugin.PermMetricsRead, plugin.PermNetOutbound)
	if _, err := e.m.Update(plugin.UpdateRequest{ID: "demo"}); err != nil {
		t.Fatal(err)
	}
	req := plugin.EnableRequest{ID: "demo", Pending: true, Accepted: []plugin.Permission{plugin.PermMetricsRead}}
	if err := e.m.Enable(req); !errors.Is(err, plugin.ErrConsentRequired) {
		t.Fatalf("err = %v", err)
	}
	req.Accepted = append(req.Accepted, plugin.PermNetOutbound)
	if err := e.m.Enable(req); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.store.Load("demo")
	if rec.Version != "2.0.0" || rec.Pending != nil || len(rec.Granted) != 2 || len(rec.Consents) != 2 {
		t.Fatalf("rec = %+v", rec)
	}
	if last := e.sup.starts[len(e.sup.starts)-1]; !strings.Contains(last.bin, "2.0.0") {
		t.Fatalf("bin = %s", last.bin)
	}
}

func TestSetConfigKeepsSecretWhenBlank(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	set := func(v map[string]string, unset ...string) error {
		return e.m.SetConfig(plugin.SetConfigRequest{ID: "demo", Values: v, Unset: unset, Actor: "root", Via: "web"})
	}
	if err := set(map[string]string{"url": "http://a", "secret": "hunter2"}); err != nil {
		t.Fatal(err)
	}
	if err := set(map[string]string{"url": "http://b", "secret": ""}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := e.store.ReadConfig("demo")
	if string(cfg.Values["secret"]) != `"hunter2"` || string(cfg.Values["url"]) != `"http://b"` {
		t.Fatalf("values = %v", cfg.Values)
	}
	if err := set(nil, "secret"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = e.store.ReadConfig("demo")
	if _, ok := cfg.Values["secret"]; ok {
		t.Fatal("unset did not delete")
	}
}

func TestSetConfigValidatesAndNotifies(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	err := e.m.SetConfig(plugin.SetConfigRequest{ID: "demo", Values: map[string]string{"mode": "slow"}})
	var fe plugin.FieldErrors
	if !errors.As(err, &fe) || fe["url"] == "" {
		t.Fatalf("err = %v", err)
	}
	if len(e.host.notified) != 0 {
		t.Fatal("notified on invalid config")
	}
	if err := e.m.SetConfig(plugin.SetConfigRequest{ID: "demo", Values: map[string]string{"url": "http://a"}}); err != nil {
		t.Fatal(err)
	}
	if len(e.host.notified) != 1 || e.host.notified[0].Revision != 1 || string(e.host.notified[0].Values["mode"]) != `"fast"` {
		t.Fatalf("notified = %+v", e.host.notified)
	}
	if err := e.m.SetConfig(plugin.SetConfigRequest{ID: "demo", Values: map[string]string{"bogus": "x"}}); err == nil {
		t.Fatal("unknown key accepted")
	}
}

func TestSetConfigPluginRejection(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	e.host.validate = func(string, map[string]json.RawMessage) (plugin.FieldErrors, error) {
		return plugin.FieldErrors{"url": "unreachable"}, nil
	}
	err := e.m.SetConfig(plugin.SetConfigRequest{ID: "demo", Values: map[string]string{"url": "http://a"}})
	var fe plugin.FieldErrors
	if !errors.As(err, &fe) || fe["url"] != "unreachable" {
		t.Fatalf("err = %v", err)
	}
	if cfg, _ := e.store.ReadConfig("demo"); cfg.Revision != 0 {
		t.Fatal("config written despite rejection")
	}
	e.host.validate = func(string, map[string]json.RawMessage) (plugin.FieldErrors, error) {
		return nil, plugin.ErrNotRunning
	}
	if err := e.m.SetConfig(plugin.SetConfigRequest{ID: "demo", Values: map[string]string{"url": "http://a"}}); err != nil {
		t.Fatalf("not-running should be ignored: %v", err)
	}
}

func TestSetConfigAuditHasNoValues(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	if err := e.m.SetConfig(plugin.SetConfigRequest{ID: "demo", Values: map[string]string{"url": "http://a", "secret": "topsecret-value"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(e.store.root(), plugin.AuditFileName))
	if bytes.Contains(b, []byte("topsecret-value")) || bytes.Contains(b, []byte("http://a")) {
		t.Fatalf("audit leaks values: %s", b)
	}
	if !bytes.Contains(b, []byte("plugin.config")) || !bytes.Contains(b, []byte("secret")) {
		t.Fatalf("audit = %s", b)
	}
}

func TestConfigRedactsSecrets(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	_ = e.m.SetConfig(plugin.SetConfigRequest{ID: "demo", Values: map[string]string{"url": "http://a", "secret": "hunter2"}})
	rc, err := e.m.Config("demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rc.Values["secret"]; ok || len(rc.SecretsSet) != 1 || rc.SecretsSet[0] != "secret" || rc.Schema == nil {
		t.Fatalf("redacted = %+v", rc)
	}
	b, _ := json.Marshal(rc)
	if bytes.Contains(b, []byte("hunter2")) {
		t.Fatal("secret leaked")
	}
	full, _ := e.m.Inventory().ConfigValues("demo")
	if string(full.Values["secret"]) != `"hunter2"` {
		t.Fatalf("inventory config = %v", full.Values)
	}
}

func TestStartEnabledMintsFreshTokens(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	first := e.sup.starts[0].token
	e.m.StopAll()
	if e.host.isRegistered("demo") || e.sup.running["demo"] {
		t.Fatal("StopAll left it registered")
	}
	m2 := NewManager(e.m.d)
	m2.StartEnabled()
	if len(e.sup.starts) != 2 {
		t.Fatalf("starts = %d", len(e.sup.starts))
	}
	second := e.sup.starts[1].token
	if first == second || second == "" {
		t.Fatalf("tokens %q %q", first, second)
	}
	if b, _ := os.ReadFile(filepath.Join(e.store.root(), "demo", plugin.TokenFileName)); string(b) != second {
		t.Fatal("token file not refreshed")
	}
	if rec, _ := e.store.Load("demo"); rec.State != plugin.StateEnabled {
		t.Fatalf("state = %s", rec.State)
	}
}

func TestApplyRevocationsDisables(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	e.m.ApplyRevocations([]plugin.Revocation{{ID: "other"}, {ID: "demo", Versions: []string{"0.9.0"}, Reason: "x"}})
	if rec, _ := e.store.Load("demo"); rec.State != plugin.StateEnabled {
		t.Fatal("unrelated revocation applied")
	}
	e.m.ApplyRevocations([]plugin.Revocation{{ID: "demo", Reason: "malware"}})
	rec, _ := e.store.Load("demo")
	if rec.State != plugin.StateRevoked || rec.Reason != "malware" || e.sup.running["demo"] || e.host.isRegistered("demo") {
		t.Fatalf("rec = %+v", rec)
	}
	if err := e.m.Enable(plugin.EnableRequest{ID: "demo", Accepted: rec.Manifest.Permissions}); !errors.Is(err, plugin.ErrRevoked) {
		t.Fatalf("enable revoked: %v", err)
	}
	st, _ := e.m.Get("demo")
	if st.RevokedReason != "malware" {
		t.Fatalf("status = %+v", st)
	}
}

func TestMarkFailedOnlyAffectsEnabled(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	e.m.MarkFailed("demo", "crash loop")
	rec, _ := e.store.Load("demo")
	if rec.State != plugin.StateFailed || rec.Reason != "crash loop" || e.host.isRegistered("demo") {
		t.Fatalf("rec = %+v", rec)
	}
	_ = e.m.Disable("demo", "root", "cli")
	e.m.MarkFailed("demo", "late")
	if rec, _ := e.store.Load("demo"); rec.State != plugin.StateInstalled {
		t.Fatalf("late failure overwrote state: %s", rec.State)
	}
}

func TestListShowsUpdateAvailable(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	e.reg.latest["demo"] = "1.2.0"
	l, err := e.m.List()
	if err != nil || len(l) != 1 {
		t.Fatalf("%v %v", l, err)
	}
	if l[0].UpdateAvailable != "1.2.0" || !l[0].Running || !l[0].Health.Connected {
		t.Fatalf("status = %+v", l[0])
	}
}

func TestDevAttachRequiresAllowUnverified(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	mb, _ := json.Marshal(testManifest("devplug"))
	_ = os.WriteFile(filepath.Join(dir, "plugin.json"), mb, 0o644)
	e.allow = false
	if _, err := e.m.DevAttach(plugin.DevRequest{Dir: dir}); !errors.Is(err, plugin.ErrUnverifiedDisallowed) {
		t.Fatalf("err = %v", err)
	}
	e.allow = true
	s, err := e.m.DevAttach(plugin.DevRequest{Dir: dir, Actor: "root"})
	if err != nil || s.ID != "devplug" || s.Token == "" || s.HostSocket == "" || !strings.HasSuffix(s.PluginSocket, "trinetra-plugin-dev-devplug.sock") {
		t.Fatalf("%+v %v", s, err)
	}
	if !e.host.devFlag["devplug"] {
		t.Fatal("registered without dev flag")
	}
	if sock, tok, ok := e.m.Inventory().Endpoint("devplug"); !ok || tok != s.Token || sock != s.PluginSocket {
		t.Fatal("dev endpoint")
	}
	if rec, ok := e.m.Inventory().Get("devplug"); !ok || rec.Trust != plugin.TrustDev {
		t.Fatalf("inventory = %+v", rec)
	}
	if _, err := e.store.Load("devplug"); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatal("dev record persisted")
	}
	if err := e.m.DevDetach("devplug"); err != nil {
		t.Fatal(err)
	}
	if e.host.isRegistered("devplug") {
		t.Fatal("still registered")
	}
}

func TestFileServesDeclaredOnly(t *testing.T) {
	e := newEnv(t)
	e.installEnabled("demo")
	decl, b, err := e.m.File("demo", "doc.txt")
	if err != nil || string(b) != "hello doc" || decl.ContentType != "text/plain" {
		t.Fatalf("%+v %q %v", decl, b, err)
	}
	for _, name := range []string{"../plugin.json", "plugin.json", "other"} {
		if _, _, err := e.m.File("demo", name); !errors.Is(err, plugin.ErrNotFound) {
			t.Errorf("%q: %v", name, err)
		}
	}
}
