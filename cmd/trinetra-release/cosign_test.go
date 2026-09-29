package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/update"
	"github.com/InfoDiveLabs/trinetra/internal/update/updatetest"
)

// testPrivFromSeed builds the same ed25519.PrivateKey updatetest.NewTestSigner
// would (a 32-byte seed filled with b), without needing access to
// TestSigner's unexported private key field.
func testPrivFromSeed(b byte) ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = b
	}
	return ed25519.NewKeyFromSeed(seed)
}

func TestConfirmVersionRequiresExactMatch(t *testing.T) {
	var out bytes.Buffer
	if err := confirmVersion(strings.NewReader("v1.2.3\n"), &out, "v1.2.3"); err != nil {
		t.Fatalf("exact match: %v", err)
	}
	if err := confirmVersion(strings.NewReader("v1.2.4\n"), &out, "v1.2.3"); err == nil {
		t.Fatal("mismatched version was accepted")
	}
	if err := confirmVersion(strings.NewReader(""), &out, "v1.2.3"); err == nil {
		t.Fatal("empty input was accepted")
	}
}

func TestDiffManifestKeysReportsMismatch(t *testing.T) {
	prod := update.KeySet{
		CI:      []update.PublicKey{updatetest.NewTestSigner(1).Public()},
		Maint:   []update.PublicKey{updatetest.NewTestSigner(2).Public()},
		Pointer: []update.PublicKey{updatetest.NewTestSigner(3).Public()},
	}
	matching := update.ManifestKeys{
		CI:      []string{base64.StdEncoding.EncodeToString(updatetest.NewTestSigner(1).Public())},
		Maint:   []string{base64.StdEncoding.EncodeToString(updatetest.NewTestSigner(2).Public())},
		Pointer: []string{base64.StdEncoding.EncodeToString(updatetest.NewTestSigner(3).Public())},
	}
	if diffs := diffManifestKeys(matching, prod); len(diffs) != 0 {
		t.Fatalf("expected no diffs for matching keys, got %v", diffs)
	}

	mismatched := update.ManifestKeys{
		CI:      []string{base64.StdEncoding.EncodeToString(updatetest.NewTestSigner(9).Public())},
		Maint:   matching.Maint,
		Pointer: matching.Pointer,
	}
	diffs := diffManifestKeys(mismatched, prod)
	if len(diffs) != 1 || !strings.HasPrefix(diffs[0], "ci:") {
		t.Fatalf("expected a single ci diff, got %v", diffs)
	}
}

func TestSummarizeManifestIncludesFiles(t *testing.T) {
	m := update.Manifest{
		Version:        "0.5.0",
		Channel:        "stable",
		MinUpgradeFrom: "0.4.1",
		Published:      "2026-10-01T10:00:00Z",
		Files: []update.File{
			{Name: "trinetra-linux-amd64", Size: 4, SHA256: "abcd"},
		},
	}
	s := summarizeManifest(m)
	for _, want := range []string{"0.5.0", "stable", "0.4.1", "2026-10-01T10:00:00Z", "trinetra-linux-amd64", "4", "abcd"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary missing %q: %s", want, s)
		}
	}
}

// TestParseCosignArgs covers review F1: "cosign vX.Y.Z --key FILE" (the
// brief's own documented order) and "cosign --key FILE vX.Y.Z" (what
// flag.FlagSet forced before this fix) must both parse identically.
func TestParseCosignArgs(t *testing.T) {
	cases := []struct {
		name         string
		args         []string
		wantVersion  string
		wantRepo     string
		wantKey      string
		wantTestkeys bool
	}{
		{
			name: "version then key", args: []string{"v1.2.3", "--key", "k.pem"},
			wantVersion: "v1.2.3", wantRepo: "InfoDiveLabs/trinetra", wantKey: "k.pem",
		},
		{
			name: "key then version", args: []string{"--key", "k.pem", "v1.2.3"},
			wantVersion: "v1.2.3", wantRepo: "InfoDiveLabs/trinetra", wantKey: "k.pem",
		},
		{
			name:        "repo and testkeys interleaved with version",
			args:        []string{"--testkeys", "v1.2.3", "--repo", "org/repo", "--key", "k.pem"},
			wantVersion: "v1.2.3", wantRepo: "org/repo", wantKey: "k.pem", wantTestkeys: true,
		},
		{
			name:        "equals-form flags after the version",
			args:        []string{"v1.2.3", "--key=k.pem", "--repo=org/repo"},
			wantVersion: "v1.2.3", wantRepo: "org/repo", wantKey: "k.pem",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			version, repo, key, testkeys, err := parseCosignArgs(c.args)
			if err != nil {
				t.Fatalf("parseCosignArgs(%v): %v", c.args, err)
			}
			if version != c.wantVersion || repo != c.wantRepo || key != c.wantKey || testkeys != c.wantTestkeys {
				t.Fatalf("parseCosignArgs(%v) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
					c.args, version, repo, key, testkeys, c.wantVersion, c.wantRepo, c.wantKey, c.wantTestkeys)
			}
		})
	}
}

func TestParseCosignArgsRequiresExactlyOneVersion(t *testing.T) {
	if _, _, _, _, err := parseCosignArgs([]string{"--key", "k.pem"}); err == nil {
		t.Fatal("no version argument accepted")
	}
	if _, _, _, _, err := parseCosignArgs([]string{"v1.2.3", "v1.2.4", "--key", "k.pem"}); err == nil {
		t.Fatal("two version arguments accepted")
	}
}

