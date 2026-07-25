package serverwatch

import (
	"fmt"

	"serverwatch/internal/config"
)

// buildNotifier is the extension point that turns a stored ChannelConfig
// into a live Notifier. It is a factory switch on cc.Type.
//
// EXTENSION POINT: later tasks (t4 telegram, t6 email, ...) add cases here
// as each channel type's concrete Notifier implementation lands. Until a
// type has a case, every Type value falls through to the default branch and
// reports "not implemented yet" — this lets the config/CLI/routing plumbing
// in this task be built, tested, and used (via `channel add`/`channel set`)
// well before any notifier actually exists, and `channel test` starts
// working automatically the moment a case is added.
func buildNotifier(cc config.ChannelConfig) (Notifier, error) {
	switch cc.Type {
	default:
		return nil, fmt.Errorf("channel type %q not implemented yet", cc.Type)
	}
}

// routeFromChannelConfig converts a ChannelConfig's routing fields into the
// Route the Dispatcher evaluates against each Alert. An empty MinSeverity is
// treated permissively as "info" (see ChannelConfig.MinSeverity doc).
func routeFromChannelConfig(cc config.ChannelConfig) Route {
	sev := cc.MinSeverity
	if sev == "" {
		sev = "info"
	}
	// config.SetChannelField/Load already reject anything but
	// info|warning|critical|"" for MinSeverity, so this should never fail in
	// practice; fall back to the most permissive severity rather than
	// silently dropping the channel if it somehow does.
	s, err := ParseSeverity(sev)
	if err != nil {
		s = SevInfo
	}
	return Route{
		MinSeverity:            s,
		IncludeKinds:           cc.IncludeKinds,
		ExcludeKinds:           cc.ExcludeKinds,
		CriticalOverridesQuiet: cc.CriticalOverridesQuiet,
	}
}

// channelsFromConfig builds the Dispatcher's []Channel from the user's
// stored channel config. A channel whose Notifier isn't available yet
// (buildNotifier returns an error) is skipped, with a note logged to
// stderr, rather than failing the whole set — one type not being
// implemented yet must not take down every other configured channel.
func channelsFromConfig(c *config.Config) []Channel {
	var out []Channel
	for _, cc := range c.Channels {
		n, err := buildNotifier(cc)
		if err != nil {
			fmt.Fprintf(stderr, "channel %q: %v; skipping\n", cc.Name, err)
			continue
		}
		out = append(out, Channel{N: n, Route: routeFromChannelConfig(cc), Enabled: cc.Enabled})
	}
	return out
}

// migrateTelegramChannel back-fills a "telegram" ChannelConfig from the
// legacy Telegram.Token/Telegram.ChatID keys, if a token is set and no
// telegram-typed channel already exists. It reports whether it changed c,
// so callers can decide whether to persist. Idempotent: safe to call on
// every CLI invocation and daemon startup. The legacy telegram.token/
// telegram.chat_id keys keep working regardless (tgClient in daemon.go
// still reads them directly) — this only makes the channel exist so it
// shows up in `channel list` and can be managed like any other channel.
func migrateTelegramChannel(c *config.Config) bool {
	if c.Telegram.Token == "" {
		return false
	}
	// Guard on Name (the unique key used everywhere else — GetChannel/
	// RemoveChannel/SetChannelField/list all key on Name), not just Type: a
	// config that already has a channel *named* "telegram" of any type (e.g.
	// hand-edited/restored as a webhook) must not get a second one appended,
	// which would collapse in list, strand one on remove, and shadow the
	// other from the CLI. Also skip if a telegram-typed channel exists under
	// any name, since the migration goal (a working telegram channel) is met.
	if _, ok := c.GetChannel("telegram"); ok {
		return false
	}
	for _, cc := range c.Channels {
		if cc.Type == "telegram" {
			return false
		}
	}
	c.AddChannel(config.ChannelConfig{
		Name:        "telegram",
		Type:        "telegram",
		Enabled:     true,
		MinSeverity: "info",
		Settings:    map[string]string{"chat_id": c.Telegram.ChatID},
	})
	return true
}
