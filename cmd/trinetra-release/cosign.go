// cmd/trinetra-release/cosign.go
package main

import (
	"bufio"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// cmdCosign implements:
// cosign vX.Y.Z [--repo InfoDiveLabs/trinetra] [--key FILE]
//
// It downloads the CI-signed draft release, shows the maintainer what they
// are about to co-sign (manifest summary plus any difference between the
// manifest's declared keys and this tool's own ProductionKeys()), requires
// the maintainer to retype the version, asks for the maintainer key
// passphrase, signs, uploads the maintainer signature, re-verifies the full
// draft, and publishes it.
func cmdCosign(args []string) error {
	fs := newFlagSet("cosign")
	repo := fs.String("repo", "InfoDiveLabs/trinetra", "GitHub repo (owner/name)")
	keyFile := fs.String("key", "", "maintainer key file (passphrase-encrypted)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errors.New("cosign: expected exactly one version argument, e.g. v1.2.3")
	}
	version := rest[0]
	if *keyFile == "" {
		return errors.New("cosign: --key is required")
	}

	tmp, err := os.MkdirTemp("", "trinetra-cosign-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if err := ghRun(
		"release", "download", version,
		"--repo", *repo,
		"--pattern", "manifest.json",
		"--pattern", "manifest.ci.sig",
		"--dir", tmp,
	); err != nil {
		return fmt.Errorf("cosign: download: %w", err)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(tmp, "manifest.json"))
	if err != nil {
		return err
	}
	ciSig, err := os.ReadFile(filepath.Join(tmp, "manifest.ci.sig"))
	if err != nil {
		return err
	}
	prod := update.ProductionKeys()
	if err := verifySignatureAny(prod.CI, update.ReleasePrefix, manifestBytes, ciSig); err != nil {
		return fmt.Errorf("cosign: CI signature: %w", err)
	}
	m, err := update.DecodeManifest(manifestBytes)
	if err != nil {
		return fmt.Errorf("cosign: %w", err)
	}

	fmt.Print(summarizeManifest(m))
	if diffs := diffManifestKeys(m.Keys, prod); len(diffs) > 0 {
		fmt.Println("key differences from this tool's ProductionKeys():")
		for _, d := range diffs {
			fmt.Println("  " + d)
		}
	}

	if err := confirmVersion(os.Stdin, os.Stdout, version); err != nil {
		return fmt.Errorf("cosign: %w", err)
	}

	pass, err := readPassphrase("Maintainer key passphrase: ")
	if err != nil {
		return err
	}
	priv, err := readEncryptedKey(*keyFile, pass)
	if err != nil {
		return fmt.Errorf("cosign: %w", err)
	}

	maintSigPath := filepath.Join(tmp, "manifest.maint.sig")
	if err := os.WriteFile(maintSigPath, signWithPrefix(priv, update.ReleasePrefix, manifestBytes), 0o644); err != nil {
		return err
	}

	if err := ghRun("release", "upload", version, maintSigPath, "--repo", *repo); err != nil {
		return fmt.Errorf("cosign: upload: %w", err)
	}

	full := filepath.Join(tmp, "full")
	if err := os.MkdirAll(full, 0o755); err != nil {
		return err
	}
	if err := ghRun("release", "download", version, "--repo", *repo, "--dir", full, "--clobber"); err != nil {
		return fmt.Errorf("cosign: post-sign download: %w", err)
	}
	if _, err := verifyDir(full, prod); err != nil {
		return fmt.Errorf("cosign: post-sign verify: %w", err)
	}

	if err := ghRun("release", "edit", version, "--repo", *repo, "--draft=false"); err != nil {
		return fmt.Errorf("cosign: publish: %w", err)
	}
	fmt.Println("published", version)
	return nil
}

// ghRun shells out to gh with an explicit argument list; it never builds a
// shell command string.
func ghRun(args ...string) error {
	cmd := exec.Command("gh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// verifySignatureAny checks a single detached signature file against a set
// of trusted keys. It mirrors internal/update's unexported verifyAny, which
// this tool cannot call directly because VerifyRelease requires both the CI
// and maintainer signatures at once; at co-sign time only the CI signature
// exists yet.
func verifySignatureAny(keys []update.PublicKey, prefix string, msg, sigFile []byte) error {
	s := strings.TrimSpace(string(sigFile))
	if s == "" {
		return errors.New("signature missing")
	}
	sig, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("signature does not verify")
	}
	full := append([]byte(prefix), msg...)
	for _, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, full, sig) {
			return nil
		}
	}
	return errors.New("signature does not verify against any trusted key")
}

// diffManifestKeys reports any role whose manifest-declared keys (base64)
// differ from prod's compiled-in keys, so a maintainer never co-signs a
// manifest quietly pointing at different trust than their own tool.
func diffManifestKeys(mk update.ManifestKeys, prod update.KeySet) []string {
	var out []string
	check := func(role string, manifestKeys []string, prodKeys []update.PublicKey) {
		prodB64 := make([]string, len(prodKeys))
		for i, k := range prodKeys {
			prodB64[i] = base64.StdEncoding.EncodeToString(k)
		}
		sort.Strings(prodB64)
		got := append([]string(nil), manifestKeys...)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(prodB64, ",") {
			out = append(out, fmt.Sprintf("%s: manifest=%v tool=%v", role, manifestKeys, prodB64))
		}
	}
	check("ci", mk.CI, prod.CI)
	check("maint", mk.Maint, prod.Maint)
	check("pointer", mk.Pointer, prod.Pointer)
	return out
}

// summarizeManifest formats the fields a maintainer must review before
// co-signing.
func summarizeManifest(m update.Manifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "version: %s\n", m.Version)
	fmt.Fprintf(&b, "channel: %s\n", m.Channel)
	fmt.Fprintf(&b, "min_upgrade_from: %s\n", m.MinUpgradeFrom)
	fmt.Fprintf(&b, "published: %s\n", m.Published)
	for _, f := range m.Files {
		fmt.Fprintf(&b, "  %s %d %s\n", f.Name, f.Size, f.SHA256)
	}
	return b.String()
}

// confirmVersion requires the operator to retype version exactly before a
// co-sign proceeds.
func confirmVersion(r io.Reader, w io.Writer, version string) error {
	fmt.Fprint(w, "Type the version to co-sign: ")
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	if strings.TrimSpace(line) != version {
		return errors.New("version confirmation did not match; aborting")
	}
	return nil
}
