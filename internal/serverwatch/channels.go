package serverwatch

import (
	"fmt"

	"serverwatch/internal/config"
	"serverwatch/internal/telegram"
)

// buildNotifier is the extension point that turns a stored ChannelConfig
// into a live Notifier. It is a factory switch on cc.Type. It takes the full
// *config.Config (not just cc) because some channel types' secrets live
// outside per-channel Settings — telegram's bot token, in particular, is
// kept in Config.Telegram.Token rather than duplicated into every telegram
// channel's Settings map.
//
// EXTENSION POINT: later tasks add cases here as each channel type's
// concrete Notifier implementation lands. Until a type has a case, every
// Type value falls through to the default branch and reports "not
// implemented yet" — this lets the config/CLI/routing plumbing in this task
// be built, tested, and used (via `channel add`/`channel set`) well before
// any notifier actually exists, and `channel test` starts working
// automatically the moment a case is added.
func buildNotifier(cc config.ChannelConfig, c *config.Config) (Notifier, error) {
	switch cc.Type {
	case "telegram":
		token := cc.Settings["token"]
		if token == "" {
			token = c.Telegram.Token
		}
		chatID := cc.Settings["chat_id"]
		if chatID == "" {
			chatID = c.Telegram.ChatID
		}
		if token == "" {
			return nil, fmt.Errorf("telegram channel %q: token not configured (set channel setting.token or telegram.token)", cc.Name)
		}
		if chatID == "" {
			return nil, fmt.Errorf("telegram channel %q: chat_id not configured (set channel setting.chat_id or telegram.chat_id)", cc.Name)
		}
		return &telegramNotifier{client: telegram.New(token, chatID), name: cc.Name}, nil
	case "email":
		host := cc.Settings["host"]
		if host == "" {
			return nil, fmt.Errorf("email channel %q: host not configured (set channel setting.host)", cc.Name)
		}
		from := cc.Settings["from"]
		if from == "" {
			return nil, fmt.Errorf("email channel %q: from not configured (set channel setting.from)", cc.Name)
		}
		toRaw := cc.Settings["to"]
		if toRaw == "" {
			return nil, fmt.Errorf("email channel %q: to not configured (set channel setting.to)", cc.Name)
		}
		to := splitEmailList(toRaw)
		if len(to) == 0 {
			return nil, fmt.Errorf("email channel %q: to not configured (set channel setting.to)", cc.Name)
		}

		port := cc.Settings["port"]
		if port == "" {
			port = "587"
		}

		starttls := true
		if v, ok := cc.Settings["starttls"]; ok {
			starttls = v != "false" && v != "0"
		}

		return &emailNotifier{
			name:     cc.Name,
			host:     host,
			port:     port,
			username: cc.Settings["username"],
			password: cc.Settings["password"],
			from:     from,
			to:       to,
			starttls: starttls,
		}, nil
	case "webhook":
		url := cc.Settings["url"]
		if url == "" {
			return nil, fmt.Errorf("webhook channel %q: url not configured (set channel setting.url)", cc.Name)
		}

		method := cc.Settings["method"]
		if method == "" {
			method = "POST"
		}

		contentType := cc.Settings["content_type"]
		if contentType == "" {
			contentType = "application/json"
		}

		tmplStr := cc.Settings["template"]
		if tmplStr == "" {
			tmplStr = defaultWebhookTemplate
		}
		tmpl, err := parseWebhookTemplate(tmplStr)
		if err != nil {
			return nil, fmt.Errorf("webhook channel %q: %w", cc.Name, err)
		}

		return &webhookNotifier{
			name:        cc.Name,
			url:         url,
			method:      method,
			contentType: contentType,
			tmpl:        tmpl,
		}, nil
	case "slack":
		url := cc.Settings["url"]
		if url == "" {
			return nil, fmt.Errorf("slack channel %q: url not configured (set channel setting.url to the Slack incoming-webhook URL)", cc.Name)
		}
		return &webhookNotifier{
			name:        cc.Name,
			url:         url,
			method:      "POST",
			contentType: "application/json",
			tmpl:        slackTmpl,
		}, nil
	case "discord":
		url := cc.Settings["url"]
		if url == "" {
			return nil, fmt.Errorf("discord channel %q: url not configured (set channel setting.url to the Discord webhook URL)", cc.Name)
		}
		return &webhookNotifier{
			name:        cc.Name,
			url:         url,
			method:      "POST",
			contentType: "application/json",
			tmpl:        discordTmpl,
		}, nil
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
		n, err := buildNotifier(cc, c)
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
// telegram.chat_id keys keep working regardless (buildNotifier's telegram
// case and pollLoop in daemon.go still read them directly, as a fallback and
// for the command-reply interface respectively) — this makes the channel
// exist so it shows up in `channel list` and can be managed like any other
// channel, and carries the legacy global CriticalOverridesQuiet setting over
// into the new channel's Route so migrated users see no behavior change.
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
		// Carry over the legacy global CriticalOverridesQuiet (true by
		// default, see config.Default) into the migrated channel's own Route
		// field. Now that outbound sends go through the per-channel Route
		// exclusively (see daemon.go's Dispatcher wiring) rather than
		// consulting the global flag directly, dropping this would silently
		// regress "critical alerts bypass quiet hours" for anyone who never
		// touched channel config.
		CriticalOverridesQuiet: c.CriticalOverridesQuiet,
	})
	return true
}
