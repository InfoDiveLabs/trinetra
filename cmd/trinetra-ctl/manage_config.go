// manage_config.go is the pure logic behind the "all settings" screen (issue #91): a
// generic browse/edit flow over config.Keys() that reaches every flat config key.
package main

import (
	"fmt"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// settingsGroups returns the distinct groups named in config.Keys(), in the catalog's own
// first-seen order, for the "all settings" screen's top level group list.
func settingsGroups() []string {
	var groups []string
	seen := map[string]bool{}
	for _, ki := range config.Keys() {
		if !seen[ki.Group] {
			seen[ki.Group] = true
			groups = append(groups, ki.Group)
		}
	}
	return groups
}

// settingsGroupKeys returns the catalog entries belonging to group, in catalog order, for
// the key list shown after a group is selected.
func settingsGroupKeys(group string) []config.KeyInfo {
	var out []config.KeyInfo
	for _, ki := range config.Keys() {
		if ki.Group == group {
			out = append(out, ki)
		}
	}
	return out
}

// applyConfigKey sets exactly one key on cfg via config.Set -- the same validated setter
// `trinetra config set` and every other manage screen ultimately use.
func applyConfigKey(cfg *config.Config, key, raw string) error {
	return cfg.Set(key, raw)
}

// managedFragmentFor reports the fragment id currently managing key on the daemon api.
func managedFragmentFor(api core.API, key string) (fragmentID string, managed bool) {
	fp, ok := api.(core.FleetProvider)
	if !ok {
		return "", false
	}
	st, err := fp.Fleet().Status()
	if err != nil || st.Link == nil {
		return "", false
	}
	id, ok := st.Link.Managed[key]
	return id, ok
}

// managedFragmentError formats the standard refusal message for a managed key, matching
// `trinetra config set`'s own wording.
func managedFragmentError(key, fragmentID string) error {
	return fmt.Errorf("%s: managed by the fleet master (fragment %s); change it on the master", key, fragmentID)
}
