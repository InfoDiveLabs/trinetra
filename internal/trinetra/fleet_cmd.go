// Package trinetra: fleet_cmd.go is the `trinetra fleet` command. Role
// changes (init/join/leave/disable) edit config and PKI files directly and
// ask for a restart, because the daemon reads the role once at start. Every
// other subcommand goes through the running daemon's control socket, which
// owns the node registry and token store.
package trinetra

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
	"github.com/InfoDiveLabs/trinetra/internal/version"
)

const fleetUsage = `usage:
  trinetra fleet init --address HOST[,IP] [--port 9443]   make this host the fleet master
  trinetra fleet token create [--tags a,b] [--ttl 1h] [--uses 1]
  trinetra fleet token list | token delete <id>
  trinetra fleet join <code> [--name NAME]                  join a master as a child
  trinetra fleet status | nodes [--tag T] [--state S] [--q TEXT]
  trinetra fleet node revoke|remove|rename|tag <node> [value]
  trinetra fleet incidents [--state firing]                 list incidents
  trinetra fleet incident <id>                              show one incident
  trinetra fleet ack <id>                                   acknowledge an incident
  trinetra fleet explain <key|id>                           print an alert's pipeline trail
  trinetra fleet silence add --match tag=web,rule=cpu* --for 2h [--comment C]
  trinetra fleet silence list | expire <id>
  trinetra fleet maintenance add --name N --match ... --days mon,tue --from 22:00 --to 02:00 --tz Asia/Kolkata
  trinetra fleet maintenance list | delete <id>
  trinetra fleet route test --node web1 [--tag t] --rule cpu --severity critical
  trinetra fleet alerting show | apply <file.json>
  trinetra fleet rules                                      list aggregate rules and their state
  trinetra fleet managed list | status
  trinetra fleet managed set [--tag T] key=value [key=value ...]
  trinetra fleet managed delete <id>
  trinetra fleet leave [--purge]                            child -> solo
  trinetra fleet disable [--purge]                          master -> solo`

const restartHint = "Restart to apply: sudo systemctl restart trinetra"

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
	case "incidents":
		return fleetIncidentsCmd(args[1:])
	case "incident":
		return fleetIncidentCmd(args[1:])
	case "ack":
		return fleetAckCmd(args[1:])
	case "explain":
		return fleetExplainCmd(args[1:])
	case "silence":
		return fleetSilenceCmd(args[1:])
	case "maintenance":
		return fleetMaintenanceCmd(args[1:])
	case "route":
		return fleetRouteCmd(args[1:])
	case "alerting":
		return fleetAlertingCmd(args[1:])
	case "rules":
		return fleetRulesCmd(args[1:])
	case "managed":
		return fleetManagedCmd(args[1:])
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

// rejectPositionals prints and returns true if pos is non-empty: a command
// that takes no positional arguments (init, leave, disable, nodes, token
// create/list) but received one is a usage error, not silently ignored.
func rejectPositionals(cmd, usage string, pos []string) bool {
	if len(pos) == 0 {
		return false
	}
	fmt.Fprintf(stderr, "%s: unexpected argument %q\nusage: %s\n", cmd, pos[0], usage)
	return true
}

