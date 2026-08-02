// channels.go holds the Channels screen's PURE logic: the add/edit/remove
// config mutations and the #79-safe validate-before-save gate, all unit-
// tested (channels_test.go) against a plain *config.Config and the fake
// core.API (run_test.go) with no terminal involved. The Bubble Tea glue
// that walks the user through these (manage_channels.go) is deliberately
// thin, mirroring the split manage_schedule.go/setup_web.go already
// establish for their own screens.
package main

import (
	"fmt"
	"sort"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// channelTypeChoices enumerates every channel type buildNotifier
// (internal/serverwatch/channels.go) implements, in the order offered when
// adding a channel.
var channelTypeChoices = []string{"telegram", "email", "webhook", "slack", "discord", "ntfy", "gotify"}

// channelFieldDef is one per-type Settings field the add/edit screen walks
// the user through, in order. Key matches the Settings map key buildNotifier
// reads for that type (internal/serverwatch/channels.go); Required is
// informational only here -- buildNotifier (via the validate-before-save
// gate below) is still the source of truth an empty required field fails
// against, this package does not duplicate that check.
type channelFieldDef struct {
	Key      string
	Label    string
	Required bool
}

// channelTypeFields lists channelFieldDefs per channel type, mirroring
// exactly what buildNotifier reads out of cc.Settings for that type
// (internal/serverwatch/channels.go), so the ctl screen never asks for (or
// omits) a field the daemon doesn't actually use.
var channelTypeFields = map[string][]channelFieldDef{
	"telegram": {
		{Key: "token", Label: "bot token (blank: use the global telegram.token)"},
		{Key: "chat_id", Label: "chat id (blank: use the global telegram.chat_id)"},
	},
	"email": {
		{Key: "host", Label: "smtp host", Required: true},
		{Key: "port", Label: "smtp port (default 587)"},
		{Key: "username", Label: "smtp username"},
		{Key: "password", Label: "smtp password"},
		{Key: "from", Label: "from address", Required: true},
		{Key: "to", Label: "to address(es), comma separated", Required: true},
		{Key: "starttls", Label: "starttls true/false (default true)"},
	},
	"webhook": {
		{Key: "url", Label: "webhook url", Required: true},
		{Key: "method", Label: "http method (default POST)"},
		{Key: "content_type", Label: "content-type (default application/json)"},
		{Key: "template", Label: "body template (blank: default json)"},
	},
	"slack": {
		{Key: "url", Label: "slack incoming-webhook url", Required: true},
	},
	"discord": {
		{Key: "url", Label: "discord webhook url", Required: true},
	},
	"ntfy": {
		{Key: "topic", Label: "topic", Required: true},
		{Key: "server", Label: "server (default https://ntfy.sh)"},
		{Key: "token", Label: "access token (optional)"},
	},
	"gotify": {
		{Key: "server", Label: "server url", Required: true},
		{Key: "token", Label: "application token", Required: true},
	},
}

// channelAnswers accumulates the Channels screen's add/edit input: which
// type, whether it's enabled, and that type's Settings fields
// (channelTypeFields), keyed the same as config.ChannelConfig.Settings. Name
// is only meaningful for add (edit identifies the channel by its existing
// name, passed separately -- see applyChannelEdit/saveChannel).
type channelAnswers struct {
	Name     string
	Type     string
	Enabled  bool
	Settings map[string]string
}

// buildChannelConfig turns ans into a config.ChannelConfig ready to add or
// validate: MinSeverity defaults to "info" (empty is the permissive default
// wherever routing is evaluated, see ChannelConfig.MinSeverity's doc), and
// Settings only carries non-empty values so a blank optional field (e.g.
// telegram's token/chat_id, meant to fall back to the global telegram.*
// keys per buildNotifier) doesn't shadow that fallback with an explicit "".
func buildChannelConfig(ans channelAnswers) config.ChannelConfig {
	cc := config.ChannelConfig{
		Name:        ans.Name,
		Type:        ans.Type,
		Enabled:     ans.Enabled,
		MinSeverity: "info",
	}
	for k, v := range ans.Settings {
		if v == "" {
			continue
		}
		if cc.Settings == nil {
			cc.Settings = map[string]string{}
		}
		cc.Settings[k] = v
	}
	return cc
}

// applyChannelAdd appends a new channel built from ans onto cfg, mirroring
// `serverwatch channel add`'s (internal/serverwatch/channel.go) own
// duplicate-name check: it refuses, leaving cfg untouched, if a channel
// named ans.Name already exists (or ans.Name is empty).
func applyChannelAdd(cfg *config.Config, ans channelAnswers) error {
	if ans.Name == "" {
		return fmt.Errorf("channel name is required")
	}
	if _, ok := cfg.GetChannel(ans.Name); ok {
		return fmt.Errorf("channel %q already exists", ans.Name)
	}
	cfg.AddChannel(buildChannelConfig(ans))
	return nil
}

// applyChannelEdit replaces the named channel's Type/Enabled/Settings with
// ans's, in place, preserving its routing fields (MinSeverity/
// IncludeKinds/ExcludeKinds/CriticalOverridesQuiet) exactly as `channel
// set`'s per-field setters would leave them untouched -- editing a
// channel's delivery settings on this screen was never meant to reset
// routing rules set elsewhere.
func applyChannelEdit(cfg *config.Config, name string, ans channelAnswers) error {
	cc, ok := cfg.GetChannel(name)
	if !ok {
		return fmt.Errorf("unknown channel %q", name)
	}
	cc.Type = ans.Type
	cc.Enabled = ans.Enabled
	cc.Settings = buildChannelConfig(ans).Settings
	return nil
}

// applyChannelRemove deletes the named channel from cfg, mirroring `channel
// remove`'s unknown-channel error.
func applyChannelRemove(cfg *config.Config, name string) error {
	if !cfg.RemoveChannel(name) {
		return fmt.Errorf("unknown channel %q", name)
	}
	return nil
}

// channelNeedsValidation reports whether cc must pass api.ValidateChannel
// before being persisted: only ENABLED channels are gated (the #79-safe
// pattern) -- a disabled channel can't misdeliver, so saving one with an
// incomplete/invalid Settings map is harmless, and is exactly how a user
// stages a channel's config before turning it on.
func channelNeedsValidation(cc config.ChannelConfig) bool {
	return cc.Enabled
}

// saveChannel is the Channels screen's single save path for BOTH add and
// edit: it builds cc from ans and, when cc is enabled, checks it against
// api.ValidateChannel BEFORE touching cfg at all -- refusing to persist an
// enabled channel that would be silently dropped at delivery time (issue
// #79, the same concern core.API.ValidateChannel's doc describes). Only
// once that gate passes (or cc is disabled, so there's nothing to
// misdeliver) does it apply the actual mutation via applyChannelAdd/
// applyChannelEdit. Nothing is applied to cfg, and nothing needs undoing,
// when the gate rejects cc. name is the existing channel's name for an edit
// (ignored for add, where ans.Name is used instead).
func saveChannel(api core.API, cfg *config.Config, name string, ans channelAnswers, isEdit bool) error {
	cc := buildChannelConfig(ans)
	if isEdit {
		cc.Name = name
	}
	if channelNeedsValidation(cc) {
		if err := api.ValidateChannel(cc); err != nil {
			return fmt.Errorf("channel %q: not saved, validation failed: %w", cc.Name, err)
		}
	}
	if isEdit {
		return applyChannelEdit(cfg, name, ans)
	}
	return applyChannelAdd(cfg, ans)
}

// sortedChannels returns a stable-ordered copy of cfg's channels sorted by
// name, the same order printChannelList (internal/serverwatch/channel.go)
// uses for `channel list`, so the ctl screen's row order never depends on
// json.Unmarshal's (unspecified) slice order.
func sortedChannels(cfg *config.Config) []config.ChannelConfig {
	out := append([]config.ChannelConfig(nil), cfg.Channels...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// copyChannelSettings returns a shallow copy of m, so seeding an edit's
// channelAnswers.Settings from an existing channel's Settings never lets
// typing in the field screen mutate the config the list screen is still
// displaying (that config is only replaced wholesale once saveChannelCmd's
// ApplyConfig round trip lands).
func copyChannelSettings(m map[string]string) map[string]string {
	if len(m) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// indexOfChannelType returns typ's index in channelTypeChoices, or 0 (the
// first choice) if typ is empty/unrecognized -- e.g. a brand-new add flow,
// or an edit of a channel whose Type predates channelTypeChoices somehow.
func indexOfChannelType(typ string) int {
	for i, t := range channelTypeChoices {
		if t == typ {
			return i
		}
	}
	return 0
}
