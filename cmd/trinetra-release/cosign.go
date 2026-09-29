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
// cosign vX.Y.Z [--repo InfoDiveLabs/trinetra] [--key FILE] [--testkeys]
//
// It downloads the CI-signed draft release, shows the maintainer what they
// are about to co-sign (manifest summary plus any difference between the
// manifest's declared keys and this tool's own trust anchor), refuses if
// the manifest's version does not match the tag being co-signed, requires
// the maintainer to retype the version on an actual terminal, asks for the
// maintainer key passphrase, signs, uploads the maintainer signature,
// re-verifies the full draft, and publishes it.
//
// --testkeys (trinetra_testkeys builds only, R22) switches the trust anchor
// to the deterministic test key set instead of update.ProductionKeys() and
// skips the decrypted-key-is-a-known-maintainer-key check, which would
// otherwise reject every test key.
func cmdCosign(args []string) error {
	version, repo, keyFile, testkeys, err := parseCosignArgs(args)
	if err != nil {
		return err
	}
	if keyFile == "" {
		return errors.New("cosign: --key is required")
	}

	prod := update.ProductionKeys()
	if testkeys {
		if testKeySet == nil {
			return fmt.Errorf("cosign: %w", errTestKeysUnavailable)
		}
		prod = testKeySet()
	}

	tmp, err := os.MkdirTemp("", "trinetra-cosign-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if err := ghRun(
		"release", "download", version,
		"--repo", repo,
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
	if err := update.VerifySignature(prod.CI, update.ReleasePrefix, manifestBytes, ciSig); err != nil {
		return fmt.Errorf("cosign: CI signature: %w", err)
	}
	m, err := update.DecodeManifest(manifestBytes)
	if err != nil {
		return fmt.Errorf("cosign: %w", err)
	}
	if err := checkManifestVersionMatchesTag(m.Version, version); err != nil {
		return fmt.Errorf("cosign: refusing to co-sign: %w", err)
	}

	fmt.Print(summarizeManifest(m))
	fmt.Print(keyReview(m.Keys, prod))

	if err := requireInteractiveConfirmation(version); err != nil {
		return fmt.Errorf("cosign: %w", err)
	}

	pass, err := readPassphrase("Maintainer key passphrase: ")
	if err != nil {
		return err
	}
	priv, err := readEncryptedKey(keyFile, pass)
	if err != nil {
		return fmt.Errorf("cosign: %w", err)
	}
	if err := checkMaintKeyTrusted(priv, prod, testkeys); err != nil {
		return fmt.Errorf("cosign: %w", err)
	}

	maintSigPath := filepath.Join(tmp, "manifest.maint.sig")
	if err := os.WriteFile(maintSigPath, signWithPrefix(priv, update.ReleasePrefix, manifestBytes), 0o644); err != nil {
		return err
	}

	if err := ghRun("release", "upload", version, maintSigPath, "--repo", repo); err != nil {
		return fmt.Errorf("cosign: upload: %w", err)
	}

	full := filepath.Join(tmp, "full")
	if err := os.MkdirAll(full, 0o755); err != nil {
		return err
	}
	if err := ghRun("release", "download", version, "--repo", repo, "--dir", full, "--clobber"); err != nil {
		return fmt.Errorf("cosign: post-sign download: %w", err)
	}
	if _, err := verifyDir(full, prod); err != nil {
		return fmt.Errorf("cosign: post-sign verify: %w", err)
	}

	if err := ghRun("release", "edit", version, "--repo", repo, "--draft=false"); err != nil {
		return fmt.Errorf("cosign: publish: %w", err)
	}
	fmt.Println("published", version)
	return nil
}

// parseCosignArgs parses "cosign vX.Y.Z [--repo R] [--key F] [--testkeys]",
// accepting the version positional and the flags in any order (review F1 —
// flag.FlagSet stops parsing at the first non-flag argument, which breaks
// the brief's own documented usage "cosign vX.Y.Z --key FILE").
func parseCosignArgs(args []string) (version, repo, keyFile string, testkeys bool, err error) {
	repo = "InfoDiveLabs/trinetra"
	repoSet := false
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--testkeys" || a == "-testkeys":
			testkeys = true
		case a == "--repo" || a == "-repo":
			i++
			if i >= len(args) {
				return "", "", "", false, fmt.Errorf("cosign: %s requires a value", a)
			}
			repo, repoSet = args[i], true
		case strings.HasPrefix(a, "--repo="):
			repo, repoSet = strings.TrimPrefix(a, "--repo="), true
		case strings.HasPrefix(a, "-repo="):
			repo, repoSet = strings.TrimPrefix(a, "-repo="), true
		case a == "--key" || a == "-key":
			i++
			if i >= len(args) {
				return "", "", "", false, fmt.Errorf("cosign: %s requires a value", a)
			}
			keyFile = args[i]
		case strings.HasPrefix(a, "--key="):
			keyFile = strings.TrimPrefix(a, "--key=")
		case strings.HasPrefix(a, "-key="):
			keyFile = strings.TrimPrefix(a, "-key=")
		case strings.HasPrefix(a, "-") && a != "-":
			return "", "", "", false, fmt.Errorf("cosign: unknown flag %q", a)
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) != 1 {
		return "", "", "", false, errors.New("cosign: expected exactly one version argument, e.g. v1.2.3")
	}
	if testkeys && !repoSet {
		// R24: never let a test-key co-sign default to the real repository.
		return "", "", "", false, errors.New("cosign: --testkeys requires an explicit --repo (a test repository, never the real one by default)")
	}
	return positional[0], repo, keyFile, testkeys, nil
}

