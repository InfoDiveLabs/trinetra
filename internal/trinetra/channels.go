package trinetra

import (
	"context"
	"fmt"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/telegram"
)

// buildNotifier is the extension point that turns a stored ChannelConfig into a live
// Notifier.
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
			client:      newGuardedHTTPClient(webhookRequestTimeout, c.Notify.BlockPrivateTargets),
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
			client:      newGuardedHTTPClient(webhookRequestTimeout, c.Notify.BlockPrivateTargets),
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
			client:      newGuardedHTTPClient(webhookRequestTimeout, c.Notify.BlockPrivateTargets),
		}, nil
	case "ntfy":
		topic := cc.Settings["topic"]
		if topic == "" {
			return nil, fmt.Errorf("ntfy channel %q: topic not configured (set channel setting.topic)", cc.Name)
		}
		server := cc.Settings["server"]
		if server == "" {
			server = defaultNtfyServer
		}
		return &ntfyNotifier{
			name:   cc.Name,
			server: strings.TrimRight(server, "/"),
			topic:  topic,
			token:  cc.Settings["token"],
			client: newGuardedHTTPClient(pushRequestTimeout, c.Notify.BlockPrivateTargets),
		}, nil
	case "gotify":
		server := cc.Settings["server"]
		if server == "" {
			return nil, fmt.Errorf("gotify channel %q: server not configured (set channel setting.server)", cc.Name)
		}
		token := cc.Settings["token"]
		if token == "" {
			return nil, fmt.Errorf("gotify channel %q: token not configured (set channel setting.token to a Gotify application token)", cc.Name)
		}
		return &gotifyNotifier{
			name:   cc.Name,
			server: strings.TrimRight(server, "/"),
			token:  token,
			client: newGuardedHTTPClient(pushRequestTimeout, c.Notify.BlockPrivateTargets),
		}, nil
	default:
		return nil, fmt.Errorf("channel type %q not implemented yet", cc.Type)
	}
}

// routeFromChannelConfig converts a ChannelConfig's routing fields into the Route the
// Dispatcher evaluates against each Alert.
func routeFromChannelConfig(cc config.ChannelConfig) Route {
	sev := cc.MinSeverity
	if sev == "" {
		sev = "info"
	}
	// config.SetChannelField/Load already reject anything but info|warning|critical|"" for
	// MinSeverity, so this should never fail in practice.
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

// channelsFromConfig builds the Dispatcher's []Channel from the user's stored channel
// config.
func channelsFromConfig(c *config.Config) []Channel {
	var out []Channel
	name := c.ServerName()
	for _, cc := range c.Channels {
		n, err := buildNotifier(cc, c)
		if err != nil {
			fmt.Fprintf(stderr, "channel %q: %v; skipping\n", cc.Name, err)
			continue
		}
		// Prefix the host name onto every delivered alert title so a multi-host setup shows which
		// host fired (#101).
		out = append(out, Channel{N: hostPrefixNotifier{inner: n, serverName: name}, Route: routeFromChannelConfig(cc), Enabled: cc.Enabled})
	}
	return out
}

// hostPrefixNotifier decorates a Notifier to prepend "[<server.name>] " to the alert title
// at send time. serverName is the resolved config.ServerName().
type hostPrefixNotifier struct {
	inner      Notifier
	serverName string
}

func (h hostPrefixNotifier) Name() string { return h.inner.Name() }

func (h hostPrefixNotifier) Send(ctx context.Context, a Alert) error {
	a.Title = titleWithHost(h.serverName, a.Title) // a is a value copy; the stored alert is untouched
	return h.inner.Send(ctx, a)
}

// titleWithHost prefixes serverName onto title as "[serverName] title", or
// returns title unchanged when serverName is empty.
func titleWithHost(serverName, title string) string {
	if serverName == "" {
		return title
	}
	return "[" + serverName + "] " + title
}

// withChatID returns a copy of c with the enrolled Telegram chat set and a telegram channel
// added if none exists.
func withChatID(c *config.Config, id string) *config.Config {
	nc := *c
	nc.Channels = append([]config.ChannelConfig(nil), c.Channels...)
	nc.Telegram.ChatID = id
	migrateTelegramChannel(&nc)
	return &nc
}

// migrateTelegramChannel back-fills a "telegram" ChannelConfig from the legacy
// Telegram.Token/Telegram.ChatID keys.
func migrateTelegramChannel(c *config.Config) bool {
	if c.Telegram.Token == "" {
		return false
	}
	// Guard on Name (the unique key used everywhere else -- GetChannel/
	// RemoveChannel/SetChannelField/list all key on Name), not just Type.
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
		// Carry over the legacy global CriticalOverridesQuiet (true by default, see
		// config.Default) into the migrated channel's own Route field.
		CriticalOverridesQuiet: c.CriticalOverridesQuiet,
	})
	return true
}
