package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// ghAsset is the subset of a release asset object that cmdLatest needs.
type ghAsset struct {
	Name string `json:"name"`
}

// ghRelease is the subset of a release object, as returned by `gh api
// repos/{owner}/{repo}/releases --paginate`, that cmdLatest needs.
//
// This reads the REST releases endpoint rather than `gh release list --json
// tagName,isPrerelease,isDraft`, because that list projection has no assets
// field at all: it cannot tell a release with a signed manifest apart from
// one without (review B1). The REST fields are snake_case
// (tag_name/prerelease/draft), unlike gh's list projection.
type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Prerelease bool      `json:"prerelease"`
	Draft      bool      `json:"draft"`
	Assets     []ghAsset `json:"assets"`
}

// requiredManifestAssets are the asset names a release must carry, all
// three, before cmdLatest will consider it. Without every one of them, no
// host can verify a signed manifest for that version
// (internal/trinetra/systemd.go installSignature and
// internal/update/fetch.go FetchRelease both require all three), so signing
// a channel pointer at such a release (review B1) would point every host at
// a version it can never actually apply.
var requiredManifestAssets = []string{"manifest.json", "manifest.ci.sig", "manifest.maint.sig"}

// hasSignedManifest reports whether assets includes every name in
// requiredManifestAssets.
func hasSignedManifest(assets []ghAsset) bool {
	have := make(map[string]bool, len(assets))
	for _, a := range assets {
		have[a.Name] = true
	}
	for _, want := range requiredManifestAssets {
		if !have[want] {
			return false
		}
	}
	return true
}

// cmdLatest implements: latest --channel stable|beta
//
// It reads a JSON array shaped like `gh api repos/{owner}/{repo}/releases
// --paginate` from stdin and prints the HIGHEST version among the matching
// releases, using update.ParseVersion/CompareVersions ordering rather than
// gh's list order (which is by creation date, not version — review F2/I2: a
// backport or an out-of-order publish must not move a channel pointer
// backwards or skip a higher version).
//
// A tag that isn't a valid "vX.Y.Z" or "vX.Y.Z-pre" (e.g. the "channels"
// release used to store signed pointers) is skipped rather than treated as
// an error. A draft is always skipped. A release whose assets do not
// include all of manifest.json, manifest.ci.sig and manifest.maint.sig is
// also skipped (review B1): no host could verify or install it, so it must
// never become a signed channel pointer.
//
//   - --channel stable additionally requires !Prerelease AND that the
//     parsed version has no pre-release part, so a tag a maintainer forgot
//     to mark as a GitHub prerelease still cannot reach stable.
//   - --channel beta considers every remaining tag, final or pre-release,
//     and picks the highest under full semver precedence (a final release
//     outranks an equal or lower pre-release of the same core version).
//
// Prints nothing and exits 0 if no tag matches; the caller (channels.yml)
// skips signing a pointer for that channel in that case.
func cmdLatest(args []string) error {
	fs := newFlagSet("latest")
	channel := fs.String("channel", "", "stable or beta")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *channel != "stable" && *channel != "beta" {
		return fmt.Errorf("latest: --channel must be stable or beta, got %q", *channel)
	}

	var releases []ghRelease
	if err := json.NewDecoder(os.Stdin).Decode(&releases); err != nil {
		return fmt.Errorf("latest: decoding release list: %w", err)
	}

	var best update.Version
	found := false
	for _, r := range releases {
		if r.Draft {
			continue
		}
		v, err := update.ParseVersion(r.TagName)
		if err != nil {
			continue // not a "vX.Y.Z" tag at all (e.g. "channels")
		}
		if *channel == "stable" && (r.Prerelease || v.Pre != "") {
			continue
		}
		if !hasSignedManifest(r.Assets) {
			continue
		}
		if !found || update.CompareVersions(v, best) > 0 {
			best = v
			found = true
		}
	}
	if found {
		fmt.Println(best.String())
	}
	return nil
}
