// Package trinetra: fleet_daemon.go is the single place the daemon's
// fleet role is honoured. startFleet does nothing at all for solo (no
// directories, no listener, no goroutines); for a master it serves the fleet
// port, tracks liveness and raises node-down alerts; for a child it starts
// the shipper and tees local writes into the outbox.
package trinetra

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

type fleetDeps struct {
	stateDir       string
	getCfg         func() *config.Config
	self           core.API
	latestSnapshot func() Snapshot
	store          SampleStore
	alog           *AlertLog
	alertStatePath string
	// alert delivers an alert the ordinary asynchronous way (enqueueAndLog):
	// logged/published immediately, actual dispatch happens off the sampler
	// goroutine on the NotifierQueue worker. Used by a child's own
	// link-status alerts (childLinkAlerts, below) and, as a fallback, by
	// masterLoop when no alerting engine is present (nil engine, tests
	// only in practice) -- never blocks its caller.
	alert func(Alert)
	// deliverSync is master-only: it delivers an alert SYNCHRONOUSLY,
	// directly against the master's own Dispatcher (bypassing the async
	// NotifierQueue), and reports whether at least one channel accepted it
	// (see deliverSyncAndLog and fleetAlertEngine.deliver, fleet_engine.go)
	// -- the fleet alerting engine needs that completion signal before it
	// may push a receipt down to a node. Only ever called from the
	// engine's own per-alert dispatch goroutine, never from a hot path
	// that must not block.
	deliverSync func(Alert) bool
	// deliverSyncTo is deliverSync narrowed to a specific channel-name subset
	// (fleet routing, task 5): same synchronous, completion-reporting
	// contract, used for the fire/recover legs once a routing config is
	// wired (fleetAlertEngine.SetRouting's deliverNamed) -- it also logs to
	// the alert log/live bus, exactly like deliverSync.
	deliverSyncTo func(a Alert, channels []string) bool
	// dispatchOnly delivers to a channel-name subset WITHOUT logging to the
	// alert log/live bus (fleet routing, task 5): used for escalation/repeat
	// notifications (fleetAlertEngine.SetRouting's dispatchOnly), which are
	// not new alert records -- only the fire/recover legs are.
	dispatchOnly func(a Alert, channels []string) bool
	// alertFallback delivers a alert locally after the child's lease/receipt
	// handoff (fleet_lease.go) gave up waiting on the master: it mirrors
	// alert's construction (same alog/bus/q closed over) but calls
	// deliverFallback instead of enqueueAndLog, since that delivery must be
	// unconditional (see deliverFallback's doc comment). Only startChild
	// ever calls it; solo and master never construct a handoff to call it
	// from. silences is startChild's own pushedSilences (task 4): passed
	// through opaquely here since daemon.go builds this closure once, before
	// any child-specific state exists, and forwarded to deliverFallback so a
	// fallback delivery still covered by a pushed silence is suppressed.
	alertFallback func(a Alert, silences *pushedSilences)
	logf          func(string, ...any)
}

type fleetRuntime struct {
	provider *fleetProvider
	tee      *outboxTee
	stop     func()
}

func fleetMasterDir(stateDir string) string { return filepath.Join(stateDir, "fleet") }
func fleetPKIDir(stateDir string) string    { return filepath.Join(stateDir, "fleet", "pki") }
func fleetChildDir(stateDir string) string  { return filepath.Join(stateDir, "fleet-child") }
func fleetOutboxDir(stateDir string) string { return filepath.Join(stateDir, "outbox") }

// fleetInitPKI creates (or reuses) the master CA and issues a fresh server
// leaf for hosts. server.crt holds the leaf followed by the CA so children
// can pin the CA from the presented chain.
func fleetInitPKI(stateDir string, hosts []string, caName string, now time.Time) error {
	dir := fleetPKIDir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	caCrt, caKey := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	crtMissing, err := fileMissing(caCrt)
	if err != nil {
		return err
	}
	keyMissing, err := fileMissing(caKey)
	if err != nil {
		return err
	}
	var ca *fleet.CA
	switch {
	case crtMissing && keyMissing:
		// First init: the only case a new CA (and so a new pin) is minted.
		if ca, err = fleet.NewCA(caName, now); err != nil {
			return err
		}
		if err := ca.Save(caCrt, caKey); err != nil {
			return err
		}
	case crtMissing || keyMissing:
		return fmt.Errorf("fleet CA at %s is unreadable or incomplete; refusing to replace it — fix or remove both files", dir)
	default:
		// Replacing an existing CA would silently rotate the pin and orphan
		// every enrolled child, so a load failure is an error, never a
		// reason to mint a new one.
		if ca, err = fleet.LoadCA(caCrt, caKey); err != nil {
			return fmt.Errorf("fleet CA at %s is unreadable or incomplete; refusing to replace it — fix or remove both files: %w", dir, err)
		}
	}
	leaf, key, err := ca.IssueServer(hosts, now)
	if err != nil {
		return err
	}
	if err := writeFileSynced(filepath.Join(dir, "server.key"), key); err != nil {
		return err
	}
	chain := append(append([]byte{}, leaf...), ca.CertPEM...)
	return os.WriteFile(filepath.Join(dir, "server.crt"), chain, 0o644)
}

