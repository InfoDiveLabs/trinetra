package trinetra

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

const usageChannel = `usage:
  trinetra channel list
  trinetra channel add <name> --type <type> [--set key=value ...] [--disabled]
  trinetra channel remove <name>
  trinetra channel set <name> <key> <value>
  trinetra channel test <name>`

// cmdChannel implements `trinetra channel ...`. Every subcommand first
// applies migrateTelegramChannel (best-effort persisted) so a pre-existing
// telegram.token setup shows up as a real channel without the user having
// to run `channel add` themselves.
func cmdChannel(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageChannel)
		return 2
	}
	c, err := loadCfg()
	if err != nil {
		switch args[0] {
		case "add", "remove", "set":
			fmt.Fprintf(stderr, "warning: existing config unreadable (%v); starting from defaults\n", err)
			c = config.Default()
		default:
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if migrateTelegramChannel(c) {
		_ = saveCfg(c) // best-effort; a save failure here shouldn't block the subcommand
	}

	switch args[0] {
	case "list":
		printChannelList(c)
		return 0
	case "add":
		return cmdChannelAdd(c, args[1:])
	case "remove":
		return cmdChannelRemove(c, args[1:])
	case "set":
		return cmdChannelSet(c, args[1:])
	case "test":
		return cmdChannelTest(c, args[1:])
	default:
		fmt.Fprintf(stderr, "unknown channel subcommand %q\n\n%s\n", args[0], usageChannel)
		return 2
	}
}

func cmdChannelAdd(c *config.Config, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, usageChannel)
		return 2
	}
	name := args[0]
	if _, ok := c.GetChannel(name); ok {
		fmt.Fprintf(stderr, "channel %q already exists\n", name)
		return 1
	}
	cc := config.ChannelConfig{Name: name, Enabled: true, MinSeverity: "info"}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--type":
			if i+1 >= len(rest) {
				fmt.Fprintln(stderr, "--type requires a value")
				return 2
			}
			i++
			cc.Type = rest[i]
		case "--set":
			if i+1 >= len(rest) {
				fmt.Fprintln(stderr, "--set requires a key=value")
				return 2
			}
			i++
			kv := strings.SplitN(rest[i], "=", 2)
			if len(kv) != 2 || kv[0] == "" {
				fmt.Fprintf(stderr, "invalid --set value %q, want key=value\n", rest[i])
				return 2
			}
			if cc.Settings == nil {
				cc.Settings = map[string]string{}
			}
			cc.Settings[kv[0]] = kv[1]
		case "--disabled":
			cc.Enabled = false
		default:
			fmt.Fprintf(stderr, "unknown flag %q\n\n%s\n", rest[i], usageChannel)
			return 2
		}
	}
	if cc.Type == "" {
		fmt.Fprintln(stderr, "channel add requires --type <type>")
		return 2
	}
	// #83: validate up front, same as the ctl Channels screen's
	// validate-before-save gate (saveChannel/channelNeedsValidation in
	// cmd/trinetra-ctl/channels.go). Only ENABLED channels are gated -- a
	// disabled channel can't misdeliver (it's never wired into the
	// Dispatcher while off), so it may still be staged with incomplete
	// settings via --disabled. buildNotifier is the same call
	// core.API.ValidateChannel wraps (coreapi_file.go/coreapi_inproc.go), so
	// this single-sources the required-field set instead of duplicating it
	// here.
	if cc.Enabled {
		if _, err := buildNotifier(cc, c); err != nil {
			fmt.Fprintf(stderr, "channel %q: not saved, validation failed: %v\n", cc.Name, err)
			return 1
		}
	}
	c.AddChannel(cc)
	if err := saveCfg(c); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	reloadDaemon()
	return 0
}

func cmdChannelRemove(c *config.Config, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: channel remove <name>")
		return 2
	}
	if !c.RemoveChannel(args[0]) {
		fmt.Fprintf(stderr, "unknown channel %q\n", args[0])
		return 1
	}
	if err := saveCfg(c); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	reloadDaemon()
	return 0
}

func cmdChannelSet(c *config.Config, args []string) int {
	if len(args) != 3 {
		fmt.Fprintln(stderr, "usage: channel set <name> <key> <value>")
		return 2
	}
	if err := c.SetChannelField(args[0], args[1], args[2]); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := saveCfg(c); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	reloadDaemon()
	return 0
}

func cmdChannelTest(c *config.Config, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: channel test <name>")
		return 2
	}
	name := args[0]
	if err := sendTestNotification(c, name, "cli"); err != nil {
		fmt.Fprintf(stderr, "channel %q: %v\n", name, err)
		return 1
	}
	cc, _ := c.GetChannel(name)
	fmt.Fprintf(stdout, "sent test notification via %q (%s)\n", name, cc.Type)
	return 0
}

// sendTestNotification builds the named channel's Notifier (buildNotifier)
// and sends it a fixed test Alert, reporting any failure along the way
// (unknown channel, an incomplete/invalid channel config, or the send
// itself failing) as a single error. source is threaded onto the test
// Alert's Source field so a channel that surfaces it (e.g. a webhook
// template referencing .Source) can tell a CLI-issued test apart from a
// web-issued one (issue #66's "send test" button, reached over the control
// socket via inprocAPI.TestChannel, coreapi_inproc.go).
//
// Shared by cmdChannelTest (`trinetra channel test <name>`) and the web
// channels page's "Send test" action so both paths exercise the exact same
// notifier-construction and delivery logic -- no channel type can behave
// differently for one caller than the other.
func sendTestNotification(c *config.Config, name, source string) error {
	cc, ok := c.GetChannel(name)
	if !ok {
		return fmt.Errorf("unknown channel %q", name)
	}
	n, err := buildNotifier(*cc, c)
	if err != nil {
		// Every channel type buildNotifier knows about (telegram, email,
		// webhook, slack, discord, ntfy, gotify) is implemented; an error
		// here means this channel's own settings are incomplete or invalid
		// (e.g. a missing token/url), not that the type is unsupported.
		return err
	}
	a := Alert{
		Key:      "test",
		Title:    "trinetra test",
		Body:     "This is a test notification from trinetra.",
		Severity: SevInfo,
		Kind:     "fire",
		Source:   source,
		Time:     time.Now().Unix(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := n.Send(ctx, a); err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	return nil
}

func printChannelList(c *config.Config) {
	names := make([]string, 0, len(c.Channels))
	byName := make(map[string]config.ChannelConfig, len(c.Channels))
	for _, cc := range c.Channels {
		names = append(names, cc.Name)
		byName[cc.Name] = cc
	}
	sort.Strings(names)
	fmt.Fprintf(stdout, "%-16s %-12s %-8s %-10s %s\n", "NAME", "TYPE", "ENABLED", "MIN-SEV", "ROUTES")
	for _, name := range names {
		cc := byName[name]
		sev := cc.MinSeverity
		if sev == "" {
			sev = "info"
		}
		fmt.Fprintf(stdout, "%-16s %-12s %-8v %-10s %s\n", cc.Name, cc.Type, cc.Enabled, sev, describeRoutes(cc))
	}
}

func describeRoutes(cc config.ChannelConfig) string {
	var parts []string
	if len(cc.IncludeKinds) > 0 {
		parts = append(parts, "include="+strings.Join(cc.IncludeKinds, ","))
	}
	if len(cc.ExcludeKinds) > 0 {
		parts = append(parts, "exclude="+strings.Join(cc.ExcludeKinds, ","))
	}
	if cc.CriticalOverridesQuiet {
		parts = append(parts, "critical-overrides-quiet")
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}