func fleetInit(args []string) int {
	fs := newFlags("fleet init")
	addr := fs.String("address", "", "comma-separated hostnames/IPs children use to reach this host (required)")
	port := fs.Int("port", 9443, "TCP port for the fleet listener")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if rejectPositionals("fleet init", "trinetra fleet init --address HOST[,IP] [--port 9443]", pos) {
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
		fmt.Fprintf(stderr, "fleet init: this host is already a fleet %s; run `trinetra fleet %s` first\n", c.FleetRole(), map[string]string{config.RoleMaster: "disable", config.RoleChild: "leave"}[c.FleetRole()])
		return 1
	}
	if err := fleetInitPKI(stateDir, hosts, "trinetra fleet CA ("+c.ServerName()+")", time.Now()); err != nil {
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
  sudo trinetra fleet token create --tags prod
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
		fmt.Fprintf(stderr, "fleet join: this host is already a fleet %s; run `trinetra fleet leave` (child) or `fleet disable` (master) first\n", c.FleetRole())
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
	// An older master's JoinResponse has no "name" field at all, which
	// decodes as "" here -- that must read as "the master didn't report a
	// final name" (fall back to what was requested), never as "the master
	// registered this node under the empty string" (review round 3, item 1:
	// the old code printed a false "registered as \"\" instead" note against
	// any pre-round-2 master).
	finalName := res.Name
	if finalName == "" {
		finalName = n
	}
	if finalName != n {
		fmt.Fprintf(stdout, "Note: %q was already taken on this master; registered as %q instead.\n", n, finalName)
	}
	fmt.Fprintf(stdout, "Joined fleet master %s as node %s (%s).\n%s\n", res.MasterURL, res.NodeID, finalName, restartHint)
	return 0
}

func fleetLeave(args []string) int {
	fs := newFlags("fleet leave")
	purge := fs.Bool("purge", false, "also delete this node's fleet identity and unsent outbox")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if rejectPositionals("fleet leave", "trinetra fleet leave [--purge]", pos) {
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
	nodeID := c.Fleet.NodeID
	c.Fleet.Role, c.Fleet.MasterURL, c.Fleet.CAPin, c.Fleet.NodeID = "", "", "", ""
	if err := saveCfg(c); err != nil {
		fmt.Fprintln(stderr, "fleet leave: save config:", err)
		return 1
	}
	// task 8 ruling: the last managed values are kept as ordinary local
	// config (they already are -- c above was never touched for them), only
	// the managed-config sidecar itself is removed, so this host stops
	// treating them as master-managed/read-only. Best-effort: a missing
	// sidecar (never managed) is not an error.
	_ = os.Remove(managedChildPath(stateDir))
	ok := true
	if *purge {
		ok = purgeAll("left the fleet", []purgeTarget{
			{path: fleetChildDir(stateDir), what: "this node's fleet identity"},
			{path: fleetOutboxDir(stateDir), what: "unsent outbox"},
		})
	}
	fmt.Fprintf(stdout, "Left the fleet; this host is solo again (local history kept).\n%s\n", restartHint)
	// Leaving is local only: the master keeps expecting this node and will
	// page it as down until it is revoked or removed there.
	fmt.Fprintf(stdout, "\nThe master will report this node as down until you tell it the node is gone.\nOn the master, run: sudo trinetra fleet node revoke %s\n(or `sudo trinetra fleet node remove %s` to also drop it from the node list; its history is kept)\n", nodeID, nodeID)
	if !ok {
		return 1
	}
	return 0
}

func fleetDisable(args []string) int {
	fs := newFlags("fleet disable")
	purge := fs.Bool("purge", false, "also delete the CA, node registry and every node's replicated history")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if rejectPositionals("fleet disable", "trinetra fleet disable [--purge]", pos) {
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
	ok := true
	if *purge {
		ok = purgeAll("disabled the fleet master", []purgeTarget{
			{path: fleetMasterDir(stateDir), what: "fleet CA, registry and replicas"},
		})
	} else {
		fmt.Fprintln(stdout, "Fleet data kept; `fleet init` again reuses the same CA so children need not re-join.")
	}
	fmt.Fprintf(stdout, "This host is solo again.\n%s\n", restartHint)
	if !ok {
		return 1
	}
	return 0
}

// purgeTarget is one directory a --purge flag deletes, with what naming it
// for the success/failure messages.
type purgeTarget struct {
	path string
	what string
}

// purgeAll removes each target's path with os.RemoveAll. Config has already
// been switched back to solo by the time this runs, so a removal failure is
// reported (which path, why) without pretending the role change failed too;
// it just means the operator has cleanup left to do. verb is the sentence
// prefix used in a failure line ("left the fleet" / "disabled the fleet
// master"). The success line lists only the targets that were actually
// removed. It reports whether every target was removed.
func purgeAll(verb string, targets []purgeTarget) bool {
	var deleted []string
	ok := true
	for _, t := range targets {
		if err := os.RemoveAll(t.path); err != nil {
			fmt.Fprintf(stderr, "%s, but could not delete %s: %v; remove it manually\n", verb, t.path, err)
			ok = false
			continue
		}
		deleted = append(deleted, t.what)
	}
	if len(deleted) > 0 {
		fmt.Fprintf(stdout, "Deleted %s.\n", strings.Join(deleted, " and "))
	}
	return ok
}

var errDaemonDown = errors.New("trinetra daemon not reachable (is it running? sudo systemctl status trinetra)")

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
		ns, err := c.Fleet().Nodes(core.NodeFilter{})
		if err != nil {
			return err
		}
		printNodeWarnings(stdout, ns)
	case config.RoleChild:
		fmt.Fprintf(stdout, "node id: %s\nmaster: %s\n", st.NodeID, st.MasterURL)
		if l := st.Link; l != nil {
			state := l.State
			if state == fleet.LinkCatchingUp {
				state += " (master reachable; unsent data is being retried)"
			}
			fmt.Fprintf(stdout, "link: %s, last ack %s\noutbox: %.1f MB, %d unsent, %d gaps\n", state, ago(l.LastAck), float64(l.OutboxBytes)/(1<<20), l.Unacked, l.Gaps)
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
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if rejectPositionals("fleet nodes", "trinetra fleet nodes [--tag T] [--state S] [--q TEXT]", pos) {
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

// fmtSkew renders a node's clock skew (server_time - sent_at): negative
// means the node's clock is ahead of the master's.
func fmtSkew(n core.NodeSummary) string {
	switch {
	case n.Self:
		return "-"
	case n.SkewSec == 0:
		return "0s"
	}
	return fmt.Sprintf("%+ds", n.SkewSec)
}

// skewWarnCLI mirrors the master's 30 s skew warning line.
const skewWarnCLI = 30

func printNodes(w io.Writer, ns []core.NodeSummary) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tSKEW\tCPU\tMEM\tDISK\tVERSION\tLAST SEEN\tTAGS\tDEPENDS ON\tID")
	for _, n := range ns {
		id := n.ID
		if len(id) > 8 {
			id = id[:8]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.0f%%\t%.0f%%\t%.0f%%\t%s\t%s\t%s\t%s\t%s\n", n.Name, n.State, fmtSkew(n), n.CPU, n.MemPct, n.WorstDiskPct, n.Version, ago(n.LastSeen), strings.Join(n.Tags, ","), strings.Join(n.DependsOn, ","), id)
	}
	tw.Flush()
}

// printNodeWarnings lists (on a master's fleet status) every node whose
// replica has refused points, with the counts, and warns about every node
// whose clock is off by more than 30 s.
func printNodeWarnings(w io.Writer, ns []core.NodeSummary) {
	for _, n := range ns {
		if n.Self {
			continue
		}
		if n.DroppedOutOfOrder > 0 || n.DroppedCardinality > 0 || n.DroppedDuplicate > 0 {
			// Cumulative counts, shown for reference: the master logs a
			// warning when out-of-order or over-limit drops grow.
			fmt.Fprintf(w, "replica drops: %s (%s): %d out of order, %d over the series limit, %d duplicates (harmless re-sends)\n", n.Name, n.ID, n.DroppedOutOfOrder, n.DroppedCardinality, n.DroppedDuplicate)
		}
		if n.SkewSec > skewWarnCLI || n.SkewSec < -skewWarnCLI {
			fmt.Fprintf(w, "warning: %s (%s) clock differs from this master's by %s (fix NTP on that host)\n", n.Name, n.ID, fmtSkew(n))
		}
	}
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
		return "", fmt.Errorf("no node matches %q (see `trinetra fleet nodes`)", ref)
	case 1:
		return hits[0], nil
	}
	return "", fmt.Errorf("%q matches %d nodes; use the node id", ref, len(hits))
}

func fleetNodeCmd(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: trinetra fleet node revoke|remove|rename|tag|depends <node> [value]")
		return 2
	}
	verb, ref := args[0], args[1]
	if (verb == "revoke" || verb == "remove") && len(args) != 2 {
		fmt.Fprintf(stderr, "usage: trinetra fleet node %s <node>\n", verb)
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		id, err := resolveNodeRef(c, ref)
		if err != nil {
			return err
		}
		switch verb {
		case "revoke":
			if err := c.Fleet().RevokeNode(id, "cli"); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Revoked %s; it can no longer send data. Its history is kept.\n", id)
		case "remove":
			if err := c.Fleet().RemoveNode(id, "cli"); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Removed %s from the fleet; it can no longer send data and is no longer monitored. Its history is kept on disk.\n", id)
		case "rename":
			if len(args) != 3 {
				return errors.New("usage: fleet node rename <node> <new-name>")
			}
			return c.Fleet().RenameNode(id, args[2], "cli")
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
			return c.Fleet().SetNodeTags(id, tags, "cli")
		case "depends":
			if len(args) != 3 {
				return errors.New("usage: fleet node depends <node> dep1,dep2,tag:t (empty string clears)")
			}
			var deps []string
			for _, d := range strings.Split(args[2], ",") {
				if d = strings.TrimSpace(d); d != "" {
					deps = append(deps, d)
				}
			}
			return c.Fleet().SetNodeDeps(id, deps, "cli")
		default:
			return fmt.Errorf("unknown node action %q (revoke, remove, rename, tag, depends)", verb)
		}
		return nil
	})
}

func fleetTokenCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: trinetra fleet token create|list|delete")
		return 2
	}
	switch args[0] {
	case "create":
		fs := newFlags("fleet token create")
		tags := fs.String("tags", "", "comma-separated tags new nodes get")
		ttl := fs.Duration("ttl", time.Hour, "how long the code stays valid")
		uses := fs.Int("uses", 1, "how many servers may join with it")
		pos, err := parseInterspersed(fs, args[1:])
		if err != nil {
			return 2
		}
		if rejectPositionals("fleet token create", "trinetra fleet token create [--tags a,b] [--ttl 1h] [--uses 1]", pos) {
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
			fmt.Fprintf(stdout, "Run this on each server to add (valid %s, %d use(s)):\n\n  sudo trinetra fleet join %s\n\n", ttl.String(), *uses, ct.JoinCode)
			return nil
		})
	case "list":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: trinetra fleet token list")
			return 2
		}
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
			fmt.Fprintln(stderr, "usage: trinetra fleet token delete <id>")
			return 2
		}
		return withDaemon(func(c *control.Client) error { return c.Fleet().DeleteToken(args[1], "cli") })
	}
	fmt.Fprintf(stderr, "unknown token command %q\n", args[0])
	return 2
}

