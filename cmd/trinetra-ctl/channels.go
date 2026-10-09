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

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// channelTypeChoices enumerates every channel type buildNotifier
// (internal/trinetra/channels.go) implements, in the order offered when adding a channel.
var channelTypeChoices = []string{"telegram", "email", "webhook", "slack", "discord", "ntfy", "gotify"}

// channelFieldDef is one per-type Settings field the add/edit screen walks the user
// through, in order.
type channelFieldDef struct {
	Key      string
	Label    string
	Required bool
}

// channelTypeFields lists channelFieldDefs per channel type, mirroring exactly what
// buildNotifier reads out of cc.Settings for that type (internal/trinetra/channels.go).
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

// channelAnswers accumulates the Channels screen's add/edit input: which type, whether it's
// enabled, and that type's Settings fields (channelTypeFields).
type channelAnswers struct {
	Name     string
	Type     string
	Enabled  bool
	Settings map[string]string
}

// buildChannelConfig turns ans into a config.ChannelConfig ready to add or validate:
// MinSeverity defaults to "info".
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

// applyChannelAdd appends a new channel built from ans onto cfg, mirroring `trinetra
// channel add`'s (internal/trinetra/channel.go) own duplicate-name check: it refuses.
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

// applyChannelEdit replaces the named channel's Type/Enabled/Settings with ans's, in place,
// preserving its routing fields.
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

// channelNeedsValidation reports whether cc must pass api.ValidateChannel before being
// persisted: only ENABLED channels are gated (the #79-safe pattern).
func channelNeedsValidation(cc config.ChannelConfig) bool {
	return cc.Enabled
}

// saveChannel is the Channels screen's single save path for BOTH add and edit: it builds cc
// from ans and, when cc is enabled.
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

// sortedChannels returns a stable-ordered copy of cfg's channels sorted by name, the same
// order printChannelList (internal/trinetra/channel.go) uses for `channel list`.
func sortedChannels(cfg *config.Config) []config.ChannelConfig {
	out := append([]config.ChannelConfig(nil), cfg.Channels...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// copyChannelSettings returns a shallow copy of m.
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

// indexOfChannelType returns typ's index in channelTypeChoices, or 0 (the first choice) if
// typ is empty/unrecognized -- e.g. a brand-new add flow.
func indexOfChannelType(typ string) int {
	for i, t := range channelTypeChoices {
		if t == typ {
			return i
		}
	}
	return 0
}
