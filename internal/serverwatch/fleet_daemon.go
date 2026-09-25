// Package serverwatch: fleet_daemon.go is the single place the daemon's
// fleet role is honoured. startFleet does nothing at all for solo (no
// directories, no listener, no goroutines); for a master it serves the fleet
// port, tracks liveness and raises node-down alerts; for a child it starts
// the shipper and tees local writes into the outbox.
package serverwatch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
	"serverwatch/internal/fleet"
)

type fleetDeps struct {
	stateDir       string
	getCfg         func() *config.Config
	self           core.API
	latestSnapshot func() Snapshot
	store          SampleStore
	alog           *AlertLog
	alertStatePath string
	alert          func(Alert)
	logf           func(string, ...any)
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
	reg         *fleet.Registry
	tracker     *fleet.Tracker
	sink        *replicaSink
	alerter     *fleet.NodeAlerter
	alert       func(Alert)
	getCfg      func() *config.Config
	logf        func(string, ...any)
	lastFlush   time.Time
	maint       maintScheduler // only touched by the maintenance goroutine
	maintaining chan struct{}  // holds a token while a maintenance slice runs
}

// masterTickInterval is how often the master loop ticks.
const masterTickInterval = 5 * time.Second

func newMasterLoop(reg *fleet.Registry, tracker *fleet.Tracker, sink *replicaSink, d fleetDeps, now time.Time) *masterLoop {
	return &masterLoop{reg: reg, tracker: tracker, sink: sink, alerter: fleet.NewNodeAlerter(), alert: d.alert,
		getCfg: d.getCfg, logf: d.logf, lastFlush: now, maintaining: make(chan struct{}, 1)}
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
	trackUnseen(l.reg, l.tracker, now.Unix())
	name := func(id string) string {
		if n, ok := l.reg.Get(id); ok {
			return n.Name
		}
		return id
	}
	for _, in := range l.alerter.Plan(l.tracker.Evaluate(now.Unix()), now.Unix(), name) {
		l.alert(fleetAlert(in, now.Unix()))
	}
	if now.Sub(l.lastFlush) >= 30*time.Second {
		l.lastFlush = now
		if err := l.reg.FlushIfDirty(); err != nil {
			l.logf("fleet: registry flush: %v", err)
		}
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
		return fmt.Errorf("load CA (run `serverwatch fleet init`): %w", err)
	}
	leaf, err := tls.LoadX509KeyPair(filepath.Join(pki, "server.crt"), filepath.Join(pki, "server.key"))
	if err != nil {
		return fmt.Errorf("load server certificate: %w", err)
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
	sink := newReplicaSink(filepath.Join(dir, "nodes"), storeOptionsFor(cfg))
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(cfg.FleetNodeDownAfter()))
	var ids []string
	revoked := map[string]bool{}
	for _, n := range reg.List() {
		ids = append(ids, n.ID)
		revoked[n.ID] = n.Revoked
	}
	tracker.Seed(ids, revoked, time.Now().Unix())

	m := fleet.NewMaster(fleet.MasterConfig{
		CA: ca, Leaf: leaf, Registry: reg, Tokens: toks, Sink: sink, Logf: d.logf,
		OnContact: func(id string, now time.Time, u *fleet.LiveUpdate) {
			backlog := int64(-1)
			if u != nil {
				backlog = 0
				if u.Outbox.OldestUnackedTS > 0 {
					backlog = now.Unix() - u.Outbox.OldestUnackedTS
				}
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
		d.logf("fleet: WARNING fleet.address is empty; children cannot be given a join URL and token creation is refused. Run `serverwatch fleet init --address ...`")
	}
	rt.provider.master = &masterState{reg: reg, tokens: toks, sink: sink, tracker: tracker,
		joinURL: fleetJoinURL(cfg), pin: fleet.SPKIPin(ca.Cert), listen: ln.Addr().String(), getCfg: d.getCfg}

	loop := newMasterLoop(reg, tracker, sink, d, time.Now())
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
		return fmt.Errorf("load identity (re-run `serverwatch fleet join`): %w", err)
	}
	ob, err := fleet.OpenOutbox(fleetOutboxDir(d.stateDir), cfg.FleetOutboxMaxBytes())
	if err != nil {
		return fmt.Errorf("open outbox: %w", err)
	}
	tee := newOutboxTee(ob, d.logf)
	live := newLiveBuilder(d.latestSnapshot, d.alertStatePath, func() HostInfo { return collectHostInfoFor(d.getCfg()) })
	sh := fleet.NewShipper(fleet.ShipperConfig{
		MasterURL: cfg.Fleet.MasterURL, Pin: cfg.Fleet.CAPin, Identity: id, Outbox: ob,
		Gaps:      &localGapFiller{store: d.store, alog: d.alog, rawRetention: configuredRawRetention(cfg), now: time.Now},
		Live:      live.Build,
		LiveEvery: time.Duration(cfg.FastInterval) * time.Second,
		Logf:      d.logf,
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
				for _, a := range la.Plan(sh.Status(), cfg.Fleet.MasterURL, started, now.Unix()) {
					d.alert(a)
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
		// Let the shipper finish its in-flight request before the outbox
		// closes under it (bounded, so a hung dial cannot stall shutdown).
		waitBounded(shipDone, time.Now().Add(5*time.Second))
		_ = ob.Close()
	}
	d.logf("fleet: child %s shipping to %s", id.NodeID(), cfg.Fleet.MasterURL)
	return nil
}
