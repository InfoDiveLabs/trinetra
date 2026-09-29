package main

import (
	"os"
	"testing"
)

// runLatest feeds input (a JSON array shaped like `gh release list --json
// tagName,isPrerelease,isDraft`) to `latest --channel CHANNEL` on stdin and
// returns what it printed on stdout plus its exit code.
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

// TestLatestPicksHighestVersionNotNewestCreated covers review F2: a v0.6.0
// release listed AFTER a v0.5.1 release in gh's (creation-date) order must
// still win, because "latest" means highest version, not newest-created.
func TestLatestPicksHighestVersionNotNewestCreated(t *testing.T) {
	input := `[
		{"tagName":"v0.5.1","isPrerelease":false,"isDraft":false},
		{"tagName":"v0.6.0","isPrerelease":false,"isDraft":false}
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
		{"tagName":"v0.6.0-beta.2","isPrerelease":true,"isDraft":false},
		{"tagName":"v0.6.0","isPrerelease":false,"isDraft":false}
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
		{"tagName":"v0.6.0-beta.1","isPrerelease":true,"isDraft":false},
		{"tagName":"v0.5.9","isPrerelease":false,"isDraft":false}
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
// gh's isPrerelease flag: a maintainer-mismarked "-rc.1" tag must still be
// excluded from stable.
func TestLatestStableRequiresNoPreReleasePartEvenIfNotFlaggedPrerelease(t *testing.T) {
	input := `[
		{"tagName":"v0.6.0-rc.1","isPrerelease":false,"isDraft":false},
		{"tagName":"v0.5.9","isPrerelease":false,"isDraft":false}
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
		{"tagName":"v0.7.0","isPrerelease":false,"isDraft":true},
		{"tagName":"v0.6.0","isPrerelease":false,"isDraft":false}
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
		{"tagName":"channels","isPrerelease":false,"isDraft":false},
		{"tagName":"v0.6.0","isPrerelease":false,"isDraft":false}
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
		{"only prereleases for stable", "stable", `[{"tagName":"v0.6.0-beta.1","isPrerelease":true,"isDraft":false}]`},
		{"only drafts", "beta", `[{"tagName":"v0.6.0","isPrerelease":false,"isDraft":true}]`},
		{"only unparsable tags", "beta", `[{"tagName":"channels","isPrerelease":false,"isDraft":false}]`},
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
