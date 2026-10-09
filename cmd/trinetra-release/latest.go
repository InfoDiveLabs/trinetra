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
// repos/{owner}/{repo}/releases --paginate`, that cmdLatest needs. The REST
// endpoint is used because `gh release list --json` has no assets field and so
// cannot tell a release with a signed manifest from one without. REST fields are
// snake_case (tag_name/prerelease/draft).
type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Prerelease bool      `json:"prerelease"`
	Draft      bool      `json:"draft"`
	Assets     []ghAsset `json:"assets"`
}

// requiredManifestAssets are the asset names a release must carry, all three,
// before cmdLatest considers it. Hosts need all three to verify a signed manifest
// (installSignature and FetchRelease both require them), so a channel pointer at
// a release lacking any would point every host at a version it can never apply.
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
// --paginate` from stdin and prints the HIGHEST matching version by
// update.ParseVersion/CompareVersions ordering, not gh's creation-date order, so
// a backport or out-of-order publish cannot move a channel pointer backwards.
//
// A tag that is not a valid "vX.Y.Z" or "vX.Y.Z-pre" (e.g. the "channels"
// release holding signed pointers) is skipped, as is any draft and any release
// missing one of manifest.json, manifest.ci.sig and manifest.maint.sig, since no
// host could install it.
//
//   - --channel stable also requires !Prerelease AND no pre-release part in the
//     parsed version, so a tag wrongly left unmarked as a prerelease cannot
//     reach stable.
//   - --channel beta considers every remaining tag and picks the highest under
//     full semver precedence (a final release outranks a pre-release of the same
//     core version).
//
// Prints nothing and exits 0 if no tag matches; channels.yml then skips signing a
// pointer for that channel.
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
