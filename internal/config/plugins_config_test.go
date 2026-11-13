package config

import "testing"

func TestPluginsAllowUnverifiedDefaultsTrue(t *testing.T) {
	var c Config
	if !c.PluginsAllowUnverified() {
		t.Fatal("default must be true")
	}
	f := false
	c.Plugins.AllowUnverified = &f
	if c.PluginsAllowUnverified() {
		t.Fatal("explicit false ignored")
	}
}

func TestPluginsRegistryURLDefault(t *testing.T) {
	var c Config
	if c.PluginsRegistryURL() != DefaultPluginRegistryURL {
		t.Fatalf("got %q", c.PluginsRegistryURL())
	}
	c.Plugins.RegistryURL = "https://example.com/idx/"
	if c.PluginsRegistryURL() != "https://example.com/idx/" {
		t.Fatal("override ignored")
	}
}

func TestPluginsConfigKeysGetSet(t *testing.T) {
	var c Config
	if v, ok := c.Get("plugins.allow_unverified"); !ok || v != "true" {
		t.Fatalf("allow_unverified = %q %v", v, ok)
	}
	if err := c.Set("plugins.allow_unverified", "false"); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Get("plugins.allow_unverified"); v != "false" {
		t.Fatalf("after set: %q", v)
	}
	if err := c.Set("plugins.allow_unverified", "maybe"); err == nil {
		t.Fatal("bad bool accepted")
	}
	if v, ok := c.Get("plugins.registry_url"); !ok || v != DefaultPluginRegistryURL {
		t.Fatalf("registry_url = %q %v", v, ok)
	}
	if err := c.Set("plugins.registry_url", "http://insecure.example/"); err == nil {
		t.Fatal("http:// registry url accepted")
	}
	if err := c.Set("plugins.registry_url", "https://example.com/idx/"); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Get("plugins.registry_url"); v != "https://example.com/idx/" {
		t.Fatalf("after set: %q", v)
	}
	names := map[string]bool{}
	for _, k := range Keys() {
		names[k.Name] = true
	}
	if !names["plugins.allow_unverified"] || !names["plugins.registry_url"] {
		t.Fatal("keys not listed in Keys()")
	}
}
