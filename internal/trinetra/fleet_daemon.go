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
	// alert is master-only: it delivers an alert through the master's own
	// dispatcher and reports whether at least one channel accepted it (see
	// deliverSyncAndLog and fleetAlertEngine.deliver, fleet_engine.go) --
	// the fleet alerting engine needs that completion signal before it may
	// push a receipt down to a node.
	alert func(Alert) bool
	// alertFallback delivers a alert locally after the child's lease/receipt
	// handoff (fleet_lease.go) gave up waiting on the master: it mirrors
	// alert's construction (same alog/bus/q closed over) but calls
	// deliverFallback instead of enqueueAndLog, since that delivery must be
	// unconditional (see deliverFallback's doc comment). Only startChild
	// ever calls it; solo and master never construct a handoff to call it
	// from.
	alertFallback func(a Alert)
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
	alert       func(Alert) bool
	getCfg      func() *config.Config
	logf        func(string, ...any)
	lastFlush   time.Time
	maint       maintScheduler // only touched by the maintenance goroutine
	maintaining chan struct{}  // holds a token while a maintenance slice runs
	// lastDropCheck is when checkDrops last ran. Only tick touches it; the
	// per-node baselines live in each replica's ingest.state.
	lastDropCheck time.Time
}

// masterTickInterval is how often the master loop ticks.
const masterTickInterval = 5 * time.Second

// newMasterLoop builds a masterLoop. A nil engine (test convenience for
// suites that don't exercise alerting) makes l.alert fall back to d.alert
// directly, exactly as before the alerting engine existed.
func newMasterLoop(reg *fleet.Registry, tracker *fleet.Tracker, sink *replicaSink, engine *fleetAlertEngine, d fleetDeps, now time.Time) *masterLoop {
	alert := d.alert
	if engine != nil {
		alert = func(a Alert) bool { engine.Submit(alertSource{}, a); return true }
	}
	return &masterLoop{reg: reg, tracker: tracker, sink: sink, engine: engine, alerter: fleet.NewNodeAlerter(), alert: alert,
		getCfg: d.getCfg, logf: d.logf, lastFlush: now, maintaining: make(chan struct{}, 1), lastDropCheck: now}
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
	intents := l.alerter.Plan(l.tracker.Evaluate(now.Unix()), now.Unix(), name)
	var nodeIDs []string
	if l.engine != nil {
		for _, n := range l.reg.List() {
			if !n.Revoked {
				nodeIDs = append(nodeIDs, n.ID)
			}
		}
	}
	l.mu.Unlock()
	for _, in := range intents {
		l.alert(fleetAlert(in, now.Unix()))
	}
	// Lease cadence (global-constraints/task-3 ruling): push lease{until:
	// now+90s} to every connected, non-revoked node at least every 30s.
	// Revoked/removed nodes are simply absent from nodeIDs. A freshly
	// connected node also gets one immediately via Hub.OnConnect
	// (engine.PushLeaseNow), so this loop need not special-case "just
	// joined".
	if l.engine != nil {
		l.engine.TickLeases(now, nodeIDs)
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
	toks, err := fleet.OpenTokens(filepath.Join(dir, "tokens.json"))
	if err != nil {
		return err
	}
	hub := fleet.NewHub(d.logf)

	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		return fmt.Errorf("load incidents: %w", err)
	}
	audit := newAuditLog(filepath.Join(dir, "audit.jsonl"))
	engine := newFleetAlertEngine(time.Now, d.alert, hub.Push, hub.Connected, incidents)
	// A freshly (re)connected node gets a lease immediately, rather than
	// waiting up to one masterTickInterval for the next TickLeases pass.
	hub.OnConnect(func(id string) { engine.PushLeaseNow(id, time.Now()) })

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
		hub: hub, engine: engine, audit: audit,
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
			onStreamFrame(lease, handoffState, receiptsPath, time.Now, f)
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
					d.alertFallback(a)
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
