package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/registry"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

func pluginKey(b byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, ed25519.SeedSize))
}

func pluginSig(priv ed25519.PrivateKey, msg []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(plugin.ManifestSigPrefix), msg...))) + "\n")
}

const cosignManifest = `{"schema":1,"id":"uptime","name":"Uptime","version":"0.1.0","publisher":"InfoDiveLabs","description":"d","host_api":"^1.0","permissions":["net.outbound"],"binaries":[{"os":"linux","arch":"amd64","sha256":"` +
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `","size":10}]}`

func pluginBuildDir(t *testing.T, ciPriv ed25519.PrivateKey) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(cosignManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugin.ManifestCISigName), pluginSig(ciPriv, []byte(cosignManifest)), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func pluginKeys() update.KeySet {
	return update.KeySet{
		CI:    []ed25519.PublicKey{pluginKey(1).Public().(ed25519.PublicKey)},
		Maint: []ed25519.PublicKey{pluginKey(2).Public().(ed25519.PublicKey)},
	}
}

func TestPluginCosignWritesAMaintainerSignature(t *testing.T) {
	dir := pluginBuildDir(t, pluginKey(1))
	var out bytes.Buffer
	confirmed := ""
	err := pluginCosign(dir, pluginKeys(), &out, func(want string) error { confirmed = want; return nil }, func() (ed25519.PrivateKey, error) { return pluginKey(2), nil })
	if err != nil {
		t.Fatal(err)
	}
	if confirmed != "uptime@0.1.0" {
		t.Errorf("confirmation asked for %q", confirmed)
	}
	for _, want := range []string{"uptime", "0.1.0", "InfoDiveLabs", "net.outbound", "linux/amd64"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("review lacks %q:\n%s", want, out.String())
		}
	}
	ci, _ := os.ReadFile(filepath.Join(dir, plugin.ManifestCISigName))
	maint, err := os.ReadFile(filepath.Join(dir, plugin.ManifestMaintSigName))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.VerifyManifest(pluginKeys(), []byte(cosignManifest), plugin.ManifestSigs{CI: ci, Maint: maint}); err != nil {
		t.Fatalf("result does not verify: %v", err)
	}
}

func TestPluginCosignRefuses(t *testing.T) {
	noop := func(string) error { return nil }
	maint := func() (ed25519.PrivateKey, error) { return pluginKey(2), nil }
	t.Run("bad CI signature", func(t *testing.T) {
		dir := pluginBuildDir(t, pluginKey(9))
		if err := pluginCosign(dir, pluginKeys(), &bytes.Buffer{}, noop, maint); err == nil {
			t.Fatal("co-signed a manifest without a valid CI signature")
		}
		if _, err := os.Stat(filepath.Join(dir, plugin.ManifestMaintSigName)); err == nil {
			t.Fatal("wrote a signature anyway")
		}
	})
	t.Run("confirmation mismatch", func(t *testing.T) {
		dir := pluginBuildDir(t, pluginKey(1))
		if err := pluginCosign(dir, pluginKeys(), &bytes.Buffer{}, func(string) error { return errors.New("no") }, maint); err == nil {
			t.Fatal("signed without confirmation")
		}
	})
	t.Run("untrusted maintainer key", func(t *testing.T) {
		dir := pluginBuildDir(t, pluginKey(1))
		if err := pluginCosign(dir, pluginKeys(), &bytes.Buffer{}, noop, func() (ed25519.PrivateKey, error) { return pluginKey(3), nil }); err == nil {
			t.Fatal("signed with a key that is not a trusted maintainer key")
		}
	})
}
