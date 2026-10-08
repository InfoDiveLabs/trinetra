// internal/trinetra/statuspage_cmd.go
package trinetra

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

const statusPageUsage = `usage:
  trinetra status-page service list
  trinetra status-page service add <id> --name NAME [--group G] [--desc D] [--order N] [--hold 3m] --target T [--target T ...]
  trinetra status-page service rm <id>
  trinetra status-page incident list [--all]
  trinetra status-page incident show <id>
  trinetra status-page incident open --title T --service ID [--service ID ...] --impact degraded|outage|maintenance --message M [--status investigating]
  trinetra status-page incident update <id> --status investigating|identified|monitoring|resolved --message M
  trinetra status-page incident resolve <id> [--message M]
targets: host | node:<id> | tag:<tag> | container:<name>[@<node>] | unit:<unit>[@<node>] | mount:<path>[@<node>]`

// statusPageWithClient runs f against the daemon's status page (test seam).
var statusPageWithClient = func(f func(core.StatusPageAPI) error) error {
	tok, _ := os.ReadFile(controlTokenPath())
	c, err := control.Dial(controlSocketPath(), strings.TrimSpace(string(tok)))
	if err != nil {
		return errDaemonDown
	}
	defer c.Close()
	return f(c.StatusPage())
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func parseStatusTarget(s string) (core.StatusTarget, error) {
	if s == core.TargetHost {
		return core.StatusTarget{Kind: core.TargetHost}, nil
	}
	kind, rest, ok := strings.Cut(s, ":")
	if !ok || rest == "" {
		return core.StatusTarget{}, fmt.Errorf("bad target %q", s)
	}
	if kind == core.TargetNode {
		return core.StatusTarget{Kind: core.TargetNode, Node: rest}, nil
	}
	val, node, _ := strings.Cut(rest, "@")
	t := core.StatusTarget{Kind: kind, Value: val, Node: node}
	switch kind {
	case core.TargetTag, core.TargetContainer, core.TargetUnit, core.TargetMount:
	default:
		return core.StatusTarget{}, fmt.Errorf("bad target kind %q", kind)
	}
	if val == "" {
		return core.StatusTarget{}, fmt.Errorf("bad target %q", s)
	}
	return t, nil
}

func cmdStatusPage(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, statusPageUsage)
		return 2
	}
	var err error
	switch args[0] + " " + args[1] {
	case "service list":
		err = statusPageServiceList()
	case "service add":
		return statusPageServiceAdd(args[2:])
	case "service rm":
		if len(args) != 3 {
			fmt.Fprintln(stderr, statusPageUsage)
			return 2
		}
		err = statusPageWithClient(func(sp core.StatusPageAPI) error { return sp.DeleteService(args[2], "cli") })
		if err == nil {
			fmt.Fprintf(stdout, "Removed service %s.\n", args[2])
		}
	case "incident list":
		all := len(args) == 3 && args[2] == "--all"
		err = statusPageWithClient(func(sp core.StatusPageAPI) error {
			incs, err := sp.Incidents(all)
			for _, inc := range incs {
				fmt.Fprintf(stdout, "%s  %-13s %-11s %s\n", inc.ID, inc.Status, inc.Impact, inc.Title)
			}
			return err
		})
	case "incident show":
		if len(args) != 3 {
			fmt.Fprintln(stderr, statusPageUsage)
			return 2
		}
		err = statusPageWithClient(func(sp core.StatusPageAPI) error {
			inc, err := sp.Incident(args[2])
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s  %s  [%s, %s]\n", inc.ID, inc.Title, inc.Status, inc.Impact)
			for _, u := range inc.Updates {
				fmt.Fprintf(stdout, "  %s  %-13s %s (%s)\n", time.Unix(u.TS, 0).Format(time.RFC3339), u.Status, u.Message, u.Author)
			}
			return nil
		})
	case "incident open":
		return statusPageIncidentOpen(args[2:])
	case "incident update", "incident resolve":
		return statusPageIncidentUpdate(args[1], args[2:])
	default:
		fmt.Fprintln(stderr, statusPageUsage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "status-page:", err)
		return 1
	}
	return 0
}