// serverLeafWarnBefore is how long before the master's server certificate
// expires that startMaster starts warning. Nothing renews it automatically.
const serverLeafWarnBefore = 90 * 24 * time.Hour

// serverLeafExpiryWarning returns a warning when the master's server
// certificate expires within serverLeafWarnBefore of now, else "".
func serverLeafExpiryWarning(leaf tls.Certificate, now time.Time) string {
	if len(leaf.Certificate) == 0 {
		return ""
	}
	c, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		return ""
	}
	left := c.NotAfter.Sub(now)
	if left > serverLeafWarnBefore {
		return ""
	}
	return fmt.Sprintf("fleet: WARNING the master's server certificate expires %s (in %d days); children cannot connect after that. "+
		"Re-issue it with `sudo trinetra fleet disable` then `sudo trinetra fleet init --address ...` (keeps the CA, so children need not re-join) and restart.",
		c.NotAfter.Format("2006-01-02"), int(left.Hours()/24))
}

// fileMissing reports whether path does not exist; any other stat error is
// returned.
func fileMissing(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	return false, err
}

// fleetJoinURL is the URL children dial: the first fleet.address with the
// listener's port. It is "" when fleet.address is empty (no dialable host).
func fleetJoinURL(cfg *config.Config) string {
	host := strings.TrimSpace(strings.Split(cfg.Fleet.Address, ",")[0])
	if host == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(cfg.FleetListen())
	if err != nil || port == "" || port == "0" {
		port = "9443"
	}
	return "https://" + net.JoinHostPort(host, port)
}

func startFleet(ctx context.Context, cfg *config.Config, d fleetDeps) *fleetRuntime {
	rt := &fleetRuntime{
		provider: &fleetProvider{self: d.self, role: config.RoleSolo, selfName: func() string { return d.getCfg().ServerName() }},
		stop:     func() {},
	}
	switch cfg.FleetRole() {
	case config.RoleMaster:
		if err := startMaster(ctx, cfg, d, rt); err != nil {
			d.logf("fleet: master failed to start, running as solo: %v", err)
		}
	case config.RoleChild:
		if err := startChild(ctx, cfg, d, rt); err != nil {
			d.logf("fleet: child link failed to start, running as solo: %v", err)
		}
	}
	// stop is called both by the daemon's SIGTERM handler and its deferred
	// cleanup; only the first call does anything.
	var once sync.Once
	stop := rt.stop
	rt.stop = func() { once.Do(stop) }
	return rt
}

