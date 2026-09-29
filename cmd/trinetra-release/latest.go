package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// ghRelease is the subset of `gh release list --json
// tagName,isPrerelease,isDraft` that cmdLatest needs.
type ghRelease struct {
	TagName      string `json:"tagName"`
	IsPrerelease bool   `json:"isPrerelease"`
	IsDraft      bool   `json:"isDraft"`
}

// cmdLatest implements: latest --channel stable|beta
//
// It reads a JSON array shaped like `gh release list --json
// tagName,isPrerelease,isDraft` from stdin and prints the HIGHEST version
// among the matching releases, using update.ParseVersion/CompareVersions
// ordering rather than gh's list order (which is by creation date, not
// version — review F2/I2: a backport or an out-of-order publish must not
// move a channel pointer backwards or skip a higher version).
//
// A tag that isn't a valid "vX.Y.Z" or "vX.Y.Z-pre" (e.g. the "channels"
// release used to store signed pointers) is skipped rather than treated as
// an error. A draft is always skipped, defensively, even though the caller
// is expected to already pass --exclude-drafts to `gh release list`.
//
//   - --channel stable additionally requires !IsPrerelease AND that the
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
		if r.IsDraft {
			continue
		}
		v, err := update.ParseVersion(r.TagName)
		if err != nil {
			continue // not a "vX.Y.Z" tag at all (e.g. "channels")
		}
		if *channel == "stable" && (r.IsPrerelease || v.Pre != "") {
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
