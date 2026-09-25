// Package serverwatch: fleet_cmd.go is the `serverwatch fleet` command. Role
// changes (init/join/leave/disable) edit config and PKI files directly and
// ask for a restart, because the daemon reads the role once at start. Every
// other subcommand goes through the running daemon's control socket, which
// owns the node registry and token store.
package serverwatch

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/control"
	"serverwatch/internal/core"
	"serverwatch/internal/fleet"
	"serverwatch/internal/version"
)

const fleetUsage = `usage:
  serverwatch fleet init --address HOST[,IP] [--port 9443]   make this host the fleet master
  serverwatch fleet token create [--tags a,b] [--ttl 1h] [--uses 1]
  serverwatch fleet token list | token delete <id>
  serverwatch fleet join <code> [--name NAME]                  join a master as a child
  serverwatch fleet status | nodes [--tag T] [--state S] [--q TEXT]
  serverwatch fleet node revoke|rename|tag <node> [value]
  serverwatch fleet leave [--purge]                            child -> solo
  serverwatch fleet disable [--purge]                          master -> solo`

const restartHint = "Restart to apply: sudo systemctl restart serverwatch"

func cmdFleet(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, fleetUsage)
		return 2
	}
	switch args[0] {
	case "init":
		return fleetInit(args[1:])
	case "join":
		return fleetJoinCmd(args[1:])
	case "leave":
		return fleetLeave(args[1:])
	case "disable":
		return fleetDisable(args[1:])
	case "status":
		return withDaemon(func(c *control.Client) error { return fleetStatus(c) })
	case "nodes":
		return fleetNodes(args[1:])
	case "node":
		return fleetNodeCmd(args[1:])
	case "token":
		return fleetTokenCmd(args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, fleetUsage)
		return 0
	}
	fmt.Fprintf(stderr, "unknown fleet command %q\n\n%s\n", args[0], fleetUsage)
	return 2
}