// waitBounded waits for done until deadline; it reports whether done closed.
func waitBounded(done <-chan struct{}, deadline time.Time) bool {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// masterLoop is the master's periodic work: pick up nodes the tracker does
// not know yet, evaluate liveness and raise alerts, flush the registry and
// run a slice of replica maintenance. tick is called from one goroutine only.
type masterLoop struct {
	reg     *fleet.Registry
	tracker *fleet.Tracker
	sink    *replicaSink
	// engine is the master alerting engine (fleet_engine.go): every alert
	// this loop raises (node-down/connectivity) goes through
	// engine.Submit(alertSource{}, ...) exactly like a child-shipped one, so
	// incidents/dedup cover both producers uniformly. It also owns the lease
	// push cadence (TickLeases, called from tick below).
	engine *fleetAlertEngine
	// mu serializes tick's liveness/alert pass with remove, so a node
	// removed mid-tick can never be re-tracked and paged. Guards alerter.
	mu          sync.Mutex
	alerter     *fleet.NodeAlerter
	alert       func(Alert)
	getCfg      func() *config.Config
	logf        func(string, ...any)
	lastFlush   time.Time
	maint       maintScheduler // only touched by the maintenance goroutine
	maintaining chan struct{}  // holds a token while a maintenance slice runs
	// lastDropCheck is when checkDrops last ran. Only tick touches it; the
	// per-node baselines live in each replica's ingest.state.
	lastDropCheck time.Time
	// started is when this masterLoop was built (master start, or the most
	// recent restart). nodeDownAfter is the blind window (spec: every node
	// is seeded as "seen at master start", so an outage that predates a
	// restart is never mistaken for a fresh one) -- captured once, matching
	// how the tracker itself was seeded, rather than re-read live. Together
	// they gate orphanChecked (B3 review round 2 1(b)): the FIRST tick once
	// now >= started+nodeDownAfter runs checkOrphanedIncidents exactly once.
	started       time.Time
	nodeDownAfter time.Duration
	orphanChecked bool
}

// masterTickInterval is how often the master loop ticks.
const masterTickInterval = 5 * time.Second

// newMasterLoop builds a masterLoop. A nil engine (test convenience for
// suites that don't exercise alerting) makes l.alert fall back to d.alert
// directly, exactly as before the alerting engine existed.
func newMasterLoop(reg *fleet.Registry, tracker *fleet.Tracker, sink *replicaSink, engine *fleetAlertEngine, d fleetDeps, now time.Time) *masterLoop {
	alert := d.alert
	if engine != nil {
		alert = func(a Alert) { engine.Submit(alertSource{}, a) }
	}
	nodeDownAfter := 2 * time.Minute // config.Default()'s own fallback, mirrored for a fleetDeps with no getCfg (tests)
	if d.getCfg != nil {
		if c := d.getCfg(); c != nil {
			nodeDownAfter = c.FleetNodeDownAfter()
		}
	}
	return &masterLoop{reg: reg, tracker: tracker, sink: sink, engine: engine, alerter: fleet.NewNodeAlerter(), alert: alert,
		getCfg: d.getCfg, logf: d.logf, lastFlush: now, maintaining: make(chan struct{}, 1), lastDropCheck: now,
		started: now, nodeDownAfter: nodeDownAfter}
}

// checkDrops logs a warning for every node whose replica dropped points out
// of order or over the series limit since the previous check. The counters
// themselves are cumulative (fleet status shows them); this is what makes a
// new problem visible without warning about an old one forever. The
// baseline it compares against is persisted per node (ingest.state), so
// growth a master restart interrupts is still warned about after it.
func (l *masterLoop) checkDrops(now time.Time) {
	since := now.Sub(l.lastDropCheck).Round(time.Minute)
	l.lastDropCheck = now
	for _, n := range l.reg.List() {
		st := l.sink.Stats(n.ID)
		dOld, dCard := st.DroppedOutOfOrder-st.WarnedOutOfOrder, st.DroppedCardinality-st.WarnedCardinality
		if err := l.sink.MarkDropsChecked(n.ID, st.DroppedOutOfOrder, st.DroppedCardinality); err != nil {
			l.logf("fleet: save drop-warning baseline for %s: %v", n.ID, err)
		}
		if dOld <= 0 && dCard <= 0 {
			continue
		}
		l.logf("fleet: WARNING node %s (%s): its replica dropped %d points out of order and %d over the %d-series limit in the last %s. Out-of-order drops usually mean the node's clock jumped back (see the SKEW column in fleet nodes).",
			n.Name, n.ID, max(dOld, 0), max(dCard, 0), maxReplicaSeries, since)
	}
}

// trackUnseen registers every registry node the tracker has never heard of
// (joined after the master started and never made contact) as last seen at
// its join time, so a node that enrolls and then goes silent still alerts.
func trackUnseen(reg *fleet.Registry, tracker *fleet.Tracker, now int64) {
	for _, n := range reg.List() {
		if tracker.State(n.ID) != "" {
			continue
		}
		seen := n.Joined
		if seen <= 0 {
			seen = now
		}
		tracker.Seen(n.ID, seen, -1)
		if n.Revoked {
			tracker.SetRevoked(n.ID, true)
		}
	}
}

func (l *masterLoop) tick(now time.Time) {
	l.mu.Lock()
	trackUnseen(l.reg, l.tracker, now.Unix())
	name := func(id string) string {
		if n, ok := l.reg.Get(id); ok {
			return n.Name
		}
		return id
	}
	ev := l.tracker.Evaluate(now.Unix())
	intents := l.alerter.Plan(ev, now.Unix(), name)
	var nodeIDs []string
	if l.engine != nil {
		for _, n := range l.reg.List() {
			if !n.Revoked {
				nodeIDs = append(nodeIDs, n.ID)
			}
		}
	}
	// checkOrphanedIncidents runs exactly once: the first tick at or after
	// the blind window (node_down_after since this masterLoop started) has
	// elapsed, so the tracker has had a real chance to observe every node's
	// TRUE state before anything is judged orphaned (B3 review round 2 1(b)).
	runOrphanCheck := !l.orphanChecked && now.Sub(l.started) >= l.nodeDownAfter
	if runOrphanCheck {
		l.orphanChecked = true
	}
	l.mu.Unlock()
	for _, in := range intents {
		l.alert(fleetAlert(in, now.Unix()))
	}
	if runOrphanCheck {
		l.checkOrphanedIncidents(now, ev)
	}
	// Lease cadence (global-constraints/task-3 ruling): push lease{until:
	// now+90s} to every connected, non-revoked node at least every 30s.
	// Revoked/removed nodes are simply absent from nodeIDs. A freshly
	// connected node also gets one immediately via Hub.OnConnect
	// (engine.PushLeaseNow), so this loop need not special-case "just
	// joined".
	if l.engine != nil {
		l.engine.TickLeases(now, nodeIDs)
		l.engine.TickSilences(now, nodeIDs)
		l.engine.TickEscalations(now)
		l.engine.TickGrouping(now)
		// TickRules (task 7) self-gates to ruleTickInterval (30s); called
		// every 5s tick exactly like the others above, cheap no-op otherwise.
		l.engine.TickRules(now)
	}
	if now.Sub(l.lastFlush) >= 30*time.Second {
		l.lastFlush = now
		if err := l.reg.FlushIfDirty(); err != nil {
			l.logf("fleet: registry flush: %v", err)
		}
	}
	if now.Sub(l.lastDropCheck) >= storeMaintenanceInterval {
		l.checkDrops(now)
	}
	// Replica maintenance rewrites and fsyncs series files, so it runs on
	// the local store's cadence (storeMaintenanceInterval), a slice of the
	// nodes per tick, off this goroutine. l.maint is only touched inside the
	// maintenance goroutine; the token channel orders successive passes.
	select {
	case l.maintaining <- struct{}{}:
		go func() {
			defer func() { <-l.maintaining }()
			for _, id := range l.maint.next(l.sink.nodeIDs(), masterTickInterval, storeMaintenanceInterval) {
				l.sink.maintainNode(id, time.Now().Unix())
			}
		}()
	default: // previous slice still running
	}
}

// stillActive reports whether the condition key names is still true, so
// checkOrphanedIncidents knows whether an open master-own incident for it
// should be recovered. It is deliberately small and generic (a plain
// switch on the key's shape) so a later task's rule alerts (B7) can extend
// it with their own keys without touching the reconciliation logic itself;
// an unrecognized key defaults to "still active" (leave it alone) rather
// than guessing it should be auto-resolved.
func (l *masterLoop) stillActive(key string, ev fleet.Evaluation) bool {
	switch {
	case key == "fleet:connectivity":
		return len(ev.MassDown) > 0
	case strings.HasPrefix(key, "fleet:node:") && strings.HasSuffix(key, ":down"):
		id := strings.TrimSuffix(strings.TrimPrefix(key, "fleet:node:"), ":down")
		n, ok := l.reg.Get(id)
		if !ok || n.Revoked {
			return false // removed or revoked: never still "down" in a way worth paging
		}
		return l.tracker.State(id) == fleet.StateDown
	case strings.HasPrefix(key, "fleet:rule:"):
		// task 7: false if the rule no longer exists in the live config, or
		// its current in-memory state (reset by this very restart) is not
		// firing -- see fleetAlertEngine.ruleStillFiring.
		if l.engine == nil {
			return false
		}
		return l.engine.ruleStillFiring(strings.TrimPrefix(key, "fleet:rule:"))
	default:
		return true
	}
}

// checkOrphanedIncidents recovers any open (firing or acked) master-own
// incident whose condition is no longer active (B3 review round 2 1(b)): a
// restart clears the in-memory NodeAlerter/tracker state that would
// normally notice and emit the matching recover itself, so without this an
// incident whose node came back online (or was removed/revoked) while the
// master was down, or a mass-connectivity event that has since ended, would
// stay stuck "firing" forever. Runs once, from tick, after the blind window.
func (l *masterLoop) checkOrphanedIncidents(now time.Time, ev fleet.Evaluation) {
	if l.engine == nil || l.engine.incidents == nil {
		return
	}
	for _, inc := range l.engine.incidents.List(core.IncidentFilter{}, nil) {
		if !isOpenState(inc.State) || len(inc.Alerts) == 0 {
			continue // not open, or nothing recorded on it
		}
		// (task 6 part 2) A grouped or dependency-folded incident can hold
		// several independent master-own members (e.g. two down nodes
		// sharing one incident, or a dependency fold): each is reconciled on
		// its own condition, not just the incident's last-appended alert. A
		// dependency-folded member (Suppressed != "") is left alone here --
		// it recovers/releases through the dependency machinery itself
		// (Submit/releaseFoldedDependents), not this reconciliation pass.
		for _, al := range inc.Alerts {
			if al.Node != "" || al.ResolvedAt != 0 || al.Suppressed != "" {
				continue
			}
			if l.stillActive(al.Key, ev) {
				continue
			}
			l.alert(fleetAlert(fleet.AlertIntent{Key: al.Key, Title: orphanRecoverTitle(al.Title), Recover: true}, now.Unix()))
		}
	}
}

// orphanRecoverTitle builds a human-readable recover title for an orphaned
// incident being reconciled after a restart, reusing the incident's own
// recorded title (its last known fire, which already carries its own 🔴)
// rather than re-deriving one from the key.
func orphanRecoverTitle(title string) string {
	return title + " -- recovered (reconciled after restart)"
}

// observeSkew is the master's OnSkew hook: it adds the sample to the node's
// filtered skew (see replicaSink.RecordSkew), hands that to the tracker
// (|skew| > 30 s is lagging), and warns once each time a node's clock is
// confirmed past 30 s. The master
// stores the child's timestamps unchanged, so a clock running ahead pins the
// replica's ordering guard to the future and later points are dropped as
// out of order; the warning and the fleet nodes SKEW column make that
// visible.
func (l *masterLoop) observeSkew(id string, now time.Time, sentAt int64) {
	skew, crossed := l.sink.RecordSkew(id, now.Unix()-sentAt)
	l.tracker.SetSkew(id, skew)
	if crossed {
		name := id
		if n, ok := l.reg.Get(id); ok {
			name = n.Name
		}
		dir := "behind"
		if skew < 0 {
			dir = "ahead of"
		}
		l.logf("fleet: WARNING node %s (%s) clock is %ds %s the master's; its data is stored with its own timestamps and may be dropped as out of order. Fix NTP on that host.", name, id, abs64(skew), dir)
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// remove deletes node id from the registry and liveness tracking and
// resolves any open alert for it. Its replica directory is left on disk.
func (l *masterLoop) remove(id string, now time.Time) error {
	l.mu.Lock()
	n, ok := l.reg.Get(id)
	if !ok {
		l.mu.Unlock()
		return core.ErrNoSuchNode
	}
	if err := l.reg.Delete(id); err != nil {
		l.mu.Unlock()
		return err
	}
	l.tracker.Forget(id)
	intents := l.alerter.Forget(id, n.Name)
	l.mu.Unlock()
	for _, in := range intents {
		l.alert(fleetAlert(in, now.Unix()))
	}
	return nil
}

// drain blocks until any in-flight maintenance slice has finished.
func (l *masterLoop) drain() { l.maintaining <- struct{}{} }

func fleetAlert(in fleet.AlertIntent, now int64) Alert {
	kind := "fire"
	if in.Recover {
		kind = "recover"
	}
	return Alert{Key: in.Key, Title: in.Title, Severity: SevCritical, Kind: kind, Source: "fleet", Time: now}
}

func startMaster(ctx context.Context, cfg *config.Config, d fleetDeps, rt *fleetRuntime) error {
	pki := fleetPKIDir(d.stateDir)
	ca, err := fleet.LoadCA(filepath.Join(pki, "ca.crt"), filepath.Join(pki, "ca.key"))
	if err != nil {
		return fmt.Errorf("load CA (run `trinetra fleet init`): %w", err)
	}
	leaf, err := tls.LoadX509KeyPair(filepath.Join(pki, "server.crt"), filepath.Join(pki, "server.key"))
	if err != nil {
		return fmt.Errorf("load server certificate: %w", err)
	}
	if w := serverLeafExpiryWarning(leaf, time.Now()); w != "" {
		d.logf("%s", w)
	}
	dir := fleetMasterDir(d.stateDir)
	reg, err := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		return err
	}
	// Node names are required to be unique for every NEW join or rename
	// (review round 2, item b), but an existing registry from before that
	// requirement is loaded as-is, with no migration -- just a one-time
	// warning naming the duplicates, so the operator knows a
	// silence/maintenance Matcher.Node glob on one of these names may hit
	// more than one node until they're renamed apart.
	if dups := fleet.DuplicateNames(reg.List()); len(dups) > 0 {
		d.logf("fleet: WARNING registry has nodes sharing a name (case-insensitive); a Matcher.Node glob on one of these will match all of them until renamed apart: %s", strings.Join(dups, "; "))
	}
	toks, err := fleet.OpenTokens(filepath.Join(dir, "tokens.json"))
	if err != nil {
		return err
	}
	hub := fleet.NewHub(d.logf)

	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		return fmt.Errorf("load incidents: %w", err)
	}
	silences, err := loadSilenceStore(filepath.Join(dir, "silences.json"))
	if err != nil {
		return fmt.Errorf("load silences: %w", err)
	}
	alerting, err := loadAlertingStore(filepath.Join(dir, "alerting.json"))
	if err != nil {
		return fmt.Errorf("load alerting config: %w", err)
	}
	audit := newAuditLog(filepath.Join(dir, "audit.jsonl"))
	engine := newFleetAlertEngine(time.Now, d.deliverSync, hub.Push, hub.Connected, incidents)
	engine.SetSilences(silences, func(id string) (string, []string) {
		if n, ok := reg.Get(id); ok {
			return n.Name, n.Tags
		}
		return id, nil
	})
	engine.SetRouting(alerting, d.deliverSyncTo, d.dispatchOnly)
	// SetConfig (task 6 fix round 1, IMPORTANT 5): lets tryDeliverGroup read
	// the LIVE fleet.fallback_after (config can change at runtime via `set`)
	// for effectiveGroupInterval's cap.
	engine.SetConfig(d.getCfg)
	// SetDependencies (task 6 part 3): expand a node's DependsOn ("tag:<t>"
	// entries resolved against the registry's CURRENT tag membership, read
	// fresh on every call so a tag added/removed after the fact takes effect
	// immediately) into a concrete node-id list the engine can check for
	// "is any of this node's dependencies currently down" without knowing
	// anything about the registry itself.
	engine.SetDependencies(func(id string) []string {
		n, ok := reg.Get(id)
		if !ok {
			return nil
		}
		var out []string
		seen := map[string]bool{}
		add := func(depID string) {
			if depID == "" || depID == id || seen[depID] {
				return
			}
			seen[depID] = true
			out = append(out, depID)
		}
		for _, d := range n.DependsOn {
			if tag, isTag := strings.CutPrefix(d, "tag:"); isTag {
				for _, other := range reg.List() {
					if slices.Contains(other.Tags, tag) {
						add(other.ID)
					}
				}
				continue
			}
			add(d)
		}
		return out
	})
	// A freshly (re)connected node gets a lease and its current silence set
	// immediately, rather than waiting up to one masterTickInterval /
	// silencePushInterval for the next Tick pass.
	hub.OnConnect(func(id string) {
		now := time.Now()
		engine.PushLeaseNow(id, now)
		engine.PushSilencesNow(id, now)
	})

	sink := newReplicaSink(filepath.Join(dir, "nodes"), storeOptionsFor(cfg), func(nodeID string, ev AlertEvent) {
		name, tags := nodeID, []string(nil)
		if n, ok := reg.Get(nodeID); ok {
			name, tags = n.Name, n.Tags
		}
		engine.HandleChildAlert(nodeID, name, tags, ev)
	})
	sink.onAckSync = engine.HandleChildAckSync
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(cfg.FleetNodeDownAfter()))
	var ids []string
	revoked := map[string]bool{}
	for _, n := range reg.List() {
		ids = append(ids, n.ID)
		revoked[n.ID] = n.Revoked
	}
	tracker.Seed(ids, revoked, time.Now().Unix())
	// SetRules (task 7): reg/sink/tracker now all exist, so the aggregate-
	// rule evaluator can read tags/LastSeen, snapshots/1m series and
	// liveness state directly (fleet-phase2-map.md section 8). started is
	// this master start's own timestamp -- absent(...)'s blind window
	// measures from here, same reference point masterLoop.started uses for
	// its own orphan-check blind window. self (round-1 review fix,
	// IMPORTANT 2) is the master's own node data -- rt.provider.selfName is
	// already built (startFleet, before this role branch ever runs);
	// d.latestSnapshot/d.store are the exact same accessors the daemon's own
	// control socket and local sampler already use for "this host".
	engine.SetRules(reg, sink, tracker, time.Now(), ruleSelfSource{
		Name: rt.provider.selfName,
		Snap: func() (Snapshot, bool) {
			if d.latestSnapshot == nil {
				return Snapshot{}, false
			}
			s := d.latestSnapshot()
			return s, s.TS != 0
		},
		Store: d.store,
	})

	loop := newMasterLoop(reg, tracker, sink, engine, d, time.Now())
	m := fleet.NewMaster(fleet.MasterConfig{
		CA: ca, Leaf: leaf, Registry: reg, Tokens: toks, Sink: sink, Hub: hub, Logf: d.logf,
		OnSkew: loop.observeSkew,
		OnContact: func(id string, now time.Time, u *fleet.LiveUpdate) {
			backlog := int64(-1)
			if u != nil {
				backlog = 0
				if u.Outbox.OldestUnackedTS > 0 {
					backlog = now.Unix() - u.Outbox.OldestUnackedTS
				}
			}
			if _, ok := reg.Get(id); !ok {
				return // removed while this request was in flight
			}
			tracker.Seen(id, now.Unix(), backlog)
		},
	})
	ln, err := net.Listen("tcp", cfg.FleetListen())
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.FleetListen(), err)
	}
	go func() {
		if err := m.Serve(ln); err != nil {
			d.logf("fleet: listener stopped: %v", err)
		}
	}()

	rt.provider.role = config.RoleMaster
	if fleetJoinURL(cfg) == "" {
		d.logf("fleet: WARNING fleet.address is empty; children cannot be given a join URL and token creation is refused. Run `trinetra fleet init --address ...`")
	}

	rt.provider.master = &masterState{reg: reg, tokens: toks, sink: sink, tracker: tracker, loop: loop,
		hub: hub, engine: engine, audit: audit, silences: silences, alerting: alerting,
		joinURL: fleetJoinURL(cfg), pin: fleet.SPKIPin(ca.Cert), listen: ln.Addr().String(), getCfg: d.getCfg}
	loopCtx, cancel := context.WithCancel(ctx)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		defer loop.drain() // wait out an in-flight maintenance slice
		tick := time.NewTicker(masterTickInterval)
		defer tick.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case now := <-tick.C:
				loop.tick(now)
			}
		}
	}()
	rt.stop = func() {
		cancel()
		deadline := time.Now().Add(5 * time.Second)
		sctx, scancel := context.WithDeadline(context.Background(), deadline)
		defer scancel()
		_ = m.Shutdown(sctx)
		if !waitBounded(loopDone, deadline) {
			d.logf("fleet: master loop did not stop within 5s")
		}
		// Drain the alerting engine's keyed dispatcher last, once nothing
		// new can reach it (the listener is closed and the loop has
		// stopped): any delivery still in flight or queued gets up to 5s to
		// actually finish. Anything still undelivered when that expires is
		// NOT retried here -- resurrectMasterAlerts (fleet_engine.go), run
		// at the next start, is what recovers it, since its incident record
		// (written durably before delivery was ever attempted) shows no
		// "delivered" event for it. Shutdown must stay bounded rather than
		// wait forever for a stuck channel.
		if !engine.Stop(5 * time.Second) {
			d.logf("fleet: alerting engine did not finish delivering within 5s; undelivered alerts will be resurrected on next start")
		}
		_ = reg.FlushIfDirty()
	}
	d.logf("fleet: master listening on %s (join URL %s)", ln.Addr(), fleetJoinURL(cfg))
	return nil
}

