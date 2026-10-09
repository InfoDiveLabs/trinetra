package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/update"
	"github.com/InfoDiveLabs/trinetra/internal/update/updatetest"
)

// seedB64 returns the base64 seed for the deterministic test signer with
// this seed byte, matching updatetest.NewTestSigner(b).
func seedB64(b byte) string {
	s := make([]byte, 32)
	for i := range s {
		s[i] = b
	}
	return base64.StdEncoding.EncodeToString(s)
}

// writeAllReleaseFiles writes the exact 9-file release set (3 binaries x 3
// linux architectures) that cmdManifest requires.
func writeAllReleaseFiles(t *testing.T, dir string) {
	t.Helper()
	for _, stem := range releaseStems {
		for _, arch := range releaseArches {
			name := stem + "-linux-" + arch
			if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = orig
	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()
	return buf.String()
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns
// fn's result plus what was written to stderr.
func captureStderr(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	code := fn()
	os.Stderr = orig
	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()
	return code, buf.String()
}

func TestManifestSignVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeAllReleaseFiles(t, dir)
	if code := run([]string{"manifest", "--dir", dir, "--version", "0.5.0", "--channel", "stable", "--min-upgrade-from", "0.4.1", "--published", "2026-10-01T10:00:00Z"}); code != 0 {
		t.Fatalf("manifest exit %d", code)
	}
	mb, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	m, err := update.DecodeManifest(mb)
	if err != nil || len(m.Files) != 9 || m.Files[0].Name != "trinetra-ctl-linux-amd64" {
		t.Fatalf("manifest %+v %v", m, err)
	}
	t.Setenv("TRINETRA_SIGNING_KEY", seedB64(1))
	if code := run([]string{"sign", "--role", "ci", "--in", filepath.Join(dir, "manifest.json"), "--out", filepath.Join(dir, "manifest.ci.sig")}); code != 0 {
		t.Fatal("sign ci")
	}
	os.WriteFile(filepath.Join(dir, "manifest.maint.sig"), updatetest.NewTestSigner(2).SignRelease(mb), 0o644)
	cs, _ := os.ReadFile(filepath.Join(dir, "manifest.ci.sig"))
	ms, _ := os.ReadFile(filepath.Join(dir, "manifest.maint.sig"))
	keys := update.KeySet{CI: []update.PublicKey{updatetest.NewTestSigner(1).Public()}, Maint: []update.PublicKey{updatetest.NewTestSigner(2).Public()}, Pointer: []update.PublicKey{updatetest.NewTestSigner(3).Public()}}
	if _, err := update.VerifyRelease(keys, mb, cs, ms); err != nil {
		t.Fatalf("round trip: %v", err)
	}
}

// TestManifestRequiresExactly9Files covers review F4: a missing release
// binary must fail manifest generation, and no manifest.json is written.
func TestManifestRequiresExactly9Files(t *testing.T) {
	dir := t.TempDir()
	writeAllReleaseFiles(t, dir)
	if err := os.Remove(filepath.Join(dir, "trinetra-web-linux-arm")); err != nil {
		t.Fatal(err)
	}
	code, stderr := captureStderr(t, func() int {
		return run([]string{"manifest", "--dir", dir, "--version", "0.5.0", "--channel", "stable", "--min-upgrade-from", "0.4.1", "--published", "2026-10-01T10:00:00Z"})
	})
	if code == 0 {
		t.Fatal("manifest generation succeeded with a missing binary")
	}
	if !strings.Contains(stderr, "trinetra-web-linux-arm") {
		t.Fatalf("stderr = %q, want it to name the missing file", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("manifest.json written despite a missing binary")
	}
}

// TestManifestRejectsUnexpectedReleaseFile covers review F4: a trinetra*-linux-* file that
// is not one of the 9 expected names must be rejected, not silently skipped.
func TestManifestRejectsUnexpectedReleaseFile(t *testing.T) {
	dir := t.TempDir()
	writeAllReleaseFiles(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "trinetra-linux-amd64.sha256"), []byte("deadbeef"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stderr := captureStderr(t, func() int {
		return run([]string{"manifest", "--dir", dir, "--version", "0.5.0", "--channel", "stable", "--min-upgrade-from", "0.4.1", "--published", "2026-10-01T10:00:00Z"})
	})
	if code == 0 {
		t.Fatal("manifest generation succeeded with an unexpected trinetra*-linux-* file")
	}
	if !strings.Contains(stderr, "unexpected release file") {
		t.Fatalf("stderr = %q, want it to mention the unexpected file", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("manifest.json written despite an unexpected release file")
	}
}

// TestManifestIgnoresNonReleaseFiles covers review F4: files that are not named
// trinetra*-linux-* (checksums.txt, darwin binaries, ...) are simply not considered.
func TestManifestIgnoresNonReleaseFiles(t *testing.T) {
	dir := t.TempDir()
	writeAllReleaseFiles(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte("not a release file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trinetra-darwin-arm64"), []byte("mac build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"manifest", "--dir", dir, "--version", "0.5.0", "--channel", "stable", "--min-upgrade-from", "0.4.1", "--published", "2026-10-01T10:00:00Z"}); code != 0 {
		t.Fatalf("manifest exit %d", code)
	}
	mb, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	m, err := update.DecodeManifest(mb)
	if err != nil || len(m.Files) != 9 {
		t.Fatalf("manifest %+v %v", m, err)
	}
	for _, f := range m.Files {
		if f.Name == "checksums.txt" || f.Name == "trinetra-darwin-arm64" {
			t.Fatalf("non-release file %q was included in the manifest", f.Name)
		}
	}
}

func TestMaintKeyfileEncryptDecrypt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maint.key")
	pub, err := writeEncryptedKey(path, []byte("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := readEncryptedKey(path, []byte("correct horse"))
	if err != nil || !priv.Public().(update.PublicKey).Equal(pub) {
		t.Fatalf("decrypt: %v", err)
	}
	if _, err := readEncryptedKey(path, []byte("wrong")); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "correct horse") {
		t.Fatal("passphrase stored")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

// TestReadEncryptedKeyRejectsMalformedEnvelopeWithoutPanicking covers
// review M3: every envelope field is bounds-checked before use (exact
// scrypt N/r/p, exact salt/nonce lengths), so a hand-edited or corrupted
// key file can only be rejected, never panic (aead.Open panics on a nonce
// of the wrong length, and a huge N is an OOM vector).
func TestReadEncryptedKeyRejectsMalformedEnvelopeWithoutPanicking(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.key")
	if _, err := writeEncryptedKey(good, []byte("correct horse")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		mod  func(envelope) envelope
	}{
		{"wrong kdf", func(x envelope) envelope { x.KDF = "argon2"; return x }},
		{"huge n", func(x envelope) envelope { x.N = 1 << 30; return x }},
		{"tiny n", func(x envelope) envelope { x.N = 1; return x }},
		{"bad r", func(x envelope) envelope { x.R = 999; return x }},
		{"bad p", func(x envelope) envelope { x.P = 999; return x }},
		{"short salt", func(x envelope) envelope { x.Salt = base64.StdEncoding.EncodeToString([]byte{1, 2, 3}); return x }},
		{"short nonce", func(x envelope) envelope { x.Nonce = base64.StdEncoding.EncodeToString([]byte{1, 2, 3}); return x }},
		{"garbage salt", func(x envelope) envelope { x.Salt = "not valid base64!!"; return x }},
		{"garbage nonce", func(x envelope) envelope { x.Nonce = "not valid base64!!"; return x }},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked: %v", r)
				}
			}()
			mb, err := json.Marshal(c.mod(e))
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, fmt.Sprintf("bad-%d.key", i))
			if err := os.WriteFile(p, mb, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readEncryptedKey(p, []byte("correct horse")); err == nil {
				t.Fatal("malformed envelope accepted")
			}
		})
	}
}

// TestReadNewPassphraseRejectsEmpty covers review M1: an empty maintainer
// passphrase must never be accepted.
func TestReadNewPassphraseRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	passFile := filepath.Join(dir, "pass")
	if err := os.WriteFile(passFile, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRINETRA_MAINT_PASSPHRASE_FILE", passFile)
	if _, err := readNewPassphrase(); err == nil {
		t.Fatal("empty passphrase accepted")
	}
}

// TestWriteEncryptedKeyRejectsEmptyPassphrase covers review M1 at the lower-level entry
// point too, and confirms no partial key file is left behind.
func TestWriteEncryptedKeyRejectsEmptyPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maint.key")
	if _, err := writeEncryptedKey(path, nil); err == nil {
		t.Fatal("empty passphrase accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("key file written despite an empty passphrase")
	}
}

func TestKeygenCIWritesSeedFileAndRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "ci.key")
	if code := run([]string{"keygen", "--role", "ci", "--out", out}); code != 0 {
		t.Fatalf("keygen exit %d", code)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != 32 {
		t.Fatalf("seed file content: %q err=%v", b, err)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// Refuses to overwrite an existing key file.
	if code := run([]string{"keygen", "--role", "ci", "--out", out}); code == 0 {
		t.Fatal("keygen overwrote existing key file")
	}
}

func TestKeygenMaintUsesPassphraseFile(t *testing.T) {
	dir := t.TempDir()
	passFile := filepath.Join(dir, "pass")
	if err := os.WriteFile(passFile, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRINETRA_MAINT_PASSPHRASE_FILE", passFile)
	out := filepath.Join(dir, "maint.key")
	if code := run([]string{"keygen", "--role", "maint", "--out", out}); code != 0 {
		t.Fatalf("keygen maint exit %d", code)
	}
	if _, err := readEncryptedKey(out, []byte("correct horse battery staple")); err != nil {
		t.Fatalf("readEncryptedKey: %v", err)
	}
}

func TestPointerExpiresExactly14DaysAfterIssued(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "pointer.json")
	if code := run([]string{"pointer", "--channel", "stable", "--version", "0.5.0", "--issued", "2026-10-01T10:00:00Z", "--out", out}); code != 0 {
		t.Fatalf("pointer exit %d", code)
	}
	b, _ := os.ReadFile(out)
	p, err := update.DecodePointer(b)
	if err != nil {
		t.Fatalf("DecodePointer: %v", err)
	}
	issued, _ := time.Parse(time.RFC3339, p.Issued)
	expires, _ := time.Parse(time.RFC3339, p.Expires)
	if got := expires.Sub(issued); got != update.MaxPointerLifetime {
		t.Fatalf("lifetime = %v, want %v", got, update.MaxPointerLifetime)
	}
}

func TestFingerprintsCommandRuns(t *testing.T) {
	if code := run([]string{"fingerprints"}); code != 0 {
		t.Fatalf("fingerprints exit %d", code)
	}
}