func fleetIncidentsCmd(args []string) int {
	fs := newFlags("fleet incidents")
	state := fs.String("state", "", "only incidents in this state (firing, acked, resolved, suppressed)")
	node := fs.String("node", "", "only incidents involving this node")
	tag := fs.String("tag", "", "only incidents involving a node with this tag")
	limit := fs.Int("limit", 0, "max incidents to show (0 = unlimited)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if rejectPositionals("fleet incidents", "trinetra fleet incidents [--state S] [--node N] [--tag T] [--limit N]", pos) {
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		incs, err := c.Fleet().Incidents(core.IncidentFilter{State: *state, Node: *node, Tag: *tag, Limit: *limit})
		if err != nil {
			return err
		}
		printIncidents(stdout, incs)
		return nil
	})
}

func printIncidents(w io.Writer, incs []core.Incident) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tSEVERITY\tTITLE\tNODES\tOPENED\tUPDATED")
	for _, inc := range incs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", inc.ID, inc.State, inc.Severity, inc.Title, strings.Join(inc.Nodes, ","), ago(inc.Opened), ago(inc.Updated))
	}
	tw.Flush()
}

func fleetIncidentCmd(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: trinetra fleet incident <id>")
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		inc, err := c.Fleet().Incident(args[0])
		if err != nil {
			return err
		}
		printIncidentDetail(stdout, inc)
		return nil
	})
}