func loadCfgForFleet() (*config.Config, error) {
	c, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return c, nil
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseInterspersed lets flags follow positional args (`join <code> --name x`).
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func fleetInit(args []string) int {
	fs := newFlags("fleet init")
	addr := fs.String("address", "", "comma-separated hostnames/IPs children use to reach this host (required)")
	port := fs.Int("port", 9443, "TCP port for the fleet listener")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	var hosts []string
	for _, h := range strings.Split(*addr, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		fmt.Fprintln(stderr, "fleet init: --address is required, e.g. --address monitor.example.com,203.0.113.7")
		return 2
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintln(stderr, "fleet init: --port must be 1-65535")
		return 2
	}
	c, err := loadCfgForFleet()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if c.FleetRole() != config.RoleSolo {
		fmt.Fprintf(stderr, "fleet init: this host is already a fleet %s; run `serverwatch fleet %s` first\n", c.FleetRole(), map[string]string{config.RoleMaster: "disable", config.RoleChild: "leave"}[c.FleetRole()])
		return 1
	}
	if err := fleetInitPKI(stateDir, hosts, "serverwatch fleet CA ("+c.ServerName()+")", time.Now()); err != nil {
		fmt.Fprintln(stderr, "fleet init:", err)
		return 1
	}
	pki := fleetPKIDir(stateDir)
	ca, err := fleet.LoadCA(pki+"/ca.crt", pki+"/ca.key")
	if err != nil {
		fmt.Fprintln(stderr, "fleet init:", err)
		return 1
	}
	c.Fleet.Role = config.RoleMaster
	c.Fleet.Address = strings.Join(hosts, ",")
	c.Fleet.Listen = fmt.Sprintf(":%d", *port)
	if err := saveCfg(c); err != nil {
		fmt.Fprintln(stderr, "fleet init: save config:", err)
		return 1
	}
	fmt.Fprintf(stdout, `Fleet master initialised.
  CA fingerprint: %s
  Children will reach this host at %s (open TCP %d).

%s
Then create a join code for each server:
  sudo serverwatch fleet token create --tags prod
`, fleet.SPKIPin(ca.Cert), fleetJoinURL(c), *port, restartHint)
	return 0
}

func fleetJoinCmd(args []string) int {
	fs := newFlags("fleet join")
	name := fs.String("name", "", "display name on the master (default: server.name)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "fleet join: expected exactly one join code (swj1_...)")
		return 2
	}
	c, err := loadCfgForFleet()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if c.FleetRole() != config.RoleSolo {
		fmt.Fprintf(stderr, "fleet join: this host is already a fleet %s; run `serverwatch fleet leave` (child) or `fleet disable` (master) first\n", c.FleetRole())
		return 1
	}
	n := *name
	if n == "" {
		n = c.ServerName()
	}
	hi, _ := json.Marshal(collectHostInfoFor(c))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := fleet.Join(ctx, pos[0], n, version.String(), hi, fleetChildDir(stateDir))
	if err != nil {
		fmt.Fprintln(stderr, "fleet join:", err)
		return 1
	}
	c.Fleet.Role = config.RoleChild
	c.Fleet.MasterURL, c.Fleet.CAPin, c.Fleet.NodeID = res.MasterURL, res.Pin, res.NodeID
	if err := saveCfg(c); err != nil {
		fmt.Fprintln(stderr, "fleet join: save config:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Joined fleet master %s as node %s (%s).\n%s\n", res.MasterURL, res.NodeID, n, restartHint)
	return 0
}

func fleetLeave(args []string) int {
	fs := newFlags("fleet leave")
	purge := fs.Bool("purge", false, "also delete this node's fleet identity and unsent outbox")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	c, err := loadCfgForFleet()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if c.FleetRole() != config.RoleChild {
		fmt.Fprintln(stderr, "fleet leave: this host is not a fleet child")
		return 1
	}
	c.Fleet.Role, c.Fleet.MasterURL, c.Fleet.CAPin, c.Fleet.NodeID = "", "", "", ""
	if err := saveCfg(c); err != nil {
		fmt.Fprintln(stderr, "fleet leave: save config:", err)
		return 1
	}
	if *purge {
		os.RemoveAll(fleetChildDir(stateDir))
		os.RemoveAll(fleetOutboxDir(stateDir))
		fmt.Fprintln(stdout, "Deleted this node's fleet identity and unsent outbox.")
	}
	fmt.Fprintf(stdout, "Left the fleet; this host is solo again (local history kept).\n%s\n", restartHint)
	return 0
}

func fleetDisable(args []string) int {
	fs := newFlags("fleet disable")
	purge := fs.Bool("purge", false, "also delete the CA, node registry and every node's replicated history")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	c, err := loadCfgForFleet()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if c.FleetRole() != config.RoleMaster {
		fmt.Fprintln(stderr, "fleet disable: this host is not a fleet master")
		return 1
	}
	c.Fleet.Role, c.Fleet.Address = "", ""
	if err := saveCfg(c); err != nil {
		fmt.Fprintln(stderr, "fleet disable: save config:", err)
		return 1
	}
	if *purge {
		os.RemoveAll(fleetMasterDir(stateDir))
		fmt.Fprintln(stdout, "Deleted fleet CA, registry and replicas.")
	} else {
		fmt.Fprintln(stdout, "Fleet data kept; `fleet init` again reuses the same CA so children need not re-join.")
	}
	fmt.Fprintf(stdout, "This host is solo again.\n%s\n", restartHint)
	return 0
}

var errDaemonDown = errors.New("serverwatch daemon not reachable (is it running? sudo systemctl status serverwatch)")

func withDaemon(f func(c *control.Client) error) int {
	tok, _ := os.ReadFile(controlTokenPath())
	c, err := control.Dial(controlSocketPath(), strings.TrimSpace(string(tok)))
	if err != nil {
		fmt.Fprintln(stderr, errDaemonDown)
		return 1
	}
	defer c.Close()
	if err := f(c); err != nil {
		fmt.Fprintln(stderr, "fleet:", err)
		return 1
	}
	return 0
}

func ago(ts int64) string {
	if ts == 0 {
		return "never"
	}
	d := time.Since(time.Unix(ts, 0)).Round(time.Second)
	if d < time.Minute {
		return d.String() + " ago"
	}
	return d.Round(time.Minute).String() + " ago"
}

func fleetStatus(c *control.Client) error {
	st, err := c.Fleet().Status()
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "role: %s\n", st.Role)
	switch st.Role {
	case config.RoleMaster:
		fmt.Fprintf(stdout, "listening: %s\njoin URL: %s\nCA fingerprint: %s\nnodes: %d (incl. this host)\n", st.Listen, st.JoinURL, st.CAPin, st.Nodes)
	case config.RoleChild:
		fmt.Fprintf(stdout, "node id: %s\nmaster: %s\n", st.NodeID, st.MasterURL)
		if l := st.Link; l != nil {
			fmt.Fprintf(stdout, "link: %s, last ack %s\noutbox: %.1f MB, %d unsent, %d gaps\n", l.State, ago(l.LastAck), float64(l.OutboxBytes)/(1<<20), l.Unacked, l.Gaps)
			if l.LastError != "" {
				fmt.Fprintf(stdout, "last error: %s\n", l.LastError)
			}
		}
	}
	return nil
}

func fleetNodes(args []string) int {
	fs := newFlags("fleet nodes")
	tag := fs.String("tag", "", "only nodes with this tag")
	state := fs.String("state", "", "only nodes in this state (online, lagging, stale, down, revoked)")
	q := fs.String("q", "", "search name, id or address")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		ns, err := c.Fleet().Nodes(core.NodeFilter{Tag: *tag, State: *state, Query: *q})
		if err != nil {
			return err
		}
		printNodes(stdout, ns)
		return nil
	})
}