func startChild(ctx context.Context, cfg *config.Config, d fleetDeps, rt *fleetRuntime) error {
	id, err := fleet.LoadIdentity(fleetChildDir(d.stateDir))
	if err != nil {
		return fmt.Errorf("load identity (re-run `trinetra fleet join`): %w", err)
	}
	ob, err := fleet.OpenOutbox(fleetOutboxDir(d.stateDir), cfg.FleetOutboxMaxBytes())
	if err != nil {
		return fmt.Errorf("open outbox: %w", err)
	}
	tee := newOutboxTee(ob, d.logf)
	live := newLiveBuilder(d.latestSnapshot, d.alertStatePath, func() HostInfo { return collectHostInfoFor(d.getCfg()) })

	// Lease-based alert handoff (fleet_lease.go): while the master holds a
	// valid lease, a firing/recovering alert is routed to it instead of
	// delivered locally (alertRoute, consulted from enqueueAndLog), falling
	// back to local delivery if no receipt arrives within
	// fleet.fallback_after or the lease expires first (handoffState.Tick,
	// driven below). lease/handoffState start with no lease ever granted, so
	// until the first "lease" frame arrives -- including forever, against an
	// old master with no stream endpoint at all -- Route always returns
	// true: local delivery, exactly today's behaviour.
	lease := newLeaseHolder(time.Now)
	handoffState := newHandoff(time.Now, func() time.Duration { return d.getCfg().FleetFallbackAfter() }, lease)
	restoreRoute := setAlertRoute(handoffState.Route)

	// childSilences is this child's copy of the master's last pushed
	// "silences" frame (fleet_silences.go), restored from its sidecar so a
	// restart while the master stays unreachable keeps honouring it for
	// fallback deliveries (see deliverFallback). It applies whatever the
	// master pushed verbatim, with no local Node re-check: the master is the
	// only place with the authoritative registry to resolve Node against
	// (review round 2 -- see pushedSilences's doc comment for why an
	// earlier round's child-side re-check was removed).
	childSilences := loadPushedSilences(childSilencesPath(d.stateDir))

	// Restart safety: handoff.pending lives only in memory, so a routed
	// alert whose receipt (or fallback) hadn't landed yet before this
	// process last stopped would otherwise vanish -- delivered neither by
	// the master (no receipt ever arrived here to prove it) nor locally.
	// Rebuild it from alertlog.jsonl (every routed fire/recover,
	// RoutedToMaster, and every delivered_locally fallback that resolves
	// one) plus the receipts sidecar (every receipt that resolves one --
	// see reconcilePendingFromLog), before the ticker below starts judging
	// anything. The sidecar is pruned to the same reconcile window right
	// after, so it does not grow forever; a prune failure is non-fatal
	// (logged), matching AlertLog's own nil-degrades-gracefully convention.
	receiptsPath := handoffReceiptsPath(d.stateDir)
	fallbackAfter := d.getCfg().FleetFallbackAfter()
	handoffState.Reconcile(reconcilePendingFromLog(d.alog, receiptsPath, fallbackAfter, time.Now()))
	if err := pruneHandoffReceipts(receiptsPath, time.Now().Add(-10*fallbackAfter).Unix()); err != nil {
		d.logf("fleet: could not prune handoff receipts: %v", err)
	}

	sh := fleet.NewShipper(fleet.ShipperConfig{
		MasterURL: cfg.Fleet.MasterURL, Pin: cfg.Fleet.CAPin, Identity: id, Outbox: ob,
		Gaps:      &localGapFiller{store: d.store, alog: d.alog, rawRetention: configuredRawRetention(cfg), now: time.Now},
		Live:      live.Build,
		LiveEvery: time.Duration(cfg.FastInterval) * time.Second,
		Logf:      d.logf,
		OnFrame: func(f fleet.Frame) {
			onStreamFrame(lease, handoffState, receiptsPath, childSilences, time.Now, f)
			applyAckFrame(d.self, f)
		},
	})
	cctx, cancel := context.WithCancel(ctx)
	shipDone := make(chan struct{})
	go func() { defer close(shipDone); sh.Run(cctx) }()
	go func() {
		var la childLinkAlerts
		started := time.Now().Unix()
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case now := <-t.C:
				warnAfter := int64(d.getCfg().FleetLinkDownWarnAfter() / time.Second)
				for _, a := range la.Plan(sh.Status(), cfg.Fleet.MasterURL, started, now.Unix(), warnAfter) {
					d.alert(a)
				}
			}
		}
	}()
	// handoffState.Tick every 5s: any alert whose master receipt is overdue,
	// or whose lease expired first, is delivered locally now (exactly once)
	// via alertFallback -- see fleetDeps.alertFallback and deliverFallback.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				for _, a := range handoffState.Tick() {
					d.alertFallback(a, childSilences)
				}
			}
		}
	}()
	rt.tee = tee
	rt.provider.role = config.RoleChild
	rt.provider.link = sh
	rt.provider.nodeID = id.NodeID()
	rt.provider.masterURL = cfg.Fleet.MasterURL
	rt.stop = func() {
		cancel()
		restoreRoute()
		// Let the shipper finish its in-flight request before the outbox
		// closes under it (bounded, so a hung dial cannot stall shutdown).
		waitBounded(shipDone, time.Now().Add(5*time.Second))
		_ = ob.Close()
	}
	d.logf("fleet: child %s shipping to %s", id.NodeID(), cfg.Fleet.MasterURL)
	return nil
}