func printIncidentDetail(w io.Writer, inc core.Incident) {
	fmt.Fprintf(w, "id: %s\nstate: %s\nseverity: %s\ntitle: %s\nnodes: %s\nopened: %s\nupdated: %s\n",
		inc.ID, inc.State, inc.Severity, inc.Title, strings.Join(inc.Nodes, ","),
		time.Unix(inc.Opened, 0).Format(time.RFC3339), time.Unix(inc.Updated, 0).Format(time.RFC3339))
	if inc.Resolved > 0 {
		fmt.Fprintf(w, "resolved: %s\n", time.Unix(inc.Resolved, 0).Format(time.RFC3339))
	}
	if inc.AckedBy != "" {
		fmt.Fprintf(w, "acked by: %s\n", inc.AckedBy)
	}
	fmt.Fprintln(w, "alerts:")
	for _, a := range inc.Alerts {
		fmt.Fprintf(w, "  %s %s (%s) fired %s", a.Node, a.Key, a.Severity, ago(a.FiredAt))
		if a.ResolvedAt > 0 {
			fmt.Fprintf(w, " resolved %s", ago(a.ResolvedAt))
		}
		if a.DeliveredLocally {
			fmt.Fprint(w, " [delivered locally]")
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "timeline:")
	printIncidentEvents(w, inc.Timeline)
}

func printIncidentEvents(w io.Writer, events []core.IncidentEvent) {
	for _, e := range events {
		fmt.Fprintf(w, "  %s %s", time.Unix(e.TS, 0).Format(time.RFC3339), e.Kind)
		if e.Detail != "" {
			fmt.Fprintf(w, ": %s", e.Detail)
		}
		if e.Actor != "" {
			fmt.Fprintf(w, " (%s)", e.Actor)
		}
		fmt.Fprintln(w)
	}
}

func fleetAckCmd(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: trinetra fleet ack <id>")
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		if err := c.Fleet().AckIncident(args[0], "cli"); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Acknowledged incident %s.\n", args[0])
		return nil
	})
}

