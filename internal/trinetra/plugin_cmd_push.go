package trinetra

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const pluginPushUsage = "usage: trinetra plugin push <id> (--nodes a,b | --tag T) [--remove] [--accept=perm,perm --i-understand-the-risk]"

var pushStdin io.Reader = os.Stdin

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

type pushPrompter struct{ r *bufio.Reader }

func (p pushPrompter) yes(q string) bool {
	fmt.Fprintf(stdout, "%s [y/N] ", q)
	line, _ := p.r.ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func cmdPluginPush(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, pluginPushUsage)
		return 2
	}
	id := args[0]
	fs := flag.NewFlagSet("plugin push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	nodes := fs.String("nodes", "", "comma-separated node names or ids")
	tag := fs.String("tag", "", "push to every node carrying this tag")
	remove := fs.Bool("remove", false, "remove the plugin from the targets instead")
	accept := fs.String("accept", "", "comma-separated permissions to accept (unverified plugins)")
	risk := fs.Bool("i-understand-the-risk", false, "acknowledge the unverified-plugin risk without prompting")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 || (*nodes == "" && *tag == "") {
		fmt.Fprintln(stderr, pluginPushUsage)
		return 2
	}
	req := plugin.PushRequest{ID: id, Nodes: splitList(*nodes), Tag: *tag, Actor: cliActor(), Via: "cli", Remove: *remove}
	targets := describePushTargets(req)
	in := pushPrompter{bufio.NewReader(pushStdin)}

	var result plugin.PushResult
	err := pluginWithClient(func(m plugin.Manager) error {
		if req.Remove {
			if !in.yes(fmt.Sprintf("Remove plugin %s from %s?", id, targets)) {
				return errPushDeclined
			}
		} else {
			st, err := m.Get(id)
			if err != nil {
				return err
			}
			if err := confirmPush(in, st, targets, *accept, *risk, &req); err != nil {
				return err
			}
		}
		var perr error
		result, perr = m.Push(req)
		return perr
	})
	switch {
	case err == errPushDeclined:
		fmt.Fprintln(stderr, "Cancelled; nothing was pushed.")
		return 1
	case err != nil:
		fmt.Fprintln(stderr, "plugin push:", err)
		return 1
	}
	printPushResult(result)
	return 0
}

var errPushDeclined = fmt.Errorf("declined")

func describePushTargets(req plugin.PushRequest) string {
	var parts []string
	if len(req.Nodes) > 0 {
		parts = append(parts, strings.Join(req.Nodes, ", "))
	}
	if req.Tag != "" {
		parts = append(parts, "every node tagged "+req.Tag)
	}
	return strings.Join(parts, " and ")
}

func confirmPush(in pushPrompter, st plugin.Status, targets, accept string, risk bool, req *plugin.PushRequest) error {
	perms := st.Manifest.Permissions
	if st.Trust != plugin.TrustUnverified {
		fmt.Fprintf(stdout, "Push %s %s to %s.\nIt will run with these permissions on each node:\n", st.Manifest.Name, st.Version, targets)
		for _, p := range perms {
			fmt.Fprintf(stdout, "  - %s: %s\n", p, plugin.Describe(p))
		}
		if !in.yes("Push this plugin?") {
			return errPushDeclined
		}
		return nil
	}
	fmt.Fprintln(stdout, plugin.RiskWarning(st.Manifest.Name, st.Version, st.Manifest.Publisher))
	fmt.Fprintf(stdout, "\nIt will be installed and started on: %s\n\n", targets)
	acceptSet := map[plugin.Permission]bool{}
	for _, p := range splitList(accept) {
		acceptSet[plugin.Permission(p)] = true
	}
	if !risk && !in.yes("I understand this plugin is unverified and want to push it") {
		return errPushDeclined
	}
	req.AcknowledgedRisk = true
	for _, p := range perms {
		if acceptSet[p] {
			req.Accepted = append(req.Accepted, p)
			continue
		}
		if !in.yes(fmt.Sprintf("Allow %s (%s) on every target node?", p, plugin.Describe(p))) {
			return errPushDeclined
		}
		req.Accepted = append(req.Accepted, p)
	}
	return nil
}

func printPushResult(res plugin.PushResult) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tPHASE\tVERSION\tERROR")
	for _, t := range res.Targets {
		name := t.NodeName
		if name == "" {
			name = t.Node
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, t.Phase, t.Version, t.Error)
	}
	tw.Flush()
}