func statusPageServiceList() error {
	return statusPageWithClient(func(sp core.StatusPageAPI) error {
		svcs, err := sp.Services()
		if err != nil {
			return err
		}
		evals, _ := sp.Evaluation()
		state := map[string]core.ServiceEvaluation{}
		for _, e := range evals {
			state[e.ServiceID] = e
		}
		for _, s := range svcs {
			e := state[s.ID]
			fmt.Fprintf(stdout, "%-20s %-24s %-12s %s\n", s.ID, s.Name, e.State, e.Reason)
		}
		return nil
	})
}

func statusPageServiceAdd(args []string) int {
	fs := newFlags("status-page service add")
	name := fs.String("name", "", "public name")
	group := fs.String("group", "", "public group heading")
	desc := fs.String("desc", "", "public description")
	order := fs.Int("order", 0, "sort order")
	hold := fs.Duration("hold", time.Duration(core.DefaultHoldDownSec)*time.Second, "hold-down")
	var targets multiFlag
	fs.Var(&targets, "target", "target (repeatable)")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(stderr, statusPageUsage)
		return 2
	}
	svc := core.StatusService{ID: pos[0], Name: *name, Group: *group, Description: *desc, Order: *order, HoldDownSec: int(hold.Seconds())}
	for _, raw := range targets {
		t, err := parseStatusTarget(raw)
		if err != nil {
			fmt.Fprintln(stderr, "status-page:", err)
			return 2
		}
		svc.Targets = append(svc.Targets, t)
	}
	err = statusPageWithClient(func(sp core.StatusPageAPI) error {
		s, err := sp.SetService(svc, "cli")
		if err == nil {
			fmt.Fprintf(stdout, "Saved service %s (%s) with %d target(s).\n", s.ID, s.Name, len(s.Targets))
		}
		return err
	})
	if err != nil {
		fmt.Fprintln(stderr, "status-page:", err)
		return 1
	}
	return 0
}

func statusPageIncidentOpen(args []string) int {
	fs := newFlags("status-page incident open")
	title := fs.String("title", "", "incident title")
	impact := fs.String("impact", "degraded", "degraded|outage|maintenance")
	msg := fs.String("message", "", "first update")
	status := fs.String("status", core.IncidentInvestigating, "first update status")
	var svcs multiFlag
	fs.Var(&svcs, "service", "affected service id (repeatable)")
	pos, err := parseInterspersed(fs, args)
	if err != nil || rejectPositionals("status-page incident open", statusPageUsage, pos) {
		return 2
	}
	in := core.NewIncident{Title: *title, Services: svcs, Impact: core.ServiceState(*impact), Update: core.NewUpdate{Status: *status, Message: *msg}}
	err = statusPageWithClient(func(sp core.StatusPageAPI) error {
		inc, err := sp.CreateIncident(in, "cli")
		if err == nil {
			fmt.Fprintf(stdout, "Opened incident %s.\n", inc.ID)
		}
		return err
	})
	if err != nil {
		fmt.Fprintln(stderr, "status-page:", err)
		return 1
	}
	return 0
}

func statusPageIncidentUpdate(verb string, args []string) int {
	fs := newFlags("status-page incident " + verb)
	status := fs.String("status", "", "update status")
	msg := fs.String("message", "", "update message")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(stderr, statusPageUsage)
		return 2
	}
	u := core.NewUpdate{Status: *status, Message: *msg}
	if verb == "resolve" {
		u.Status = core.IncidentResolved
		if u.Message == "" {
			u.Message = "This incident has been resolved."
		}
	}
	err = statusPageWithClient(func(sp core.StatusPageAPI) error {
		inc, err := sp.PostUpdate(pos[0], u, "cli")
		if err == nil {
			fmt.Fprintf(stdout, "Incident %s is now %s.\n", inc.ID, inc.Status)
		}
		return err
	})
	if err != nil {
		fmt.Fprintln(stderr, "status-page:", err)
		return 1
	}
	return 0
}