func fleetExplainCmd(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: trinetra fleet explain <key|id>")
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		events, err := c.Fleet().Explain(args[0])
		if err != nil {
			return err
		}
		printIncidentEvents(stdout, events)
		return nil
	})
}

// --- silence / maintenance -------------------------------------------------

// parseMatchSpec parses a comma-separated "key=value" matcher spec, e.g.
// "tag=web,node=db*,rule=cpu*,severity=critical", into a single core.Matcher
// (AND semantics across its fields; empty fields match anything).
func parseMatchSpec(spec string) (core.Matcher, error) {
	var m core.Matcher
	if strings.TrimSpace(spec) == "" {
		return m, errors.New("--match is required, e.g. --match tag=web,rule=cpu*")
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return core.Matcher{}, fmt.Errorf("invalid --match term %q (want key=value)", part)
		}
		k, v := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		switch k {
		case "tag":
			m.Tag = v
		case "node":
			m.Node = v
		case "rule":
			m.Rule = v
		case "severity":
			m.Severity = v
		default:
			return core.Matcher{}, fmt.Errorf("unknown match key %q (want tag, node, rule or severity)", k)
		}
	}
	return m, nil
}

func formatMatchers(ms []core.Matcher) string {
	var parts []string
	for _, m := range ms {
		var kv []string
		if m.Tag != "" {
			kv = append(kv, "tag="+m.Tag)
		}
		if m.Node != "" {
			kv = append(kv, "node="+m.Node)
		}
		if m.Rule != "" {
			kv = append(kv, "rule="+m.Rule)
		}
		if m.Severity != "" {
			kv = append(kv, "severity="+m.Severity)
		}
		parts = append(parts, strings.Join(kv, "&"))
	}
	return strings.Join(parts, " OR ")
}

func fleetSilenceCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: trinetra fleet silence add|list|expire")
		return 2
	}
	switch args[0] {
	case "add":
		return fleetSilenceAdd(args[1:])
	case "list":
		return fleetSilenceList(args[1:])
	case "expire":
		return fleetSilenceExpire(args[1:])
	}
	fmt.Fprintf(stderr, "unknown silence command %q\n", args[0])
	return 2
}

func fleetSilenceAdd(args []string) int {
	fs := newFlags("fleet silence add")
	match := fs.String("match", "", "comma-separated matchers: tag=,node=,rule=,severity= "+
		"(node= matches the node name (glob) or its exact id; renaming a node stops name-based silences from matching it)")
	forDur := fs.Duration("for", 0, "how long the silence lasts from now")
	until := fs.String("until", "", "RFC3339 end time (alternative to --for)")
	comment := fs.String("comment", "", "why")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	usage := "trinetra fleet silence add --match tag=web,node=db*,rule=cpu*,severity=critical --for 2h | --until RFC3339 [--comment ...]\n" +
		"  node= matches the node name (glob) or its exact id; renaming a node stops name-based silences from matching it."
	if rejectPositionals("fleet silence add", usage, pos) {
		return 2
	}
	m, err := parseMatchSpec(*match)
	if err != nil {
		fmt.Fprintln(stderr, "fleet silence add:", err)
		return 2
	}
	var end int64
	switch {
	case *forDur > 0:
		end = time.Now().Add(*forDur).Unix()
	case *until != "":
		t, err := time.Parse(time.RFC3339, *until)
		if err != nil {
			fmt.Fprintln(stderr, "fleet silence add: --until must be RFC3339:", err)
			return 2
		}
		end = t.Unix()
	default:
		fmt.Fprintln(stderr, "fleet silence add: one of --for or --until is required")
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		sil, err := c.Fleet().CreateSilence(core.Silence{
			Matchers: []core.Matcher{m}, Start: time.Now().Unix(), End: end, Author: "cli", Comment: *comment,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Created silence %s, active until %s.\n", sil.ID, time.Unix(sil.End, 0).Format(time.RFC3339))
		return nil
	})
}

func fleetSilenceList(args []string) int {
	if rejectPositionals("fleet silence list", "trinetra fleet silence list", args) {
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		sils, err := c.Fleet().Silences()
		if err != nil {
			return err
		}
		printSilences(stdout, sils)
		return nil
	})
}

func printSilences(w io.Writer, sils []core.Silence) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tMATCH\tSTART\tEND\tAUTHOR\tCOMMENT")
	for _, s := range sils {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, formatMatchers(s.Matchers),
			time.Unix(s.Start, 0).Format(time.RFC3339), time.Unix(s.End, 0).Format(time.RFC3339), s.Author, s.Comment)
	}
	tw.Flush()
}

