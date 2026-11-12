package trinetra

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const pluginOfferTTL = 24 * time.Hour

type pluginOfferState struct {
	offer   fleet.PluginOffer
	path    string
	expires time.Time
}

type pluginPusher struct {
	lifecycle func() plugin.Lifecycle
	nodes     func() []fleet.Node
	push      func(nodeID string, f fleet.Frame) bool
	connected func(nodeID string) bool
	audit     *auditLog
	configFor func(id string) map[string]string
	now       func() time.Time

	mu      sync.Mutex
	offers  map[string]map[string]*pluginOfferState
	removes map[string]map[string]fleet.PluginRemove
	states  map[string]map[string]plugin.NodeState
	revoked []plugin.Revocation
}

func newPluginPusher(lifecycle func() plugin.Lifecycle, nodes func() []fleet.Node, push func(string, fleet.Frame) bool,
	connected func(string) bool, audit *auditLog, configFor func(string) map[string]string, now func() time.Time) *pluginPusher {
	if now == nil {
		now = time.Now
	}
	return &pluginPusher{lifecycle: lifecycle, nodes: nodes, push: push, connected: connected, audit: audit, configFor: configFor, now: now,
		offers: map[string]map[string]*pluginOfferState{}, removes: map[string]map[string]fleet.PluginRemove{},
		states: map[string]map[string]plugin.NodeState{}}
}

