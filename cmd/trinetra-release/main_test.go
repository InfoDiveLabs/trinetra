package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// seedB64 returns the base64 seed for the deterministic test signer with
// this seed byte, matching update.NewTestSigner(b).
func seedB64(b byte) string {
	s := make([]byte, 32)
	for i := range s {
		s[i] = b
	}
	return base64.StdEncoding.EncodeToString(s)
}

func TestManifestSignVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "trinetra-linux-amd64"), []byte("core"), 0o755)
	os.WriteFile(filepath.Join(dir, "trinetra-web-linux-arm64"), []byte("web"), 0o755)
	if code := run([]string{"manifest", "--dir", dir, "--version", "0.5.0", "--channel", "stable", "--min-upgrade-from", "0.4.1", "--published", "2026-10-01T10:00:00Z"}); code != 0 {
		t.Fatalf("manifest exit %d", code)
	}
	mb, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	m, err := update.DecodeManifest(mb)
	if err != nil || len(m.Files) != 2 || m.Files[0].Name != "trinetra-linux-amd64" {
		t.Fatalf("manifest %+v %v", m, err)
	}
	t.Setenv("TRINETRA_SIGNING_KEY", seedB64(1))
	if code := run([]string{"sign", "--role", "ci", "--in", filepath.Join(dir, "manifest.json"), "--out", filepath.Join(dir, "manifest.ci.sig")}); code != 0 {
		t.Fatal("sign ci")
	}
	os.WriteFile(filepath.Join(dir, "manifest.maint.sig"), update.NewTestSigner(2).SignRelease(mb), 0o644)
	cs, _ := os.ReadFile(filepath.Join(dir, "manifest.ci.sig"))
	ms, _ := os.ReadFile(filepath.Join(dir, "manifest.maint.sig"))
	keys := update.KeySet{CI: []update.PublicKey{update.NewTestSigner(1).Public()}, Maint: []update.PublicKey{update.NewTestSigner(2).Public()}, Pointer: []update.PublicKey{update.NewTestSigner(3).Public()}}
	if _, err := update.VerifyRelease(keys, mb, cs, ms); err != nil {
		t.Fatalf("round trip: %v", err)
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

func TestVerifyCommandWithTestKeys(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "trinetra-linux-amd64"), []byte("core"), 0o755)
	if code := run([]string{"manifest", "--dir", dir, "--version", "0.5.0", "--channel", "stable", "--min-upgrade-from", "0.4.1", "--published", "2026-10-01T10:00:00Z"}); code != 0 {
		t.Fatalf("manifest exit %d", code)
	}
	mb, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	os.WriteFile(filepath.Join(dir, "manifest.ci.sig"), update.NewTestSigner(1).SignRelease(mb), 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.maint.sig"), update.NewTestSigner(2).SignRelease(mb), 0o644)
	if code := run([]string{"verify", dir, "--testkeys"}); code != 0 {
		t.Fatalf("verify exit %d", code)
	}
	// Tampering with a release file must fail verification.
	os.WriteFile(filepath.Join(dir, "trinetra-linux-amd64"), []byte("tampered"), 0o755)
	if code := run([]string{"verify", dir, "--testkeys"}); code == 0 {
		t.Fatal("verify accepted a tampered file")
	}
}

func TestFingerprintsCommandRuns(t *testing.T) {
	if code := run([]string{"fingerprints"}); code != 0 {
		t.Fatalf("fingerprints exit %d", code)
	}
}
