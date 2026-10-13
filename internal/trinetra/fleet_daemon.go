// Package trinetra: fleet_daemon.go is the single place the daemon's fleet role is
// honoured. startFleet does nothing at all for solo.
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
	// alert delivers an alert the ordinary asynchronous way (enqueueAndLog): logged/published
	// immediately.
	alert func(Alert)
	// deliverSync is master-only: it delivers an alert SYNCHRONOUSLY, directly against the
	// master's own Dispatcher (bypassing the async NotifierQueue).
	deliverSync func(Alert) bool
	// deliverSyncTo is deliverSync narrowed to a specific channel-name subset same
	// synchronous, completion-reporting contract.
	deliverSyncTo func(a Alert, channels []string) bool
	// dispatchOnly delivers to a channel-name subset WITHOUT logging to the alert log/live
	// bus: used for escalation/repeat notifications.
	dispatchOnly func(a Alert, channels []string) bool
	// alertFallback delivers a alert locally after the child's lease/receipt handoff
	// (fleet_lease.go) gave up waiting on the master: it mirrors alert's construction.
	alertFallback func(a Alert, silences *pushedSilences, prefix string)
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

// fleetInitPKI creates (or reuses) the master CA and issues a fresh server leaf for hosts..
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
		// Replacing an existing CA would silently rotate the pin and orphan every enrolled child,
		// so a load failure is an error, never a reason to mint a new one.
		if ca, err = fleet.LoadCA(caCrt, caKey); err != nil {
			return fmt.Errorf("fleet CA at %s is unreadable or incomplete; refusing to replace it — fix or remove both files: %w", dir, err)
		}
	}
	leaf, key, err := ca.IssueServer(hosts, now)
	if err != nil {
		return err
	}
	if err := writeFileAtomicSynced(filepath.Join(dir, "server.key"), key, 0o600); err != nil {
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

// fileMissing reports whether path does not exist; any other stat error is returned.
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

// fleetJoinURL is the URL children dial: the first fleet.address with the listener's port.
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

// masterLoop is the master's periodic work: pick up nodes the tracker does not know yet,
// evaluate liveness and raise alerts.
type masterLoop struct {
	reg     *fleet.Registry
	tracker *fleet.Tracker
	sink    *replicaSink
	// engine is the master alerting engine (fleet_engine.go): every alert this loop raises.
	engine *fleetAlertEngine
	// managed drives the periodic managed-config push cadence (TickManaged); nil-safe (a
	// bare-bones masterLoop from an older test suite never calls it).
	managed *managedPusher
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
	// lastDropCheck is when checkDrops last ran.
	lastDropCheck time.Time
	// started is when this masterLoop was built (master start or latest restart).
	// nodeDownAfter is the blind window.
	started       time.Time
	nodeDownAfter time.Duration
	orphanChecked bool
}

// masterTickInterval is how often the master loop ticks.
const masterTickInterval = 5 * time.Second

// masterShutdownDeadline bounds how long rt.stop waits for m.Shutdown / the master loop /
// the alerting engine to finish once the master is asked to stop.
var masterShutdownDeadline = 5 * time.Second

// newMasterLoop builds a masterLoop.
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

// checkDrops logs a warning for every node whose replica dropped points out of order or
// over the series limit since the previous check.
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

// trackUnseen registers every registry node the tracker has never heard of (joined after
// the master started and never made contact) as last seen at its join time.
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

// nodeDownAlerted reports whether the loop has an individual node-down alert
// open for id; a node lost as part of a fleet-wide connectivity drop does not.
func (l *masterLoop) nodeDownAlerted(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.alerter != nil && l.alerter.Alerted(id)
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
	if l.engine != nil || l.managed != nil {
		for _, n := range l.reg.List() {
			if !n.Revoked {
				nodeIDs = append(nodeIDs, n.ID)
			}
		}
	}
	// checkOrphanedIncidents runs exactly once, on the first tick at or after the blind
	// window.
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
	// Lease cadence: push lease{until: now+90s} to every connected, non-revoked node at least
	// every 30s.
	if l.engine != nil {
		l.engine.TickLeases(now, nodeIDs)
		l.engine.TickSilences(now, nodeIDs)
		l.engine.TickEscalations(now)
		l.engine.TickGrouping(now)
		// TickRules self-gates to ruleTickInterval (30s); called
		// every 5s tick exactly like the others above, cheap no-op otherwise.
		l.engine.TickRules(now)
	}
	// TickManaged self-gates to managedPushInterval (10m); nil-safe.
	l.managed.TickManaged(now, nodeIDs)
	if now.Sub(l.lastFlush) >= 30*time.Second {
		l.lastFlush = now
		if err := l.reg.FlushIfDirty(); err != nil {
			l.logf("fleet: registry flush: %v", err)
		}
	}
	if now.Sub(l.lastDropCheck) >= storeMaintenanceInterval {
		l.checkDrops(now)
	}
	// Replica maintenance rewrites and fsyncs series files, so it runs on the local store's
	// cadence (storeMaintenanceInterval), a slice of the nodes per tick.
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

// stillActive reports whether the condition key names is still true.
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
		// false if the rule no longer exists in the live config, or its current in-memory state
		// (reset by this restart) is not firing; see fleetAlertEngine.ruleStillFiring.
		if l.engine == nil {
			return false
		}
		return l.engine.ruleStillFiring(strings.TrimPrefix(key, "fleet:rule:"))
	default:
		return true
	}
}

