package main

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

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
		CI:      []update.PublicKey{update.NewTestSigner(1).Public()},
		Maint:   []update.PublicKey{update.NewTestSigner(2).Public()},
		Pointer: []update.PublicKey{update.NewTestSigner(3).Public()},
	}
	matching := update.ManifestKeys{
		CI:      []string{base64.StdEncoding.EncodeToString(update.NewTestSigner(1).Public())},
		Maint:   []string{base64.StdEncoding.EncodeToString(update.NewTestSigner(2).Public())},
		Pointer: []string{base64.StdEncoding.EncodeToString(update.NewTestSigner(3).Public())},
	}
	if diffs := diffManifestKeys(matching, prod); len(diffs) != 0 {
		t.Fatalf("expected no diffs for matching keys, got %v", diffs)
	}

	mismatched := update.ManifestKeys{
		CI:      []string{base64.StdEncoding.EncodeToString(update.NewTestSigner(9).Public())},
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