func printNodes(w io.Writer, ns []core.NodeSummary) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tCPU\tMEM\tDISK\tVERSION\tLAST SEEN\tTAGS\tID")
	for _, n := range ns {
		id := n.ID
		if len(id) > 8 {
			id = id[:8]
		}
		fmt.Fprintf(tw, "%s\t%s\t%.0f%%\t%.0f%%\t%.0f%%\t%s\t%s\t%s\t%s\n", n.Name, n.State, n.CPU, n.MemPct, n.WorstDiskPct, n.Version, ago(n.LastSeen), strings.Join(n.Tags, ","), id)
	}
	tw.Flush()
}

// resolveNodeRef maps an id, id prefix (>= 6 chars) or exact name to one node id.
func resolveNodeRef(c *control.Client, ref string) (string, error) {
	ns, err := c.Fleet().Nodes(core.NodeFilter{})
	if err != nil {
		return "", err
	}
	var hits []string
	for _, n := range ns {
		if n.Self {
			continue
		}
		if n.ID == ref || n.Name == ref || (len(ref) >= 6 && strings.HasPrefix(n.ID, ref)) {
			hits = append(hits, n.ID)
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no node matches %q (see `serverwatch fleet nodes`)", ref)
	case 1:
		return hits[0], nil
	}
	return "", fmt.Errorf("%q matches %d nodes; use the node id", ref, len(hits))
}

func fleetNodeCmd(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: serverwatch fleet node revoke|rename|tag <node> [value]")
		return 2
	}
	verb, ref := args[0], args[1]
	return withDaemon(func(c *control.Client) error {
		id, err := resolveNodeRef(c, ref)
		if err != nil {
			return err
		}
		switch verb {
		case "revoke":
			if err := c.Fleet().RevokeNode(id); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Revoked %s; it can no longer send data. Its history is kept.\n", id)
		case "rename":
			if len(args) != 3 {
				return errors.New("usage: fleet node rename <node> <new-name>")
			}
			return c.Fleet().RenameNode(id, args[2])
		case "tag":
			if len(args) != 3 {
				return errors.New("usage: fleet node tag <node> tag1,tag2 (empty string clears)")
			}
			var tags []string
			for _, t := range strings.Split(args[2], ",") {
				if t = strings.TrimSpace(t); t != "" {
					tags = append(tags, t)
				}
			}
			return c.Fleet().SetNodeTags(id, tags)
		default:
			return fmt.Errorf("unknown node action %q (revoke, rename, tag)", verb)
		}
		return nil
	})
}

func fleetTokenCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: serverwatch fleet token create|list|delete")
		return 2
	}
	switch args[0] {
	case "create":
		fs := newFlags("fleet token create")
		tags := fs.String("tags", "", "comma-separated tags new nodes get")
		ttl := fs.Duration("ttl", time.Hour, "how long the code stays valid")
		uses := fs.Int("uses", 1, "how many servers may join with it")
		if _, err := parseInterspersed(fs, args[1:]); err != nil {
			return 2
		}
		var tl []string
		for _, t := range strings.Split(*tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tl = append(tl, t)
			}
		}
		return withDaemon(func(c *control.Client) error {
			ct, err := c.Fleet().CreateToken(core.TokenSpec{TTLSeconds: int64(ttl.Seconds()), Uses: *uses, Tags: tl, Creator: "cli"})
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Run this on each server to add (valid %s, %d use(s)):\n\n  sudo serverwatch fleet join %s\n\n", ttl.String(), *uses, ct.JoinCode)
			return nil
		})
	case "list":
		return withDaemon(func(c *control.Client) error {
			ts, err := c.Fleet().Tokens()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tUSES LEFT\tEXPIRES\tTAGS")
			for _, t := range ts {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", t.ID, t.Uses, time.Unix(t.Expires, 0).Format(time.RFC3339), strings.Join(t.Tags, ","))
			}
			return tw.Flush()
		})
	case "delete":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: serverwatch fleet token delete <id>")
			return 2
		}
		return withDaemon(func(c *control.Client) error { return c.Fleet().DeleteToken(args[1]) })
	}
	fmt.Fprintf(stderr, "unknown token command %q\n", args[0])
	return 2
}
