package trinetra

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type catalogFake struct {
	cat       plugin.Catalog
	gotReresh *bool
}

func (c catalogFake) Catalog(refresh bool) (plugin.Catalog, error) {
	if c.gotReresh != nil {
		*c.gotReresh = refresh
	}
	return c.cat, nil
}

func stubCatalogClient(t *testing.T, c catalogFake) (out, errb *bytes.Buffer) {
	t.Helper()
	prevW, prevO, prevE := pluginWithClient, stdout, stderr
	t.Cleanup(func() { pluginWithClient, stdout, stderr = prevW, prevO, prevE })
	pluginWithClient = func(f func(plugin.Manager) error) error {
		return f(plugin.Compose(nil, nil, c, nil))
	}
	out, errb = &bytes.Buffer{}, &bytes.Buffer{}
	stdout, stderr = out, errb
	return out, errb
}

func TestPluginCatalogTable(t *testing.T) {
	var refreshed bool
	exp := time.Now().Add(24 * time.Hour).Unix()
	out, errb := stubCatalogClient(t, catalogFake{gotReresh: &refreshed, cat: plugin.Catalog{
		Fetched: time.Now().Unix(), Expires: exp,
		Entries: []plugin.CatalogEntry{
			{ID: "uptime", Latest: "1.1.0", Installed: "1.0.0", UpdateAvailable: true, Permissions: []plugin.Permission{"metrics.write", "alerts.raise"}, Description: "Checks endpoints"},
			{ID: "evil", Latest: "0.1.0", Description: "x", Revoked: "steals tokens"},
		},
	}})
	if code := cmdPluginCatalog([]string{"--refresh"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !refreshed {
		t.Fatal("--refresh not forwarded")
	}
	got := out.String()
	for _, want := range []string{"ID", "LATEST", "INSTALLED", "PERMISSIONS", "DESCRIPTION", "uptime", "1.1.0", "1.0.0 (update)", "metrics.write,alerts.raise", "Checks endpoints", "REVOKED: steals tokens"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if errb.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", errb)
	}
}

func TestPluginCatalogStaleWarning(t *testing.T) {
	exp := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix()
	out, errb := stubCatalogClient(t, catalogFake{cat: plugin.Catalog{
		Fetched: exp - 100, Expires: exp, Stale: true,
		Entries: []plugin.CatalogEntry{{ID: "uptime", Latest: "1.0.0"}},
	}})
	if code := cmdPluginCatalog(nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "catalog is stale (expired 2026-10-01); run trinetra plugin catalog --refresh") {
		t.Fatalf("stderr = %q", errb)
	}
	if !strings.Contains(out.String(), "uptime") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestPluginCatalogNoKeys(t *testing.T) {
	out, _ := stubCatalogClient(t, catalogFake{cat: plugin.Catalog{Stale: true, Error: plugin.ErrNoRegistryKeys.Error()}})
	if code := cmdPluginCatalog(nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "registry not configured in this build") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestPluginCatalogRejectsUnknownFlag(t *testing.T) {
	_, errb := stubCatalogClient(t, catalogFake{})
	if code := cmdPluginCatalog([]string{"--nope"}); code != 2 || !strings.Contains(errb.String(), "usage") {
		t.Fatalf("code=%d stderr=%q", code, errb)
	}
}