// checkOrphanedIncidents recovers any open (firing or acked) master-own incident whose
// condition is no longer active.
func (l *masterLoop) checkOrphanedIncidents(now time.Time, ev fleet.Evaluation) {
	if l.engine == nil || l.engine.incidents == nil {
		return
	}
	for _, inc := range l.engine.incidents.List(core.IncidentFilter{}, nil) {
		if !isOpenState(inc.State) || len(inc.Alerts) == 0 {
			continue // not open, or nothing recorded on it
		}
		// A grouped or dependency-folded incident can hold several independent master-own members
		// (e.g. two down nodes sharing one incident, or a dependency fold).
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

// orphanRecoverTitle builds a human-readable recover title for an orphaned incident being
// reconciled after a restart, reusing the incident's own recorded title.
func orphanRecoverTitle(title string) string {
	return title + " -- recovered (reconciled after restart)"
}

// observeSkew is the master's OnSkew hook: it adds the sample to the node's filtered skew
// (see replicaSink.RecordSkew), hands that to the tracker (|skew| > 30 s is lagging).
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
	// Node names are required to be unique for every NEW join or rename but an existing
	// registry from before that requirement is loaded as-is, with no migration.
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
	managed, err := loadManagedFragmentStore(filepath.Join(dir, "managed.json"))
	if err != nil {
		return fmt.Errorf("load managed config: %w", err)
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
	// SetConfig: lets tryDeliverGroup read the LIVE fleet.fallback_after (config can change at
	// runtime via `set`) for effectiveGroupInterval's cap.
	engine.SetConfig(d.getCfg)
	// SetDependencies: expand a node's DependsOn.
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
	// managedPusher drives the master's managed_config push: on change
	// (SaveManaged/DeleteManaged, fleetAPIImpl), on connect (below).
	managedPush := newManagedPusher(hub.Push, hub.Connected, managed, func(id string) []string {
		if n, ok := reg.Get(id); ok {
			return n.Tags
		}
		return nil
	})

	// A freshly (re)connected node gets a lease, its current silence set and its current
	// managed-config desired set immediately.
	hub.OnConnect(func(id string) {
		now := time.Now()
		engine.PushLeaseNow(id, now)
		engine.PushSilencesNow(id, now)
		managedPush.PushOne(id)
	})

	sink := newReplicaSink(filepath.Join(dir, "nodes"), storeOptionsFor(cfg), func(nodeID string, ev AlertEvent) {
		name, tags := nodeID, []string(nil)
		if n, ok := reg.Get(nodeID); ok {
			name, tags = n.Name, n.Tags
		}
		engine.HandleChildAlert(nodeID, name, tags, ev)
	})
	sink.onAckSync = engine.HandleChildAckSync
	// rpcReg is the master's pending-RPC registry for remote calls (currently just
	// container_logs) over the master-to-child stream: wired into both the hub.
	rpcReg := newRPCRegistry(time.Now, d.logf)
	hub.OnRPCResult(rpcReg.Deliver)
	sink.hub = hub
	sink.rpc = rpcReg
	sink.incidents = incidents
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(cfg.FleetNodeDownAfter()))
	var ids []string
	revoked := map[string]bool{}
	for _, n := range reg.List() {
		ids = append(ids, n.ID)
		revoked[n.ID] = n.Revoked
	}
	tracker.Seed(ids, revoked, time.Now().Unix())
	// SetRules: reg/sink/tracker now all exist, so the aggregate-rule evaluator can read
	// tags/LastSeen.
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
	loop.managed = managedPush
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
		managed: managed, managedPush: managedPush,
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
		// Force-close every live /fleet/v1/stream connection before waiting on m.Shutdown:
		// http.Server.Shutdown does not cancel a still-running handler's own request context.
		hub.CloseAll()
		deadline := time.Now().Add(masterShutdownDeadline)
		sctx, scancel := context.WithDeadline(context.Background(), deadline)
		defer scancel()
		_ = m.Shutdown(sctx)
		if !waitBounded(loopDone, deadline) {
			d.logf("fleet: master loop did not stop within 5s")
		}
		// Drain the alerting engine's keyed dispatcher last, once nothing new can reach it (the
		// listener is closed and the loop has stopped).
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
	// managedState is this child's managed-config state, restored from its sidecar.
	managedState := loadManagedChild(managedChildPath(d.stateDir), d.getCfg, d.self, time.Now)
	// Reconcile once, right after restoring managedState and before the shipper starts.
	reconcileManagedValuesAtStart(managedState, d.logf)
	live := newLiveBuilder(d.latestSnapshot, d.alertStatePath, func() HostInfo { return collectHostInfoFor(d.getCfg()) }, managedState)

	// Lease-based alert handoff (fleet_lease.go): while the master holds a valid lease, a
	// firing/recovering alert is routed to it instead of delivered locally.
	lease := newLeaseHolder(time.Now)
	handoffState := newHandoff(time.Now, func() time.Duration { return d.getCfg().FleetFallbackAfter() }, lease)
	restoreRoute := setAlertRoute(handoffState.Route)

	// childSilences is this child's copy of the master's last pushed "silences" frame.
	childSilences := loadPushedSilences(childSilencesPath(d.stateDir))

	// Restart safety: handoff.pending lives only in memory, so a routed alert whose receipt
	// (or fallback) hadn't landed yet before this process last stopped would otherwise vanish.
	receiptsPath := handoffReceiptsPath(d.stateDir)
	fallbackAfter := d.getCfg().FleetFallbackAfter()
	handoffState.Reconcile(reconcilePendingFromLog(d.alog, receiptsPath, fallbackAfter, time.Now()))
	if err := pruneHandoffReceipts(receiptsPath, time.Now().Add(-10*fallbackAfter).Unix()); err != nil {
		d.logf("fleet: could not prune handoff receipts: %v", err)
	}

	// sh is declared with var (rather than :=) so OnFrame's closure -- which runs only later,
	// off the stream's read loop, well after this literal finishes constructing it.
	var sh *fleet.Shipper
	sh = fleet.NewShipper(fleet.ShipperConfig{
		MasterURL: cfg.Fleet.MasterURL, Pin: cfg.Fleet.CAPin, Identity: id, Outbox: ob,
		Gaps:      &localGapFiller{store: d.store, alog: d.alog, rawRetention: configuredRawRetention(cfg), now: time.Now},
		Live:      live.Build,
		LiveEvery: time.Duration(cfg.FastInterval) * time.Second,
		Logf:      d.logf,
		OnFrame: func(f fleet.Frame) {
			onStreamFrame(lease, handoffState, receiptsPath, childSilences, time.Now, f)
			applyAckFrame(d.self, f)
			applyManagedConfigFrame(managedState, f)
			handleRPCFrame(d.self, sh, d.logf, f)
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
				st := sh.Status()
				handleLinkRevocation(st, lease, handoffState, childSilences, d.alertFallback)
				warnAfter := int64(d.getCfg().FleetLinkDownWarnAfter() / time.Second)
				for _, a := range la.Plan(st, cfg.Fleet.MasterURL, started, now.Unix(), warnAfter) {
					d.alert(a)
				}
			}
		}
	}()
	// handoffState.Tick every 5s: any alert whose master receipt is overdue, or whose lease
	// expired first, is delivered locally now (exactly once) via alertFallback.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				for _, a := range handoffState.Tick() {
					d.alertFallback(a, childSilences, fallbackPrefix)
				}
			}
		}
	}()
	rt.tee = tee
	rt.provider.role = config.RoleChild
	rt.provider.link = sh
	rt.provider.nodeID = id.NodeID()
	rt.provider.masterURL = cfg.Fleet.MasterURL
	rt.provider.managed = managedState
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