// ghRun shells out to gh with an explicit argument list; it never builds a
// shell command string.
func ghRun(args ...string) error {
	cmd := exec.Command("gh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// checkManifestVersionMatchesTag refuses to co-sign a manifest whose
// version does not match the release tag it was downloaded from (review
// F3). Without this, a CI-signed manifest for version A attached to draft
// tag B would be co-signed and published under B, and hosts would install A
// believing it to be B.
func checkManifestVersionMatchesTag(manifestVersion, tag string) error {
	mv, err := update.ParseVersion(manifestVersion)
	if err != nil {
		return fmt.Errorf("manifest version %q: %w", manifestVersion, err)
	}
	tv, err := update.ParseVersion(tag)
	if err != nil {
		return fmt.Errorf("tag %q: %w", tag, err)
	}
	if mv != tv {
		return fmt.Errorf("manifest version %s does not match tag %s", manifestVersion, tag)
	}
	return nil
}

// checkMaintKeyTrusted refuses to sign and upload with a decrypted
// maintainer key whose public half is not one of prod's trusted maintainer
// keys (review M5). A wrong key file would otherwise produce a
// manifest.maint.sig that fails verification and, because upload has no
// --clobber, blocks a clean re-run. Skipped only on the --testkeys path,
// where prod is the test key set rather than the real production keys.
func checkMaintKeyTrusted(priv ed25519.PrivateKey, prod update.KeySet, testkeys bool) error {
	if testkeys {
		return nil
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return errors.New("decrypted key is not an ed25519 key")
	}
	for _, k := range prod.Maint {
		if pub.Equal(k) {
			return nil
		}
	}
	return errors.New("decrypted key's public half is not in ProductionKeys().Maint")
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

// keyReview is what cosign shows about manifest.keys (R19): one quiet line
// when the key set the new binary compiles in equals this tool's trust
// anchor, and a prominent rotation block naming each changed role otherwise
// -- the one warning meant to catch a malicious keys.go change, so it must
// not fire on every release.
func keyReview(mk update.ManifestKeys, prod update.KeySet) string {
	diffs := diffManifestKeys(mk, prod)
	if len(diffs) == 0 {
		return "keys: unchanged (the new build trusts the same keys as this tool)\n"
	}
	var b strings.Builder
	b.WriteString("\n!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n")
	b.WriteString("KEY ROTATION: this release changes the keys hosts will trust.\n")
	b.WriteString("Co-sign only if you made this change deliberately.\n")
	for _, d := range diffs {
		b.WriteString("  " + d + "\n")
	}
	b.WriteString("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n\n")
	return b.String()
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

// openConfirmTTY opens the controlling terminal for the interactive version
// retype gate. It is a variable so tests can substitute a fake
// reader/writer without a real terminal (review F2).
var openConfirmTTY = func() (io.ReadWriteCloser, error) {
	return openTTY()
}

// requireInteractiveConfirmation reads the "type the version" gate from the
// controlling terminal, so it cannot be satisfied by a pipe on stdin
// (review F2). When TRINETRA_MAINT_PASSPHRASE_FILE is set — the
// automation/e2e path, where there is no human present to retype anything —
// the terminal requirement is waived and the gate is skipped rather than
// failing closed.
func requireInteractiveConfirmation(version string) error {
	if os.Getenv("TRINETRA_MAINT_PASSPHRASE_FILE") != "" {
		return nil
	}
	tty, err := openConfirmTTY()
	if err != nil {
		return errors.New("no terminal available to confirm the version; set TRINETRA_MAINT_PASSPHRASE_FILE for automation")
	}
	defer tty.Close()
	return confirmVersion(tty, tty, version)
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