func fleetSilenceExpire(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: trinetra fleet silence expire <id>")
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		if err := c.Fleet().ExpireSilence(args[0], "cli"); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Expired silence %s.\n", args[0])
		return nil
	})
}

var weekdayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
var weekdayShort = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

func parseWeekdays(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		wd, ok := weekdayNames[part]
		if !ok {
			return nil, fmt.Errorf("invalid --days value %q (want mon,tue,wed,thu,fri,sat,sun)", part)
		}
		out = append(out, wd)
	}
	if len(out) == 0 {
		return nil, errors.New("--days is required, e.g. --days mon,tue")
	}
	return out, nil
}

func formatWeekdays(wds []int) string {
	var out []string
	for _, wd := range wds {
		if wd >= 0 && wd < 7 {
			out = append(out, weekdayShort[wd])
		}
	}
	return strings.Join(out, ",")
}

func fleetMaintenanceCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: trinetra fleet maintenance add|list|delete")
		return 2
	}
	switch args[0] {
	case "add":
		return fleetMaintenanceAdd(args[1:])
	case "list":
		return fleetMaintenanceList(args[1:])
	case "delete":
		return fleetMaintenanceDelete(args[1:])
	}
	fmt.Fprintf(stderr, "unknown maintenance command %q\n", args[0])
	return 2
}

func fleetMaintenanceAdd(args []string) int {
	fs := newFlags("fleet maintenance add")
	name := fs.String("name", "", "name")
	match := fs.String("match", "", "comma-separated matchers")
	days := fs.String("days", "", "comma-separated weekdays: mon,tue,wed,thu,fri,sat,sun")
	from := fs.String("from", "", "start time HH:MM (in --tz)")
	to := fs.String("to", "", "end time HH:MM (in --tz; before --from means it crosses midnight)")
	tz := fs.String("tz", "UTC", "IANA time zone")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	usage := "trinetra fleet maintenance add --name N --match ... --days mon,tue --from HH:MM --to HH:MM --tz TZ"
	if rejectPositionals("fleet maintenance add", usage, pos) {
		return 2
	}
	if strings.TrimSpace(*name) == "" {
		fmt.Fprintln(stderr, "fleet maintenance add: --name is required")
		return 2
	}
	m, err := parseMatchSpec(*match)
	if err != nil {
		fmt.Fprintln(stderr, "fleet maintenance add:", err)
		return 2
	}
	wds, err := parseWeekdays(*days)
	if err != nil {
		fmt.Fprintln(stderr, "fleet maintenance add:", err)
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		mw, err := c.Fleet().SaveMaintenance(core.Maintenance{
			Name: *name, Matchers: []core.Matcher{m}, Weekdays: wds, From: *from, To: *to, TZ: *tz, Author: "cli",
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Created maintenance window %s (%s).\n", mw.ID, mw.Name)
		return nil
	})
}

func fleetMaintenanceList(args []string) int {
	if rejectPositionals("fleet maintenance list", "trinetra fleet maintenance list", args) {
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		ms, err := c.Fleet().Maintenances()
		if err != nil {
			return err
		}
		printMaintenances(stdout, ms)
		return nil
	})
}

func printMaintenances(w io.Writer, ms []core.Maintenance) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tMATCH\tDAYS\tFROM\tTO\tTZ\tAUTHOR")
	for _, m := range ms {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.Name, formatMatchers(m.Matchers),
			formatWeekdays(m.Weekdays), m.From, m.To, m.TZ, m.Author)
	}
	tw.Flush()
}

func fleetMaintenanceDelete(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: trinetra fleet maintenance delete <id>")
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		if err := c.Fleet().DeleteMaintenance(args[0], "cli"); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Deleted maintenance window %s.\n", args[0])
		return nil
	})
}

func fleetRouteCmd(args []string) int {
	if len(args) == 0 || args[0] != "test" {
		fmt.Fprintln(stderr, "usage: trinetra fleet route test --node NAME [--tag t1,t2] --rule RULE --severity SEV")
		return 2
	}
	return fleetRouteTest(args[1:])
}

