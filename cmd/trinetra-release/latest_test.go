package main

import (
	"os"
	"testing"
)

// runLatest feeds input (a JSON array shaped like `gh api
// repos/{owner}/{repo}/releases --paginate`) to `latest --channel CHANNEL`
// on stdin and returns what it printed on stdout plus its exit code.
func runLatest(t *testing.T, channel, input string) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	go func() {
		w.Write([]byte(input))
		w.Close()
	}()
	var code int
	out := captureStdout(t, func() {
		code = run([]string{"latest", "--channel", channel})
	})
	os.Stdin = origStdin
	r.Close()
	return out, code
}

// withManifest is the assets array literal for a release that carries all
// three required manifest assets.
const withManifest = `[{"name":"manifest.json"},{"name":"manifest.ci.sig"},{"name":"manifest.maint.sig"}]`

// noManifest is the assets array literal for a release with plain binary
// assets and no manifest at all (e.g. the old serverwatch-era v0.4.1).
const noManifest = `[{"name":"serverwatch-linux-amd64"},{"name":"checksums.txt"}]`

// TestLatestPicksHighestVersionNotNewestCreated covers review F2: a v0.6.0
// release listed AFTER a v0.5.1 release in gh's (creation-date) order must
// still win, because "latest" means highest version, not newest-created.
func TestLatestPicksHighestVersionNotNewestCreated(t *testing.T) {
	input := `[
		{"tag_name":"v0.5.1","prerelease":false,"draft":false,"assets":` + withManifest + `},
		{"tag_name":"v0.6.0","prerelease":false,"draft":false,"assets":` + withManifest + `}
	]`
	out, code := runLatest(t, "stable", input)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out != "0.6.0\n" {
		t.Fatalf("out = %q, want \"0.6.0\\n\"", out)
	}
}

// TestLatestBetaComparesPrereleaseAgainstFinal covers review F2: for the
// beta channel, a final release outranks an earlier pre-release of the same
// core version (0.6.0 > 0.6.0-beta.2 under semver precedence).
func TestLatestBetaComparesPrereleaseAgainstFinal(t *testing.T) {
	input := `[
		{"tag_name":"v0.6.0-beta.2","prerelease":true,"draft":false,"assets":` + withManifest + `},
		{"tag_name":"v0.6.0","prerelease":false,"draft":false,"assets":` + withManifest + `}
	]`
	out, code := runLatest(t, "beta", input)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out != "0.6.0\n" {
		t.Fatalf("out = %q, want \"0.6.0\\n\"", out)
	}
}

// TestLatestStableIgnoresPrereleases covers review F2: the stable channel
// must never pick a pre-release, even one with a numerically higher core
// version than the highest available final release.
func TestLatestStableIgnoresPrereleases(t *testing.T) {
	input := `[
		{"tag_name":"v0.6.0-beta.1","prerelease":true,"draft":false,"assets":` + withManifest + `},
		{"tag_name":"v0.5.9","prerelease":false,"draft":false,"assets":` + withManifest + `}
	]`
	out, code := runLatest(t, "stable", input)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out != "0.5.9\n" {
		t.Fatalf("out = %q, want \"0.5.9\\n\"", out)
	}
}

// TestLatestStableRequiresNoPreReleasePartEvenIfNotFlaggedPrerelease covers
// the brief's "whose version has no pre-release part" clause independent of
// gh's prerelease flag: a maintainer-mismarked "-rc.1" tag must still be
// excluded from stable.
func TestLatestStableRequiresNoPreReleasePartEvenIfNotFlaggedPrerelease(t *testing.T) {
	input := `[
		{"tag_name":"v0.6.0-rc.1","prerelease":false,"draft":false,"assets":` + withManifest + `},
		{"tag_name":"v0.5.9","prerelease":false,"draft":false,"assets":` + withManifest + `}
	]`
	out, code := runLatest(t, "stable", input)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out != "0.5.9\n" {
		t.Fatalf("out = %q, want \"0.5.9\\n\"", out)
	}
}

// TestLatestIgnoresDrafts covers review F2/brief: a draft release, even a
// higher version, must never be picked.
func TestLatestIgnoresDrafts(t *testing.T) {
	input := `[
		{"tag_name":"v0.7.0","prerelease":false,"draft":true,"assets":` + withManifest + `},
		{"tag_name":"v0.6.0","prerelease":false,"draft":false,"assets":` + withManifest + `}
	]`
	out, code := runLatest(t, "stable", input)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out != "0.6.0\n" {
		t.Fatalf("out = %q, want \"0.6.0\\n\"", out)
	}
}

