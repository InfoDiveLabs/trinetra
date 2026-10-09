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
type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Prerelease bool      `json:"prerelease"`
	Draft      bool      `json:"draft"`
	Assets     []ghAsset `json:"assets"`
}

// requiredManifestAssets are the asset names a release must carry, all three, before
// cmdLatest considers it.
var requiredManifestAssets = []string{"manifest.json", "manifest.ci.sig", "manifest.maint.sig"}

// hasSignedManifest reports whether assets includes every name in requiredManifestAssets.
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