func (p *pluginPusher) resolveTargets(req plugin.PushRequest) ([]fleet.Node, error) {
	all := p.nodes()
	seen := map[string]bool{}
	var out []fleet.Node
	add := func(n fleet.Node) {
		if !seen[n.ID] {
			seen[n.ID] = true
			out = append(out, n)
		}
	}
	for _, want := range req.Nodes {
		if want == core.SelfNodeID {
			continue
		}
		found := false
		for _, n := range all {
			if n.ID == want || strings.EqualFold(n.Name, want) {
				if n.Revoked {
					return nil, fmt.Errorf("node %s is revoked", want)
				}
				add(n)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown node %q", want)
		}
	}
	if req.Tag != "" {
		for _, n := range all {
			if !n.Revoked && slices.Contains(n.Tags, req.Tag) {
				add(n)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no target nodes: give --nodes or a --tag that matches at least one node")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func samePermissions(a, b []plugin.Permission) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// Push offers (or removes) a plugin on the target nodes.
func (p *pluginPusher) Push(req plugin.PushRequest) (plugin.PushResult, error) {
	if !plugin.IDPattern.MatchString(req.ID) {
		return plugin.PushResult{}, fmt.Errorf("invalid plugin id %q", req.ID)
	}
	targets, err := p.resolveTargets(req)
	if err != nil {
		return plugin.PushResult{}, err
	}
	if req.Remove {
		return p.pushRemove(req, targets)
	}
	lc := p.lifecycle()
	if lc == nil {
		return plugin.PushResult{}, plugin.ErrUnavailable
	}
	st, err := lc.Get(req.ID)
	if err != nil {
		return plugin.PushResult{}, err
	}
	switch st.Trust {
	case plugin.TrustVerified:
	case plugin.TrustUnverified:
		if !req.AcknowledgedRisk {
			return plugin.PushResult{}, errors.New("pushing an unverified plugin requires acknowledging the risk")
		}
		if !samePermissions(req.Accepted, st.Manifest.Permissions) {
			return plugin.PushResult{}, errors.New("pushing an unverified plugin requires accepting every permission it declares")
		}
	default:
		return plugin.PushResult{}, fmt.Errorf("%s plugins cannot be pushed", st.Trust)
	}
	pkg, err := lc.PackageFile(req.ID)
	if err != nil {
		return plugin.PushResult{}, err
	}
	if pkg.Size <= 0 || pkg.Size > fleet.MaxPluginPackage {
		return plugin.PushResult{}, fmt.Errorf("package size %d is outside the pushable range", pkg.Size)
	}
	names := make([]string, len(targets))
	for i, n := range targets {
		names[i] = n.Name
	}
	now := p.now()
	offer := fleet.PluginOffer{
		ID: req.ID, Version: st.Version, SHA256: pkg.SHA256, Size: pkg.Size, Trust: st.Trust,
		Consent: plugin.ConsentRecord{Time: now.Unix(), Actor: req.Actor, Via: req.Via, Version: st.Version,
			Permissions: st.Manifest.Permissions, Unverified: st.Trust == plugin.TrustUnverified, Nodes: names},
	}
	if st.Trust == plugin.TrustUnverified {
		offer.Consent.Permissions = req.Accepted
	}
	res := plugin.PushResult{}
	p.mu.Lock()
	for _, n := range targets {
		if p.offers[n.ID] == nil {
			p.offers[n.ID] = map[string]*pluginOfferState{}
		}
		for sha, o := range p.offers[n.ID] {
			if o.offer.ID == req.ID {
				delete(p.offers[n.ID], sha)
			}
		}
		delete(p.removes[n.ID], req.ID)
		p.offers[n.ID][pkg.SHA256] = &pluginOfferState{offer: offer, path: pkg.Path, expires: now.Add(pluginOfferTTL)}
		ns := plugin.NodeState{Node: n.ID, NodeName: n.Name, Version: st.Version, Phase: phaseOffered, Size: pkg.Size}
		p.setStateLocked(req.ID, ns)
		res.Targets = append(res.Targets, ns)
	}
	p.mu.Unlock()
	for _, n := range targets {
		p.sendOffer(n.ID, offer)
	}
	_ = p.audit.Append(req.Actor, "fleet.plugin.push", req.ID, fmt.Sprintf("version %s to %s via %s", st.Version, strings.Join(names, ","), req.Via), now.Unix())
	return res, nil
}

func (p *pluginPusher) pushRemove(req plugin.PushRequest, targets []fleet.Node) (plugin.PushResult, error) {
	rm := fleet.PluginRemove{ID: req.ID, Reason: "removed by " + req.Actor}
	names := make([]string, len(targets))
	res := plugin.PushResult{}
	p.mu.Lock()
	for i, n := range targets {
		names[i] = n.Name
		for sha, o := range p.offers[n.ID] {
			if o.offer.ID == req.ID {
				delete(p.offers[n.ID], sha)
			}
		}
		if p.removes[n.ID] == nil {
			p.removes[n.ID] = map[string]fleet.PluginRemove{}
		}
		p.removes[n.ID][req.ID] = rm
		ns := plugin.NodeState{Node: n.ID, NodeName: n.Name, Phase: phaseOffered}
		p.setStateLocked(req.ID, ns)
		res.Targets = append(res.Targets, ns)
	}
	p.mu.Unlock()
	for _, n := range targets {
		p.sendRemove(n.ID, rm)
	}
	_ = p.audit.Append(req.Actor, "fleet.plugin.remove", req.ID, fmt.Sprintf("from %s via %s", strings.Join(names, ","), req.Via), p.now().Unix())
	return res, nil
}

func (p *pluginPusher) setStateLocked(id string, ns plugin.NodeState) {
	if p.states[id] == nil {
		p.states[id] = map[string]plugin.NodeState{}
	}
	p.states[id][ns.Node] = ns
}

func (p *pluginPusher) sendOffer(nodeID string, o fleet.PluginOffer) {
	if p.configFor != nil {
		o.Config = p.configFor(o.ID)
	}
	b, err := json.Marshal(o)
	if err != nil {
		return
	}
	p.push(nodeID, fleet.Frame{Type: fleet.FramePluginOffer, Data: b})
}

func (p *pluginPusher) sendRemove(nodeID string, r fleet.PluginRemove) {
	if b, err := json.Marshal(r); err == nil {
		p.push(nodeID, fleet.Frame{Type: fleet.FramePluginRemove, Data: b})
	}
}

// NodeStates reports the last known push state of plugin id on every node it was pushed to.
func (p *pluginPusher) NodeStates(id string) ([]plugin.NodeState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]plugin.NodeState, 0, len(p.states[id]))
	for _, s := range p.states[id] {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeName < out[j].NodeName })
	return out, nil
}

// Open serves the offered package to nodeID; it fails unless that node has an unexpired offer for sha.
func (p *pluginPusher) Open(nodeID, sha string) (io.ReadSeekCloser, int64, error) {
	p.mu.Lock()
	o := p.offers[nodeID][sha]
	if o != nil && !p.now().Before(o.expires) {
		delete(p.offers[nodeID], sha)
		o = nil
	}
	p.mu.Unlock()
	if o == nil {
		return nil, 0, errors.New("no outstanding offer")
	}
	f, err := os.Open(o.path)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil || fi.Size() != o.offer.Size {
		f.Close()
		return nil, 0, errors.New("offered package changed on disk")
	}
	return f, fi.Size(), nil
}

// OnConnect re-sends offers and removals the node has not completed, and the known revocations.
func (p *pluginPusher) OnConnect(nodeID string) {
	now := p.now()
	p.mu.Lock()
	var offers []fleet.PluginOffer
	for sha, o := range p.offers[nodeID] {
		if !now.Before(o.expires) {
			delete(p.offers[nodeID], sha)
			continue
		}
		if ph := p.states[o.offer.ID][nodeID].Phase; ph == "" || ph == phaseOffered || ph == phaseDownloading {
			offers = append(offers, o.offer)
		}
	}
	var removes []fleet.PluginRemove
	for id, r := range p.removes[nodeID] {
		if p.states[id][nodeID].Phase != phaseRemoved {
			removes = append(removes, r)
		}
	}
	revoked := slices.Clone(p.revoked)
	p.mu.Unlock()
	for _, o := range offers {
		p.sendOffer(nodeID, o)
	}
	for _, r := range removes {
		p.sendRemove(nodeID, r)
	}
	if len(revoked) > 0 {
		if b, err := json.Marshal(fleet.PluginRevoke{Revocations: revoked}); err == nil {
			p.push(nodeID, fleet.Frame{Type: fleet.FramePluginRevoke, Data: b})
		}
	}
}

// OnReport folds a child's plugin reports into the per-node push states.
func (p *pluginPusher) OnReport(nodeID string, reports []fleet.PluginReport) {
	if len(reports) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range reports {
		prev, ok := p.states[r.ID][nodeID]
		if !ok {
			continue
		}
		prev.Version, prev.State, prev.Phase = r.Version, r.State, r.Phase
		prev.Received, prev.Size, prev.Error = r.Received, r.Size, r.Error
		p.setStateLocked(r.ID, prev)
	}
}

// Revoke records revocations and sends them to every connected node.
func (p *pluginPusher) Revoke(revs []plugin.Revocation) {
	if len(revs) == 0 {
		return
	}
	p.mu.Lock()
	p.revoked = append(p.revoked, revs...)
	p.mu.Unlock()
	b, err := json.Marshal(fleet.PluginRevoke{Revocations: revs})
	if err != nil {
		return
	}
	for _, n := range p.nodes() {
		if p.connected(n.ID) {
			p.push(n.ID, fleet.Frame{Type: fleet.FramePluginRevoke, Data: b})
		}
	}
}

// masterPluginHooks lets startMaster install fleet hooks before the plugin subsystem is wired; each is a no-op until set.
type masterPluginHooks struct{ p atomic.Pointer[pluginPusher] }

var masterPlugins masterPluginHooks

// Open implements fleet.PluginPackageSource.
func (h *masterPluginHooks) Open(nodeID, sha string) (io.ReadSeekCloser, int64, error) {
	p := h.p.Load()
	if p == nil {
		return nil, 0, errors.New("plugin push is not available")
	}
	return p.Open(nodeID, sha)
}

func (h *masterPluginHooks) onConnect(nodeID string) {
	if p := h.p.Load(); p != nil {
		p.OnConnect(nodeID)
	}
}

func (h *masterPluginHooks) onReport(nodeID string, reports []fleet.PluginReport) {
	if p := h.p.Load(); p != nil {
		p.OnReport(nodeID, reports)
	}
}

// configStrings converts stored config values to the string form used by SetConfig: arrays are newline-joined.
func configStrings(values map[string]json.RawMessage) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for k, raw := range values {
		var s string
		var arr []string
		switch {
		case json.Unmarshal(raw, &s) == nil:
			out[k] = s
		case json.Unmarshal(raw, &arr) == nil:
			out[k] = strings.Join(arr, "\n")
		default:
			out[k] = string(raw)
		}
	}
	return out
}