// TestLatestIgnoresUnparsableTags covers review F2: a non-semver tag (the
// "channels" pointer-storage release) must be skipped, not crash the
// comparison.
func TestLatestIgnoresUnparsableTags(t *testing.T) {
	input := `[
		{"tag_name":"channels","prerelease":false,"draft":false,"assets":[]},
		{"tag_name":"v0.6.0","prerelease":false,"draft":false,"assets":` + withManifest + `}
	]`
	out, code := runLatest(t, "stable", input)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out != "0.6.0\n" {
		t.Fatalf("out = %q, want \"0.6.0\\n\"", out)
	}
}

// TestLatestPrintsNothingWhenNoneMatch covers the brief's "print nothing and
// exit 0 if none" requirement, both for a truly empty list and for a list
// whose only entries are excluded by the channel filter.
func TestLatestPrintsNothingWhenNoneMatch(t *testing.T) {
	cases := []struct {
		name    string
		channel string
		input   string
	}{
		{"empty list", "stable", `[]`},
		{"only prereleases for stable", "stable", `[{"tag_name":"v0.6.0-beta.1","prerelease":true,"draft":false,"assets":` + withManifest + `}]`},
		{"only drafts", "beta", `[{"tag_name":"v0.6.0","prerelease":false,"draft":true,"assets":` + withManifest + `}]`},
		{"only unparsable tags", "beta", `[{"tag_name":"channels","prerelease":false,"draft":false,"assets":[]}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, code := runLatest(t, c.channel, c.input)
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			if out != "" {
				t.Fatalf("out = %q, want empty", out)
			}
		})
	}
}

// TestLatestRejectsUnknownChannel covers basic flag validation.
func TestLatestRejectsUnknownChannel(t *testing.T) {
	if code := run([]string{"latest", "--channel", "nightly"}); code == 0 {
		t.Fatal("latest accepted an unknown --channel")
	}
}

// TestLatestSkipsReleaseWithoutManifest covers B1: a release whose assets
// don't include a signed manifest (e.g. v0.4.1, shipped as serverwatch
// binaries with no manifest.json) must never be picked, even though it is a
// final, non-draft release with a higher... er, lower version than a
// qualifying one. The weekly channels.yml run must not sign a pointer at a
// release a host cannot verify.
func TestLatestSkipsReleaseWithoutManifest(t *testing.T) {
	input := `[
		{"tag_name":"v0.4.1","prerelease":false,"draft":false,"assets":` + noManifest + `},
		{"tag_name":"v0.5.0","prerelease":false,"draft":false,"assets":` + withManifest + `}
	]`
	out, code := runLatest(t, "stable", input)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out != "0.5.0\n" {
		t.Fatalf("out = %q, want \"0.5.0\\n\"", out)
	}
}

// TestLatestPrintsNothingWhenOnlyReleaseLacksManifest covers B1: if v0.4.1
// is the only release, latest must print nothing (not fall back to it), so
// channels.yml skips signing rather than signing a pointer at an
// unverifiable release.
func TestLatestPrintsNothingWhenOnlyReleaseLacksManifest(t *testing.T) {
	input := `[{"tag_name":"v0.4.1","prerelease":false,"draft":false,"assets":` + noManifest + `}]`
	out, code := runLatest(t, "stable", input)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out != "" {
		t.Fatalf("out = %q, want empty", out)
	}
}

// TestLatestSkipsReleaseMissingOneManifestAsset covers B1: all three of
// manifest.json, manifest.ci.sig and manifest.maint.sig are required. A
// release missing only manifest.maint.sig (e.g. a CI-signed draft that was
// never co-signed and published) is skipped just like one with no manifest
// at all.
func TestLatestSkipsReleaseMissingOneManifestAsset(t *testing.T) {
	input := `[
		{"tag_name":"v0.5.0","prerelease":false,"draft":false,"assets":[{"name":"manifest.json"},{"name":"manifest.ci.sig"}]},
		{"tag_name":"v0.4.9","prerelease":false,"draft":false,"assets":` + withManifest + `}
	]`
	out, code := runLatest(t, "stable", input)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out != "0.4.9\n" {
		t.Fatalf("out = %q, want \"0.4.9\\n\"", out)
	}
}
