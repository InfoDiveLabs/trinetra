// Package serverwatch: fleet_daemon.go is the single place the daemon's
// fleet role is honoured. startFleet does nothing at all for solo (no
// directories, no listener, no goroutines); for a master it serves the fleet
// port, tracks liveness and raises node-down alerts; for a child it starts
// the shipper and tees local writes into the outbox.
package serverwatch

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	ca, err := fleet.LoadCA(caCrt, caKey)
	if err != nil {
		if ca, err = fleet.NewCA(caName, now); err != nil {
			return err
		}
		if err := ca.Save(caCrt, caKey); err != nil {
			return err
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

// fleetJoinURL is the URL children dial: the first fleet.address with the
// listener's port.
func fleetJoinURL(cfg *config.Config) string {
	host := strings.TrimSpace(strings.Split(cfg.Fleet.Address, ",")[0])
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
	return rt
}

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
	rt.provider.master = &masterState{reg: reg, tokens: toks, sink: sink, tracker: tracker,
		joinURL: fleetJoinURL(cfg), pin: fleet.SPKIPin(ca.Cert), listen: ln.Addr().String(), getCfg: d.getCfg}

	loopCtx, cancel := context.WithCancel(ctx)
	go func() {
		alerter := fleet.NewNodeAlerter()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		lastFlush, lastMaint := time.Now(), time.Now()
		maintaining := make(chan struct{}, 1)
		name := func(id string) string {
			if n, ok := reg.Get(id); ok {
				return n.Name
			}
			return id
		}
		for {
			select {
			case <-loopCtx.Done():
				return
			case now := <-tick.C:
				for _, in := range alerter.Plan(tracker.Evaluate(now.Unix()), now.Unix(), name) {
					d.alert(fleetAlert(in, now.Unix()))
				}
				if now.Sub(lastFlush) >= 30*time.Second {
					lastFlush = now
					if err := reg.FlushIfDirty(); err != nil {
						d.logf("fleet: registry flush: %v", err)
					}
				}
				if now.Sub(lastMaint) >= time.Duration(d.getCfg().SampleInterval)*time.Second {
					lastMaint = now
					select {
					case maintaining <- struct{}{}:
						go func() { defer func() { <-maintaining }(); sink.Maintain(time.Now().Unix()) }()
					default: // previous pass still running
					}
				}
			}
		}
	}()
	rt.stop = func() {
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = m.Shutdown(sctx)
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
		select {
		case <-shipDone:
		case <-time.After(5 * time.Second):
		}
		_ = ob.Close()
	}
	d.logf("fleet: child %s shipping to %s", id.NodeID(), cfg.Fleet.MasterURL)
	return nil
}
