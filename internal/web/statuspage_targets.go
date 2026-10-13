package web

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// targetOption is one checkbox in a target picker.
type targetOption struct {
	Value, Label, State string
	Checked             bool
}

// targetNode is one server's section of the Services form's target checklist.
type targetNode struct {
	Name       string
	Whole      targetOption
	Containers []targetOption
	Units      []targetOption
	Err        string
	Picked     int
}

// statusPick is what the Monitoring page needs to offer "Add to status page".
type statusPick struct {
	Node     string
	Services []core.StatusService
	Groups   []string
}

func statusGroups(svcs []core.StatusService) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range svcs {
		if g := strings.TrimSpace(s.Group); g != "" && !seen[strings.ToLower(g)] {
			seen[strings.ToLower(g)] = true
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

func pickTargetValue(kind, value, node string) string {
	return formatStatusTarget(core.StatusTarget{Kind: kind, Value: value, Node: node})
}

// buildStatusPick returns nil unless the viewer is an admin and this daemon
// serves the status page, so Monitoring shows no checkboxes otherwise.
func buildStatusPick(r *http.Request, d Deps) *statusPick {
	if currentRole(r) != string(RoleAdmin) {
		return nil
	}
	sp := statusPageAPI(d)
	if sp == nil {
		return nil
	}
	svcs, err := sp.Services()
	if err != nil {
		return nil
	}
	p := &statusPick{Services: svcs, Groups: statusGroups(svcs)}
	if ns := nodeFrom(r); !ns.Self {
		p.Node = ns.ID
	}
	return p
}

type inventorySource struct {
	id, name string
	api      core.API
}

// statusTargetInventory lists every server's containers and units, each
// checked when its value is in picked. Fleet nodes are read in parallel.
func statusTargetInventory(r *http.Request, d Deps, picked map[string]bool) []targetNode {
	srcs := []inventorySource{{name: "This server", api: d.API}}
	if isMaster, nodes := resolveMasterAndNodes(r, d); isMaster && d.NodeAPI != nil {
		for _, n := range nodes {
			if n.ID == core.SelfNodeID {
				if n.Name != "" {
					srcs[0].name = n.Name + " (this server)"
				}
				continue
			}
			name := n.Name
			if name == "" {
				name = n.ID
			}
			srcs = append(srcs, inventorySource{id: n.ID, name: name, api: d.NodeAPI(n.ID)})
		}
	}
	out := make([]targetNode, len(srcs))
	var wg sync.WaitGroup
	for i, s := range srcs {
		wg.Add(1)
		go func(i int, s inventorySource) {
			defer wg.Done()
			out[i] = inventoryNode(s, picked)
		}(i, s)
	}
	wg.Wait()
	return out
}

func inventoryNode(s inventorySource, picked map[string]bool) targetNode {
	n := targetNode{Name: s.name}
	whole := core.StatusTarget{Kind: core.TargetHost}
	if s.id != "" {
		whole = core.StatusTarget{Kind: core.TargetNode, Node: s.id}
	}
	n.Whole = targetOption{Value: formatStatusTarget(whole), Label: "The whole server"}
	if s.api == nil {
		n.Err = "not reachable right now"
	} else if v, err := s.api.Monitoring(); err != nil {
		n.Err = "not reachable right now"
	} else {
		for _, c := range v.Containers {
			n.Containers = append(n.Containers, targetOption{Value: pickTargetValue(core.TargetContainer, c.Name, s.id), Label: c.Name, State: c.State})
		}
		for _, u := range v.Units {
			n.Units = append(n.Units, targetOption{Value: pickTargetValue(core.TargetUnit, u.Name, s.id), Label: u.Name, State: u.Active})
		}
	}
	mark := func(o *targetOption) {
		if picked[o.Value] {
			o.Checked = true
			n.Picked++
		}
	}
	mark(&n.Whole)
	for i := range n.Containers {
		mark(&n.Containers[i])
	}
	for i := range n.Units {
		mark(&n.Units[i])
	}
	return n
}

// splitPicked moves every target line the checklist shows into the checked
// set and returns the rest, which stay in the free-text field.
func splitPicked(nodes []targetNode, lines []string) (rest []string) {
	shown := map[string]bool{}
	for _, n := range nodes {
		shown[n.Whole.Value] = true
		for _, o := range n.Containers {
			shown[o.Value] = true
		}
		for _, o := range n.Units {
			shown[o.Value] = true
		}
	}
	for _, l := range lines {
		if !shown[l] {
			rest = append(rest, l)
		}
	}
	return rest
}

func targetLines(text string, picked []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range append(picked, strings.Split(text, "\n")...) {
		l = strings.TrimSpace(l)
		if l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

// mergeTargets appends add to have, skipping targets already present.
func mergeTargets(have, add []core.StatusTarget) (merged []core.StatusTarget, added int) {
	seen := map[string]bool{}
	merged = append(merged, have...)
	for _, t := range have {
		seen[formatStatusTarget(t)] = true
	}
	for _, t := range add {
		if k := formatStatusTarget(t); !seen[k] {
			seen[k] = true
			merged = append(merged, t)
			added++
		}
	}
	return merged, added
}

var slugJunk = regexp.MustCompile(`[^a-z0-9]+`)

// serviceSlug derives a unique service id from a display name.
func serviceSlug(name string, taken map[string]bool) string {
	base := strings.Trim(slugJunk.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(base) > 34 {
		base = strings.TrimRight(base[:34], "-")
	}
	if base == "" {
		base = "service"
	}
	id := base
	for i := 2; taken[id]; i++ {
		id = base + "-" + strconv.Itoa(i)
	}
	return id
}

func statusServiceAddTargetsHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sp := statusPageAPI(d)
		if sp == nil {
			http.NotFound(w, r)
			return
		}
		fail := func(msg string) {
			data := buildStatusServicesPage(r, d)
			data.Error = "Couldn't add to the status page: " + msg
			finishStatusServicesPage(r, d, &data)
			renderStatusTemplate(w, "statuspage_services.html", data, http.StatusBadRequest)
		}
		if err := r.ParseForm(); err != nil {
			fail("the form could not be read")
			return
		}
		targets, err := parseStatusTargetText(strings.Join(r.PostForm["target"], "\n"))
		if err != nil {
			fail(err.Error())
			return
		}
		if len(targets) == 0 {
			fail("nothing was selected")
			return
		}
		existing, err := sp.Services()
		if err != nil {
			fail(err.Error())
			return
		}
		var svc core.StatusService
		added := len(targets)
		switch r.FormValue("mode") {
		case "existing":
			id := r.FormValue("service")
			found := false
			for _, s := range existing {
				if s.ID == id {
					svc, found = s, true
				}
			}
			if !found {
				fail("pick a service to add to")
				return
			}
			svc.Targets, added = mergeTargets(svc.Targets, targets)
		case "new":
			name := strings.TrimSpace(r.FormValue("name"))
			if name == "" {
				fail("give the new service a name")
				return
			}
			taken := map[string]bool{}
			for _, s := range existing {
				taken[s.ID] = true
			}
			svc = core.StatusService{ID: serviceSlug(name, taken), Name: name, Group: strings.TrimSpace(r.FormValue("group")),
				Order: len(existing), HoldDownSec: core.DefaultHoldDownSec}
			svc.Targets, _ = mergeTargets(nil, targets)
			added = len(svc.Targets)
		default:
			fail("choose a new or an existing service")
			return
		}
		saved, err := sp.SetService(svc, auditUser(r))
		if err != nil {
			if errors.Is(err, core.ErrStatusPageOnChild) {
				err = errors.New(statusUnavailableText(err))
			}
			fail(err.Error())
			return
		}
		logAudit(d, r, "status_page.service.save", saved.ID, "", saved.Name)
		q := url.Values{"added": {strconv.Itoa(added)}, "to": {saved.ID}}
		http.Redirect(w, r, "/status-page/services?"+q.Encode(), http.StatusSeeOther)
	}
}
