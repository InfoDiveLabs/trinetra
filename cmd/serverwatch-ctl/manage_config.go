// manage_config.go is the pure logic behind the "all settings" screen (issue
// #91): a generic browse/edit flow over config.Keys() that reaches every
// flat config key, including the ~20 that had no dedicated screen (sampling
// intervals, baseline/anomaly tuning, global thresholds,
// critical_overrides_quiet, storage.* (restart required), collection
// toggles, the remaining web.* keys the guided web-setup wizard doesn't
// collect, and public.*). It is deliberately unit-tested against plain
// *config.Config values with no terminal involved, mirroring
// manage_schedule.go/manage_quiet.go/manage_health.go/manage_monitor.go;
// manage_ui.go stays the thin Bubble Tea glue that drives these functions
// (group list -> key list -> value input) the same fetch-fresh/mutate-pure/
// ApplyConfig shape every other management screen already uses.
package main

import "github.com/InfoDiveLabs/trinetra/internal/config"

// settingsGroups returns the distinct groups named in config.Keys(), in the
// catalog's own first-seen order, for the "all settings" screen's top level
// group list.
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

// settingsGroupKeys returns the catalog entries belonging to group, in
// catalog order, for the key list shown after a group is selected. An
// unrecognized group name returns no rows.
func settingsGroupKeys(group string) []config.KeyInfo {
	var out []config.KeyInfo
	for _, ki := range config.Keys() {
		if ki.Group == group {
			out = append(out, ki)
		}
	}
	return out
}

// applyConfigKey sets exactly one key on cfg via config.Set -- the same
// validated setter `serverwatch config set` and every other manage screen
// ultimately use -- so the generic screen's value input gets each key's
// real validation for free and can never disagree with config.go. Returns
// config.Set's error unapplied: the caller must not persist an invalid
// value or call ApplyConfig on it.
func applyConfigKey(cfg *config.Config, key, raw string) error {
	return cfg.Set(key, raw)
}
