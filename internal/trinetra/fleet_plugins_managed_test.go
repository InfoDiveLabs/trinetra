package trinetra

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func withManagedPluginSchema(t *testing.T, f func(id string) (*plugin.ConfigSchema, bool)) {
	t.Helper()
	prev := managedPluginSchema
	managedPluginSchema = f
	t.Cleanup(func() { managedPluginSchema = prev })
}

func uptimeSchema(id string) (*plugin.ConfigSchema, bool) {
	if id != "uptime" {
		return nil, false
	}
	return &plugin.ConfigSchema{Type: "object", Properties: map[string]*plugin.SchemaProp{"interval": {Type: "integer", Title: "Interval"}, "checks": {Type: "array", Title: "Checks"}}}, true
}

func TestManagedAcceptsPluginKeys(t *testing.T) {
	withManagedPluginSchema(t, uptimeSchema)
	s, err := loadManagedFragmentStore(filepath.Join(t.TempDir(), "managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	frag, err := s.Save(core.ManagedFragment{Tag: "web", Values: map[string]string{
		"plugin.uptime.interval": "30", "plugin.uptime.checks": "http://a\nhttp://b", "thresholds.cpu_pct": "88",
	}}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	values, keyFrag, conflicts := s.Desired([]string{"web"})
	if len(conflicts) != 0 || values["plugin.uptime.interval"] != "30" || values["thresholds.cpu_pct"] != "88" || keyFrag["plugin.uptime.checks"] != frag.ID {
		t.Fatalf("desired = %v %v %v", values, keyFrag, conflicts)
	}
	if keys := orderedManagedKeys(values); keys[0] != "thresholds.cpu_pct" || keys[len(keys)-1] != "plugin.uptime.interval" {
		t.Fatalf("ordered keys = %v", keys)
	}
}

func TestManagedRejectsUnknownPluginKey(t *testing.T) {
	withManagedPluginSchema(t, uptimeSchema)
	for _, k := range []string{"plugin.uptime.nope", "plugin.ghost.interval"} {
		err := validateManagedFragmentValues(map[string]string{k: "1"})
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Fatalf("%s accepted: %v", k, err)
		}
	}
	if err := validateManagedFragmentValues(map[string]string{"plugin.Uptime.interval": "1"}); err == nil {
		t.Fatal("malformed plugin key accepted")
	}
	withManagedPluginSchema(t, nil)
	if err := validateManagedFragmentValues(map[string]string{"plugin.uptime.interval": "1"}); err == nil {
		t.Fatal("plugin key accepted with no plugin subsystem")
	}
}

func TestManagedRejectsPluginValueOfWrongType(t *testing.T) {
	withManagedPluginSchema(t, uptimeSchema)
	err := validateManagedFragmentValues(map[string]string{"plugin.uptime.interval": "soon"})
	if err == nil || !strings.Contains(err.Error(), "plugin.uptime.interval") {
		t.Fatalf("non-integer accepted: %v", err)
	}
	if err := validateManagedFragmentValues(map[string]string{"plugin.uptime.interval": "45"}); err != nil {
		t.Fatalf("valid integer rejected: %v", err)
	}
}

func TestManagedChildAppliesPluginKeysViaSetConfig(t *testing.T) {
	lc := newFleetFakeLifecycle()
	lc.installed["uptime"] = plugin.Status{Installed: plugin.Installed{ID: "uptime"}}
	cfg := config.Default()
	self := &managedApplyAPI{}
	getCfg := func() *config.Config {
		if self.applied != nil {
			return self.applied
		}
		return cfg
	}
	mc := newManagedChild(filepath.Join(t.TempDir(), "managed.json"), getCfg, self, nil)
	mc.SetPluginApplier(newManagedPluginApplier(func() plugin.Lifecycle { return lc }, "fleet:master-1"))

	frame := managedConfigFrameData{Version: 1, Values: map[string]string{
		"thresholds.cpu_pct": "77", "plugin.uptime.interval": "30", "plugin.ghost.interval": "5",
	}}
	mc.Apply(frame)

	if len(lc.configs) != 1 {
		t.Fatalf("SetConfig calls = %+v", lc.configs)
	}
	c := lc.configs[0]
	if c.ID != "uptime" || c.Values["interval"] != "30" || c.Actor != "fleet" || c.Via != "fleet:master-1" {
		t.Fatalf("SetConfig = %+v", c)
	}
	if v, _ := self.applied.Get("thresholds.cpu_pct"); v != "77" {
		t.Fatalf("core key not applied: %q", v)
	}
	rep := mc.Report()
	if rep == nil || !rep.Applied || rep.Error != "" || rep.Values["plugin.uptime.interval"] != "30" || rep.Values["thresholds.cpu_pct"] != "77" {
		t.Fatalf("report = %+v", rep)
	}
	if _, ok := rep.Values["plugin.ghost.interval"]; ok {
		t.Fatal("a plugin that is not installed here was reported as applied")
	}

	lc.current = map[string]plugin.RedactedConfig{"uptime": {Values: map[string]json.RawMessage{"interval": json.RawMessage(`30`)}}}
	mc.Apply(frame)
	if len(lc.configs) != 1 {
		t.Fatalf("matching config was re-applied: %+v", lc.configs)
	}
}

func TestManagedChildPluginSecretsReappliedOnlyWhenChanged(t *testing.T) {
	lc := newFleetFakeLifecycle()
	lc.installed["uptime"] = plugin.Status{Installed: plugin.Installed{ID: "uptime"}}
	lc.current = map[string]plugin.RedactedConfig{"uptime": {SecretsSet: []string{"token"}}}
	apply := newManagedPluginApplier(func() plugin.Lifecycle { return lc }, "fleet:m")
	if ok, err := apply("uptime", map[string]string{"token": "a"}, nil); !ok || err != nil {
		t.Fatalf("first apply: %v %v", ok, err)
	}
	if ok, err := apply("uptime", map[string]string{"token": "a"}, map[string]string{"token": "a"}); !ok || err != nil || len(lc.configs) != 1 {
		t.Fatalf("unchanged secret re-applied: %v %v %d", ok, err, len(lc.configs))
	}
	if _, err := apply("uptime", map[string]string{"token": "b"}, map[string]string{"token": "a"}); err != nil || len(lc.configs) != 2 {
		t.Fatalf("changed secret not applied: %v %d", err, len(lc.configs))
	}
}

func TestManagedNonPluginKeysUnchanged(t *testing.T) {
	cfg := config.Default()
	self := &managedApplyAPI{}
	mc := newManagedChild(filepath.Join(t.TempDir(), "managed.json"), func() *config.Config {
		if self.applied != nil {
			return self.applied
		}
		return cfg
	}, self, nil)
	mc.Apply(managedConfigFrameData{Version: 2, Values: map[string]string{"thresholds.mem_pct": "66"}})
	rep := mc.Report()
	if rep == nil || !rep.Applied || rep.Values["thresholds.mem_pct"] != "66" {
		t.Fatalf("report = %+v", rep)
	}
	for k := range rep.Values {
		if strings.HasPrefix(k, "plugin.") {
			t.Fatalf("unexpected plugin key %s", k)
		}
	}
	mc.Apply(managedConfigFrameData{Version: 3, Values: map[string]string{"plugin.uptime.interval": "30"}})
	if r := mc.Report(); r.Applied || !strings.Contains(r.Error, "not available") {
		t.Fatalf("plugin key with no applier should report an error: %+v", r)
	}
}