// TestCosignAcceptsVersionBeforeOrAfterKeyFlag is the run()-level regression
// test for review F1: it exercises the brief's exact documented usage
// ("cosign vX.Y.Z --key FILE") end to end through run(), with an empty
// --key value so it fails fast on the "--key is required" check before any
// gh/network call, instead of misparsing the version as a flag value or
// erroring out on "expected exactly one version argument".
func TestCosignAcceptsVersionBeforeOrAfterKeyFlag(t *testing.T) {
	for _, args := range [][]string{
		{"cosign", "v1.2.3", "--key", ""},
		{"cosign", "--key", "", "v1.2.3"},
	} {
		args := args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stderr := captureStderr(t, func() int { return run(args) })
			if code == 0 {
				t.Fatalf("%v: expected failure (no key file), got exit 0", args)
			}
			if !strings.Contains(stderr, "--key is required") {
				t.Fatalf("%v: stderr = %q, want it to mention --key is required", args, stderr)
			}
			if strings.Contains(stderr, "expected exactly one version argument") {
				t.Fatalf("%v: stderr = %q, positional argument was misparsed", args, stderr)
			}
		})
	}
}

// TestCheckManifestVersionMatchesTag covers review F3.
func TestCheckManifestVersionMatchesTag(t *testing.T) {
	if err := checkManifestVersionMatchesTag("0.5.0", "v0.5.0"); err != nil {
		t.Fatalf("matching version rejected: %v", err)
	}
	if err := checkManifestVersionMatchesTag("0.5.0", "0.5.0"); err != nil {
		t.Fatalf("matching version (no v prefix on tag) rejected: %v", err)
	}
	if err := checkManifestVersionMatchesTag("0.5.0-beta.1", "v0.5.0-beta.1"); err != nil {
		t.Fatalf("matching pre-release rejected: %v", err)
	}
	if err := checkManifestVersionMatchesTag("0.5.0", "v0.5.1"); err == nil {
		t.Fatal("mismatched version accepted")
	}
	if err := checkManifestVersionMatchesTag("0.5.0", "v0.6.0"); err == nil {
		t.Fatal("mismatched minor version accepted")
	}
	if err := checkManifestVersionMatchesTag("0.5.0", "not-a-version"); err == nil {
		t.Fatal("unparseable tag accepted")
	}
}

// TestCheckMaintKeyTrusted covers review M5.
func TestCheckMaintKeyTrusted(t *testing.T) {
	prod := update.KeySet{Maint: []update.PublicKey{updatetest.NewTestSigner(2).Public()}}
	trusted := testPrivFromSeed(2)
	untrusted := testPrivFromSeed(9)

	if err := checkMaintKeyTrusted(trusted, prod, false); err != nil {
		t.Fatalf("trusted key rejected: %v", err)
	}
	if err := checkMaintKeyTrusted(untrusted, prod, false); err == nil {
		t.Fatal("untrusted key accepted")
	}
	if err := checkMaintKeyTrusted(untrusted, prod, true); err != nil {
		t.Fatalf("testkeys path should skip the trust check: %v", err)
	}
}

// fakeTTY is an injectable stand-in for the controlling terminal in tests
// (review F2).
type fakeTTY struct {
	r io.Reader
	w io.Writer
}

func (f fakeTTY) Read(p []byte) (int, error)  { return f.r.Read(p) }
func (f fakeTTY) Write(p []byte) (int, error) { return f.w.Write(p) }
func (f fakeTTY) Close() error                { return nil }

// TestRequireInteractiveConfirmation covers review F2: the version retype
// gate must come from a real terminal (injectable in tests via
// openConfirmTTY), must refuse to run without one, and must be skipped only
// when TRINETRA_MAINT_PASSPHRASE_FILE marks this as the automation/e2e path.
func TestRequireInteractiveConfirmation(t *testing.T) {
	origTTY := openConfirmTTY
	t.Cleanup(func() { openConfirmTTY = origTTY })

	t.Run("skipped when passphrase file is set", func(t *testing.T) {
		t.Setenv("TRINETRA_MAINT_PASSPHRASE_FILE", "/anything")
		openConfirmTTY = func() (io.ReadWriteCloser, error) {
			t.Fatal("must not open a terminal when the passphrase file is set")
			return nil, nil
		}
		if err := requireInteractiveConfirmation("v1.2.3"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("refuses without a terminal", func(t *testing.T) {
		openConfirmTTY = func() (io.ReadWriteCloser, error) {
			return nil, errors.New("no tty")
		}
		if err := requireInteractiveConfirmation("v1.2.3"); err == nil {
			t.Fatal("expected an error with no terminal available")
		}
	})

	t.Run("reads the retype from the injected terminal", func(t *testing.T) {
		openConfirmTTY = func() (io.ReadWriteCloser, error) {
			return fakeTTY{r: strings.NewReader("v1.2.3\n"), w: &bytes.Buffer{}}, nil
		}
		if err := requireInteractiveConfirmation("v1.2.3"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects a mismatched retype from the terminal", func(t *testing.T) {
		openConfirmTTY = func() (io.ReadWriteCloser, error) {
			return fakeTTY{r: strings.NewReader("v1.2.4\n"), w: &bytes.Buffer{}}, nil
		}
		if err := requireInteractiveConfirmation("v1.2.3"); err == nil {
			t.Fatal("mismatched retype accepted")
		}
	})
}