func fleetRouteTest(args []string) int {
	fs := newFlags("fleet route test")
	node := fs.String("node", "", "node display name or id")
	tag := fs.String("tag", "", "comma-separated tags")
	rule := fs.String("rule", "", "alert key/rule")
	severity := fs.String("severity", "", "severity (info, warning, critical)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	usage := "trinetra fleet route test --node NAME [--tag t1,t2] --rule RULE --severity SEV"
	if rejectPositionals("fleet route test", usage, pos) {
		return 2
	}
	var tags []string
	if strings.TrimSpace(*tag) != "" {
		tags = strings.Split(*tag, ",")
	}
	return withDaemon(func(c *control.Client) error {
		d, err := c.Fleet().RouteTest(core.TestAlert{Node: *node, Tags: tags, Rule: *rule, Severity: *severity})
		if err != nil {
			return err
		}
		printRouteDecision(stdout, d)
		return nil
	})
}

// printRouteDecision prints d: the matched route, then EVERY matched policy
// (more than one when Continue chained several routes together -- B5 fix
// round 1: each escalates independently, so each gets its own steps and
// repeat_every printed separately) and, last, whether a silence would
// suppress this exact alert.
func printRouteDecision(w io.Writer, d core.RouteDecision) {
	route := d.Route
	if route == "" {
		route = "(no route matched; using the default policy)"
	}
	fmt.Fprintf(w, "route: %s\n", route)
	for _, p := range d.Policies {
		fmt.Fprintf(w, "policy: %s\n", p.Name)
		for i, s := range p.Steps {
			fmt.Fprintf(w, "  step %d: after %s -> %s\n", i, s.After, strings.Join(s.Channels, ", "))
		}
		if p.RepeatEvery != "" {
			fmt.Fprintf(w, "  repeat_every: %s\n", p.RepeatEvery)
		}
	}
	if d.Suppressed != "" {
		fmt.Fprintf(w, "suppressed: %s\n", d.Suppressed)
	} else {
		fmt.Fprintln(w, "suppressed: no")
	}
}

func fleetAlertingCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: trinetra fleet alerting show|apply <file.json>")
		return 2
	}
	switch args[0] {
	case "show":
		return fleetAlertingShow(args[1:])
	case "apply":
		return fleetAlertingApply(args[1:])
	}
	fmt.Fprintf(stderr, "unknown alerting command %q\n", args[0])
	return 2
}

func fleetAlertingShow(args []string) int {
	if rejectPositionals("fleet alerting show", "trinetra fleet alerting show", args) {
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		cfg, err := c.Fleet().Alerting()
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(b))
		return nil
	})
}

// fleetAlertingApply applies file's config with Version 0 (unconditional):
// the CLI does not do optimistic locking (that is plan C's web editor's
// job), so it always overwrites whatever is currently stored.
func fleetAlertingApply(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: trinetra fleet alerting apply <file.json>")
		return 2
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "fleet alerting apply:", err)
		return 1
	}
	var cfg core.AlertingConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		fmt.Fprintf(stderr, "fleet alerting apply: parse %s: %v\n", args[0], err)
		return 1
	}
	cfg.Version = 0
	return withDaemon(func(c *control.Client) error {
		if err := c.Fleet().SetAlerting(cfg, "cli"); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Applied alerting config from %s.\n", args[0])
		return nil
	})
}

// fleetRulesCmd is `trinetra fleet rules` (task 7): a table of every
// configured aggregate rule's current value/firing state.
func fleetRulesCmd(args []string) int {
	if rejectPositionals("fleet rules", "trinetra fleet rules", args) {
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		states, err := c.Fleet().RuleStates()
		if err != nil {
			return err
		}
		printRuleStates(stdout, states)
		return nil
	})
}

// ruleStateLabel renders one rule's STATE column: an Error takes priority
// (the Expr currently fails to parse -- shouldn't happen, since SetAlerting
// validates it, but surfaced rather than hidden if it ever does), then
// "no data" (task-7 ruling: never fires/recovers, holds the previous
// firing/since), then firing/ok.
func ruleStateLabel(s core.RuleState) string {
	switch {
	case s.Error != "":
		return "error: " + s.Error
	case s.NoData:
		return "no data"
	case s.Firing:
		return "firing"
	}
	return "ok"
}

// fleetManagedCmd is `trinetra fleet managed` (task 8): CRUD + status over
// the master's managed-config fragments.
func fleetManagedCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: trinetra fleet managed list|set|delete|status")
		return 2
	}
	switch args[0] {
	case "list":
		return fleetManagedList(args[1:])
	case "set":
		return fleetManagedSet(args[1:])
	case "delete":
		return fleetManagedDelete(args[1:])
	case "status":
		return fleetManagedStatus(args[1:])
	}
	fmt.Fprintf(stderr, "unknown managed command %q\n", args[0])
	return 2
}

