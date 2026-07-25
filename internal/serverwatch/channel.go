package serverwatch

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"serverwatch/internal/config"
)

const usageChannel = `usage:
  serverwatch channel list
  serverwatch channel add <name> --type <type> [--set key=value ...]
  serverwatch channel remove <name>
  serverwatch channel set <name> <key> <value>
  serverwatch channel test <name>`

// cmdChannel implements `serverwatch channel ...`. Every subcommand first
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
		default:
			fmt.Fprintf(stderr, "unknown flag %q\n\n%s\n", rest[i], usageChannel)
			return 2
		}
	}
	if cc.Type == "" {
		fmt.Fprintln(stderr, "channel add requires --type <type>")
		return 2
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
	cc, ok := c.GetChannel(name)
	if !ok {
		fmt.Fprintf(stderr, "unknown channel %q\n", name)
		return 1
	}
	n, err := buildNotifier(*cc)
	if err != nil {
		// Deliberately surfaces buildNotifier's "not implemented yet" error
		// as-is: `channel test` starts working automatically once a later
		// task adds a case for this type, with no change needed here.
		fmt.Fprintf(stderr, "channel %q: %v\n", name, err)
		return 1
	}
	a := Alert{
		Key:      "test",
		Title:    "serverwatch test",
		Body:     fmt.Sprintf("test notification from serverwatch for channel %q", name),
		Severity: SevInfo,
		Kind:     "fire",
		Source:   "cli",
		Time:     time.Now().Unix(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := n.Send(ctx, a); err != nil {
		fmt.Fprintf(stderr, "test send via %q failed: %v\n", name, err)
		return 1
	}
	fmt.Fprintf(stdout, "test alert sent via %q\n", name)
	return 0
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
