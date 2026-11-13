package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// cmdPluginCosign implements: plugin-cosign --dir BUILD --key FILE [--testkeys]
func cmdPluginCosign(args []string) error {
	fs := flag.NewFlagSet("plugin-cosign", flag.ContinueOnError)
	dir := fs.String("dir", "", "plugin build directory holding plugin.json and plugin.json.ci.sig")
	keyFile := fs.String("key", "", "passphrase-encrypted maintainer key")
	testkeys := fs.Bool("testkeys", false, "trust the test key set (trinetra_testkeys builds only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *keyFile == "" {
		return errors.New("plugin-cosign: --dir and --key are required")
	}
	prod := update.ProductionKeys()
	if *testkeys {
		if testKeySet == nil {
			return fmt.Errorf("plugin-cosign: %w", errTestKeysUnavailable)
		}
		prod = testKeySet()
	}
	confirm := func(want string) error {
		if os.Getenv("TRINETRA_MAINT_PASSPHRASE_FILE") != "" {
			return nil
		}
		tty, err := openConfirmTTY()
		if err != nil {
			return errors.New("no terminal available to confirm; set TRINETRA_MAINT_PASSPHRASE_FILE for automation")
		}
		defer tty.Close()
		return confirmVersion(tty, tty, want)
	}
	loadKey := func() (ed25519.PrivateKey, error) {
		pass, err := readPassphrase("Maintainer key passphrase: ")
		if err != nil {
			return nil, err
		}
		return readEncryptedKey(*keyFile, pass)
	}
	return pluginCosign(*dir, prod, os.Stdout, confirm, loadKey)
}

// pluginCosign checks the CI signature on dir/plugin.json, shows what is being signed, asks
// for id@version to be retyped, and writes the maintainer signature with a trusted maintainer key.
func pluginCosign(dir string, keys update.KeySet, out io.Writer, confirm func(want string) error, loadKey func() (ed25519.PrivateKey, error)) error {
	mb, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return err
	}
	ciSig, err := os.ReadFile(filepath.Join(dir, plugin.ManifestCISigName))
	if err != nil {
		return fmt.Errorf("plugin-cosign: %w", err)
	}
	if err := update.VerifySignature(keys.CI, plugin.ManifestSigPrefix, mb, ciSig); err != nil {
		return fmt.Errorf("plugin-cosign: CI signature: %w", err)
	}
	m, err := plugin.DecodeManifest(mb)
	if err != nil {
		return fmt.Errorf("plugin-cosign: %w", err)
	}
	fmt.Fprintf(out, "plugin:      %s %s\npublisher:   %s\nhost_api:    %s\npermissions:\n", m.ID, m.Version, m.Publisher, m.HostAPI)
	if len(m.Permissions) == 0 {
		fmt.Fprintln(out, "  (none)")
	}
	for _, p := range m.Permissions {
		fmt.Fprintf(out, "  %s: %s\n", p, plugin.Describe(p))
	}
	fmt.Fprintln(out, "binaries:")
	for _, b := range m.Binaries {
		fmt.Fprintf(out, "  %s/%s  %s  %d bytes\n", b.OS, b.Arch, b.SHA256, b.Size)
	}
	if err := confirm(m.ID + "@" + m.Version); err != nil {
		return fmt.Errorf("plugin-cosign: %w", err)
	}
	priv, err := loadKey()
	if err != nil {
		return fmt.Errorf("plugin-cosign: %w", err)
	}
	pub, _ := priv.Public().(ed25519.PublicKey)
	trusted := false
	for _, k := range keys.Maint {
		trusted = trusted || pub.Equal(k)
	}
	if !trusted {
		return errors.New("plugin-cosign: the key is not a trusted maintainer key in this build")
	}
	sig := ed25519.Sign(priv, append([]byte(plugin.ManifestSigPrefix), mb...))
	path := filepath.Join(dir, plugin.ManifestMaintSigName)
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(out, "wrote", path)
	return nil
}