func fleetManagedList(args []string) int {
	if rejectPositionals("fleet managed list", "trinetra fleet managed list", args) {
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		frags, err := c.Fleet().Managed()
		if err != nil {
			return err
		}
		printManagedFragments(stdout, frags)
		return nil
	})
}

func printManagedFragments(w io.Writer, frags []core.ManagedFragment) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTAG\tVERSION\tAUTHOR\tVALUES")
	for _, f := range frags {
		tag := f.Tag
		if tag == "" {
			tag = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", f.ID, tag, f.Version, f.Author, formatManagedValues(f.Values))
	}
	tw.Flush()
}

// formatManagedValues renders values as "key=value,key=value,..." in
// managedFragmentAllowlistKeys order, so the same fragment always prints
// identically regardless of Go's randomized map iteration.
func formatManagedValues(values map[string]string) string {
	var parts []string
	for _, k := range managedFragmentAllowlistKeys {
		if v, ok := values[k]; ok {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, ",")
}

// parseManagedKV parses one or more "key=value" positional arguments into a
// map, rejecting anything malformed (no "=") -- the value itself is
// whatever config.Set will ultimately validate, so no parsing happens here.
func parseManagedKV(args []string) (map[string]string, error) {
	if len(args) == 0 {
		return nil, errors.New("at least one key=value is required")
	}
	out := map[string]string{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid key=value %q", a)
		}
		out[k] = v
	}
	return out, nil
}

// fleetManagedSet is `trinetra fleet managed set [--tag T] key=value ...`:
// creates a new fragment for --tag ("" = every node), or updates the
// existing one for that tag if one already exists (task-8 ruling: "one
// fragment per tag, simplest") -- entirely replacing its Values with what
// was given here, not merging.
func fleetManagedSet(args []string) int {
	fs := newFlags("fleet managed set")
	tag := fs.String("tag", "", "target only nodes carrying this tag (default: every node)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	values, err := parseManagedKV(pos)
	if err != nil {
		fmt.Fprintln(stderr, "fleet managed set:", err)
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		// Round-1 review MINOR: no list-then-decide here any more -- an
		// empty ID is a server-side upsert-by-tag (managedFragmentStore.Save,
		// atomic under its own lock), so this can never race a concurrent
		// `fleet managed set --tag X` into creating two fragments for the
		// same tag (the TOCTOU a client-side list+create/update used to
		// have).
		saved, err := c.Fleet().SaveManaged(core.ManagedFragment{Tag: *tag, Values: values}, "cli")
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Saved managed-config fragment %s (tag=%s, version %d): %s\n",
			saved.ID, firstNonEmptyStr(saved.Tag, "*"), saved.Version, formatManagedValues(saved.Values))
		return nil
	})
}

func firstNonEmptyStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func fleetManagedDelete(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: trinetra fleet managed delete <id>")
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		if err := c.Fleet().DeleteManaged(args[0], "cli"); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Deleted managed-config fragment %s.\n", args[0])
		return nil
	})
}

func fleetManagedStatus(args []string) int {
	if rejectPositionals("fleet managed status", "trinetra fleet managed status", args) {
		return 2
	}
	return withDaemon(func(c *control.Client) error {
		sts, err := c.Fleet().ManagedStatus()
		if err != nil {
			return err
		}
		printManagedStatuses(stdout, sts)
		return nil
	})
}

func printManagedStatuses(w io.Writer, sts []core.ManagedStatus) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tAPPLIED\tDESIRED\tOK\tDRIFT\tERROR")
	for _, s := range sts {
		drift := strings.Join(s.Drift, ",")
		if drift == "" {
			drift = "-"
		}
		errText := s.Error
		if errText == "" {
			errText = "-"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%t\t%s\t%s\n", s.Node, s.Version, s.Desired, s.Applied, drift, errText)
		for _, c := range s.Conflicts {
			fmt.Fprintf(tw, "  conflict\t%s\t\t\t%s\t\n", c.Key, strings.Join(c.Fragments, ","))
		}
	}
	tw.Flush()
}

func printRuleStates(w io.Writer, states []core.RuleState) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tVALUE\tSINCE\tEXPR")
	for _, s := range states {
		value := "-"
		if s.HasValue {
			value = strconv.FormatFloat(s.Value, 'f', -1, 64)
		}
		since := "-"
		if s.Since > 0 {
			since = time.Unix(s.Since, 0).Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Name, ruleStateLabel(s), value, since, s.Expr)
	}
	tw.Flush()
}
