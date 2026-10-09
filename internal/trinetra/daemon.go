package trinetra

import (
	"context"
	"crypto/rand"
	"fmt"
	"html"
	"math/big"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/telegram"
)

// buildFastChecks builds anomaly Checks for the cheap, high-frequency metrics
// (cpu/mem/swap/temp) that collectFast populates every fast tick.
func buildFastChecks(snap Snapshot, c *config.Config) []Check {
	var checks []Check
	fastInterval := c.FastInterval
	add := func(key string, val, thr float64, hasThr, crit bool, culprit string) {
		if !c.TargetEnabled(key) {
			return
		}
		if o, ok := c.TargetThreshold(key); ok {
			thr, hasThr = o, true
		}
		ch := Check{Key: key, Value: val, Threshold: thr, HasThreshold: hasThr, Critical: crit, Interval: fastInterval}
		// Name the resource culprit in the fire message (#103) when there is one to name;
		// breach() uses FireMsg verbatim over the numeric format.
		if culprit != "" && hasThr {
			ch.FireMsg = fmt.Sprintf("%s = %.1f ≥ threshold %.1f%s", key, val, thr, culprit)
		}
		checks = append(checks, ch)
	}
	add("cpu", snap.CPU, c.Thresholds.CPUPct, true, false, cpuCulprit(snap))
	add("mem", snap.MemPct, c.Thresholds.MemPct, true, false, memCulprit(snap))
	add("swap", snap.SwapPct, c.Thresholds.SwapPct, true, false, "")
	if snap.TempC > 0 {
		add("temp", snap.TempC, c.Thresholds.TempC, true, false, "")
	}
	return checks
}

// binaryCheck builds a Check for a binary health state (docker/service/ smart: either "ok"
// or "bad", never a graduated numeric reading), so fire/recover text can be human wording.
func binaryCheck(key string, bad bool, fireMsg, recoverMsg string, interval int) Check {
	v := 0.0
	if bad {
		v = 1
	}
	return Check{Key: key, Value: v, Threshold: 1, HasThreshold: true, Critical: true,
		Interval: interval, FireMsg: fireMsg, RecoverMsg: recoverMsg}
}

// buildSlowChecks builds anomaly Checks for the expensive metrics.
func buildSlowChecks(snap Snapshot, c *config.Config, active map[string]ActiveAlert) []Check {
	var checks []Check
	slowInterval := c.SampleInterval
	add := func(key string, val, thr float64, hasThr, crit bool) {
		if !c.TargetEnabled(key) {
			return
		}
		if o, ok := c.TargetThreshold(key); ok {
			thr, hasThr = o, true
		}
		checks = append(checks, Check{Key: key, Value: val, Threshold: thr, HasThreshold: hasThr, Critical: crit, Interval: slowInterval})
	}
	for mount, pct := range snap.Disks {
		add("disk:"+mount, pct, c.Thresholds.DiskPct, true, true)
	}
	// docker containers: running=ok(0), anything else=bad(1)
	for name, state := range snap.Containers {
		key := "docker:" + name
		if !c.TargetEnabled(key) {
			continue
		}
		bad := state != "running"
		fireMsg := fmt.Sprintf("container %s is down (%s)", name, state)
		recoverMsg := fmt.Sprintf("container %s recovered", name)
		checks = append(checks, binaryCheck(key, bad, fireMsg, recoverMsg, slowInterval))
	}
	// SMART: FAILED=bad(1), otherwise ok(0)
	for dev, health := range snap.SmartHealth {
		key := "smart:" + dev
		if !c.TargetEnabled(key) {
			continue
		}
		bad := health == "FAILED"
		fireMsg := fmt.Sprintf("SMART FAILED on %s", dev)
		recoverMsg := fmt.Sprintf("SMART health recovered on %s", dev)
		checks = append(checks, binaryCheck(key, bad, fireMsg, recoverMsg, slowInterval))
	}
	// systemd: only failed units appear in the list.
	failed := map[string]bool{}
	for _, u := range snap.FailedUnits {
		key := "service:" + u
		failed[key] = true
		if !c.TargetEnabled(key) {
			continue
		}
		fireMsg := fmt.Sprintf("unit %s failed", u)
		recoverMsg := fmt.Sprintf("unit %s recovered", u)
		checks = append(checks, binaryCheck(key, true, fireMsg, recoverMsg, slowInterval))
	}
	for key := range active {
		if strings.HasPrefix(key, "service:") && !failed[key] {
			u := strings.TrimPrefix(key, "service:")
			recoverMsg := fmt.Sprintf("unit %s recovered", u)
			checks = append(checks, binaryCheck(key, false, "", recoverMsg, slowInterval))
		}
	}
	// docker/smart recovery sweep: if a container is REMOVED (not just stopped) or `docker
	// ps`/`smartctl --scan` starts erroring.
	for key := range active {
		if strings.HasPrefix(key, "docker:") {
			name := strings.TrimPrefix(key, "docker:")
			if _, ok := snap.Containers[name]; !ok {
				recoverMsg := fmt.Sprintf("container %s recovered", name)
				checks = append(checks, binaryCheck(key, false, "", recoverMsg, slowInterval))
			}
		}
		if strings.HasPrefix(key, "smart:") {
			dev := strings.TrimPrefix(key, "smart:")
			if _, ok := snap.SmartHealth[dev]; !ok {
				recoverMsg := fmt.Sprintf("SMART health recovered on %s", dev)
				checks = append(checks, binaryCheck(key, false, "", recoverMsg, slowInterval))
			}
		}
	}
	// collector:<name> health checks (#110): a slow-tier collector failing for
	// collectorAlertThreshold consecutive cycles fires; recovers on success.
	checks = append(checks, buildCollectorChecks(snap, slowInterval)...)
	return checks
}

// buildChecks returns the full fast+slow check set.
func buildChecks(snap Snapshot, c *config.Config, active map[string]ActiveAlert) []Check {
	checks := buildFastChecks(snap, c)
	checks = append(checks, buildSlowChecks(snap, c, active)...)
	return checks
}

// fastMetricSet converts the fast-tier fields of a Snapshot into the MetricSet a
// SampleStore Append expects, for the write-path counterpart of buildFastChecks.
func fastMetricSet(s Snapshot) MetricSet {
	ms := MetricSet{
		"cpu":    s.CPU,
		"mem":    s.MemPct,
		"swap":   s.SwapPct,
		"load1":  s.Load1,
		"load5":  s.Load5,
		"load15": s.Load15,
	}
	if s.TempC > 0 {
		ms["temp"] = s.TempC
	}
	return ms
}

// slowMetricSet converts the slow-tier fields of a Snapshot into a MetricSet, for the
// write-path counterpart of buildSlowChecks.
func slowMetricSet(s Snapshot) MetricSet {
	ms := make(MetricSet, len(s.Disks))
	for mount, pct := range s.Disks {
		ms["disk:"+mount] = pct
	}
	return ms
}

// containerMetricSet converts a Snapshot's per-container docker stats (s.ContainerStats,
// filled by collectSlow when collect.container_stats is enabled) into a MetricSet.
func containerMetricSet(s Snapshot) MetricSet {
	ms := make(MetricSet, len(s.ContainerStats)*2)
	for name, cs := range s.ContainerStats {
		ms["docker:"+name+":cpu"] = cs.CPUPct
		ms["docker:"+name+":mem"] = cs.MemMiB
	}
	return ms
}

// netRateMetricSet converts a NetRateCalc.Rates() result into a MetricSet: one
// "net:<iface>:rx" and one "net:<iface>:tx" entry (bytes/sec) per interface.
func netRateMetricSet(rates map[string]IfaceRate) MetricSet {
	ms := make(MetricSet, len(rates)*2)
	for iface, r := range rates {
		ms["net:"+iface+":rx"] = r.RxBps
		ms["net:"+iface+":tx"] = r.TxBps
	}
	return ms
}

// smartMetricSet converts a Snapshot's per-device SMART attributes (s.SmartAttrs, filled by
// collectSlow for every discovered SMART device) into a MetricSet.
func smartMetricSet(s Snapshot) MetricSet {
	ms := make(MetricSet, len(s.SmartAttrs))
	for dev, a := range s.SmartAttrs {
		if a.TempC > 0 {
			ms["smart:"+dev+":temp"] = float64(a.TempC)
		}
	}
	return ms
}

// shouldHeartbeat reports whether at least intervalSec seconds have passed since lastUnix
// (the last written heartbeat), given the current time nowUnix. lastUnix==0.
func shouldHeartbeat(lastUnix, nowUnix int64, intervalSec int) bool {
	if intervalSec <= 0 {
		return true
	}
	return nowUnix-lastUnix >= int64(intervalSec)
}

func pingHealthchecks(url string, httpGet func(string) error) {
	if url == "" {
		return
	}
	_ = httpGet(url)
}

func httpPing(url string) error { return httpPingGuarded(url, false) }

// httpPingGuarded is httpPing with the opt-in SSRF guard (#97): when block is true it
// refuses to ping a loopback/link-local/private healthchecks URL.
func httpPingGuarded(url string, block bool) error {
	// A bare http.Get has no timeout: a hung healthchecks endpoint would block
	// the sampler loop indefinitely. Bound it.
	client := newGuardedHTTPClient(10*time.Second, block)
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// collectFast gathers the cheap, high-frequency resources (/proc reads and the /sys thermal
// read). prev is the caller's single-goroutine-owned CPUStat.
func collectFast(x Exec, fs FileSource, prev *CPUStat) Snapshot {
	var snap Snapshot
	if b, err := fs.Read("/proc/stat"); err == nil {
		if cur, err := parseProcStat(string(b)); err == nil {
			if prev.Total != 0 {
				snap.CPU = cpuBusyPct(*prev, cur)
			}
			*prev = cur
		}
	}
	if b, err := fs.Read("/proc/meminfo"); err == nil {
		if m, err := parseMeminfo(string(b)); err == nil {
			snap.MemPct = m.UsedPct()
			snap.SwapPct = m.SwapUsedPct()
		}
	}
	if b, err := fs.Read("/proc/loadavg"); err == nil {
		snap.Load1, snap.Load5, snap.Load15, _ = parseLoadavg(string(b))
	}
	if zones, _ := fs.Glob("/sys/class/thermal/thermal_zone*/temp"); len(zones) > 0 {
		if b, err := fs.Read(zones[0]); err == nil {
			snap.TempC, _ = parseThermal(string(b))
		}
	}
	return snap
}

// smartCache holds the last SMART scan so collectSlow can reuse it between scans.
type smartCache struct {
	lastScan int64
	health   map[string]string
	attrs    map[string]SmartAttr
}

// collectSlow gathers the expensive resources: shelling out to df/docker/
// systemctl/smartctl, plus the network connectivity dial.
func smartScanAttempt(x Exec, snap *Snapshot) ([]byte, error) {
	snap.collectorsAttempted["smart"] = true
	return runMaybeSudo(x, "smartctl", "--scan")
}

func collectSlow(x Exec, fs FileSource, da dockerAccess, c *config.Config, store SampleStore, nowUnix int64, sc *smartCache, smartIntervalSec int) Snapshot {
	var snap Snapshot
	snap.DockerAccess = da.method
	// Per-collector attempt/error tracking (#110): disk and the failed-units check always run.
	snap.collectorsAttempted = map[string]bool{"disk": true, "services": true}
	// snap.Disks and snap.DiskDetail are BOTH derived from the single typed `df -PT -B1` call,
	// gated by isRealMount && isRealFsType.
	if out, err := x.Run("df", "-PT", "-B1"); err == nil {
		types := parseDFTypes(string(out))
		var inodes map[string]float64
		if iout, ierr := x.Run("df", "-Pi"); ierr == nil {
			inodes = parseDFInodes(string(iout))
		}
		snap.Disks = map[string]float64{}
		snap.DiskDetail = map[string]DiskDetail{}
		for mount, d := range types {
			if !isRealMount(mount) || !isRealFsType(d.FsType) {
				continue
			}
			snap.Disks[mount] = d.UsagePct
			if pct, ok := inodes[mount]; ok {
				d.InodePct = pct
			}
			if days, ok := projectMountDaysToFull(store, mount, d.UsagePct, c, nowUnix); ok {
				d.DaysToFull = days
				d.DaysToFullKnown = true
			}
			snap.DiskDetail[mount] = d
		}
	} else {
		recordCollectorErr(&snap, "disk", err)
	}
	snap.Online = checkOnline(defaultConnHosts, connDial)

	// docker containers (ps -a lists all, so state is always known -> recovery works)
	if da.available {
		snap.collectorsAttempted["docker"] = true
		if cs, err := da.list(x); err == nil {
			snap.Containers = map[string]string{}
			for _, ct := range cs {
				snap.Containers[ct.Name] = ct.State
			}
		} else {
			recordCollectorErr(&snap, "docker", err)
		}
	}
	// per-container cpu/mem/net (opt-in, docker-availability-gated): a stats error.
	if da.available && c != nil && c.ContainerStatsEnabled() {
		if cs, err := da.stats(x); err == nil {
			snap.ContainerStats = map[string]ContainerStat{}
			for _, s := range cs {
				snap.ContainerStats[s.Name] = s
			}
		}
	}
	// Swarm task -> service collapse (#118): key containers and their stats by stable service
	// name instead of ephemeral task name.
	if da.swarm {
		snap.Containers, snap.ContainerStats = collapseSwarmTasks(snap.Containers, snap.ContainerStats)
	}
	// failed systemd units (alerting collection -- always runs, unaffected by
	// collect.services below)
	if out, err := runMaybeSudo(x, "systemctl", "--failed", "--plain", "--no-legend"); err == nil {
		snap.FailedUnits = parseFailedUnits(string(out))
	} else {
		recordCollectorErr(&snap, "services", err)
	}
	// full systemd unit inventory.
	if c != nil && c.ServicesEnabled() {
		if units, err := listUnits(x); err == nil {
			snap.Units = units
		}
	}
	// SMART health for every discovered device (all queried each cycle -> recovery works),
	// throttled to smartIntervalSec.
	doScan := sc == nil || sc.health == nil || nowUnix-sc.lastScan >= int64(smartIntervalSec)
	if !doScan {
		snap.SmartHealth = sc.health
		if c != nil && c.SmartAttrsEnabled() {
			snap.SmartAttrs = sc.attrs
		}
	} else if out, err := smartScanAttempt(x, &snap); err != nil {
		recordCollectorErr(&snap, "smart", err)
	} else {
		snap.SmartHealth = map[string]string{}
		// SMART attribute detail (temp/wear/realloc, opt-in via collect.smart_attrs) is the
		// heaviest optional per-device call: one extra smartctl invocation per discovered device.
		attrsEnabled := c != nil && c.SmartAttrsEnabled()
		if attrsEnabled {
			snap.SmartAttrs = map[string]SmartAttr{}
		}
		for _, dev := range parseSmartScan(string(out)) {
			h := "UNKNOWN"
			if ho, herr := runMaybeSudo(x, "smartctl", "-H", dev); herr == nil {
				if passed, ok := parseSmartHealth(string(ho)); ok {
					if passed {
						h = "PASSED"
					} else {
						h = "FAILED"
					}
				}
			}
			snap.SmartHealth[dev] = h
			if !attrsEnabled {
				continue
			}
			// A failure here just leaves this device out of SmartAttrs for
			// the tick, same as the health check above.
			if ao, aerr := runMaybeSudo(x, "smartctl", "-A", dev); aerr == nil {
				snap.SmartAttrs[dev] = parseSmartAttrs(string(ao))
			}
		}
		if sc != nil {
			// Cached maps are read-only downstream (buildSlowChecks/ smartMetricSet only read them),
			// so sharing the map reference between sc and snap is safe: no deep-copy needed.
			sc.health = snap.SmartHealth
			sc.attrs = snap.SmartAttrs
			sc.lastScan = nowUnix
		}
	}
	return snap
}

// mergeSlowFields copies every field collectSlow can populate from slow onto merged, in
// place.
func mergeSlowFields(merged *Snapshot, slow Snapshot) {
	merged.Disks = slow.Disks
	merged.DiskDetail = slow.DiskDetail
	merged.Online = slow.Online
	merged.DockerAccess = slow.DockerAccess
	merged.Containers = slow.Containers
	merged.FailedUnits = slow.FailedUnits
	merged.SmartHealth = slow.SmartHealth
	merged.SmartAttrs = slow.SmartAttrs
	merged.ContainerStats = slow.ContainerStats
	merged.Units = slow.Units
	merged.CollectorHealth = slow.CollectorHealth
}

// collectSnapshot runs both tiers and merges them into one full Snapshot.
func collectSnapshot(x Exec, fs FileSource, prev *CPUStat, da dockerAccess, c *config.Config, store SampleStore, nowUnix int64) Snapshot {
	snap := collectFast(x, fs, prev)
	// nil, 0: one-shot callers (boot report, pollLoop) always want a fresh
	// SMART scan rather than sharing/throttling against the sampler loop's cache.
	slow := collectSlow(x, fs, da, c, store, nowUnix, nil, 0)
	mergeSlowFields(&snap, slow)
	return snap
}

// eventToAlert converts an anomaly Event (the internal alert-state transition) into the
// channel-agnostic Alert the Dispatcher understands.
func eventToAlert(e Event, nowUnix int64) Alert {
	sev := SevWarning
	if e.Critical {
		sev = SevCritical
	}
	return Alert{
		Key:      e.Key,
		Title:    html.EscapeString(e.Text),
		Body:     "",
		Severity: sev,
		Kind:     e.Kind,
		Source:   "anomaly",
		Time:     nowUnix,
	}
}

// dispatcherTimeout bounds how long the Dispatcher gives each channel to
// deliver a single Alert before treating it as timed out (see Dispatcher.Dispatch).
const dispatcherTimeout = 15 * time.Second

// alertLogRetention bounds how long AlertEvents are kept in the alert log (see
// PruneAlertLog).
const alertLogRetention = 30 * 24 * time.Hour

// storeMaintenanceInterval throttles the fsync-heavy store maintenance (Downsample +
// Prune).
const storeMaintenanceInterval = 15 * time.Minute

// alertRoute, when set, decides whether an Alert reaching enqueueAndLog is delivered
// locally (true) or held back because the fleet master is expected to deliver it instead.
var alertRoute atomic.Pointer[func(Alert) bool]

// setAlertRoute installs f (nil clears it) as alertRoute and returns a restore func that
// puts back whatever was installed before.
func setAlertRoute(f func(Alert) bool) (restore func()) {
	var prev *func(Alert) bool
	if f == nil {
		prev = alertRoute.Swap(nil)
	} else {
		prev = alertRoute.Swap(&f)
	}
	return func() { alertRoute.Store(prev) }
}

// reloadOnHUP is the SIGHUP handler's logic, split out so it is testable: read cfgPath and
// apply it via reload.
func reloadOnHUP(cfgPath string, reload func(*config.Config) error) (msg string, err error) {
	c, loadErr := config.Load(cfgPath)
	if loadErr != nil {
		return "", nil
	}
	if err := reload(c); err != nil {
		return "", err
	}
	return "config reloaded", nil
}

// enqueueAndLog records the alert to the AlertLog and the live event bus, then hands
// delivery to the async notifier queue.
func enqueueAndLog(alog *AlertLog, bus *eventBus, q *NotifierQueue, a Alert, quiet bool) {
	deliverLocally := true
	if route := alertRoute.Load(); route != nil {
		deliverLocally = (*route)(a)
	}
	if alog != nil {
		_ = alog.AppendAlertEvent(AlertEvent{
			Time:           a.Time,
			Key:            a.Key,
			Title:          a.Title,
			Severity:       a.Severity.String(),
			Kind:           a.Kind,
			Source:         a.Source,
			RoutedToMaster: !deliverLocally,
			FiredAt:        a.Time,
		})
	}
	bus.Publish(core.Event{
		Kind:     alertEventKind(a),
		Severity: a.Severity.String(),
		Source:   a.Source,
		Title:    a.Title,
		Time:     a.Time,
	})
	if deliverLocally {
		q.Enqueue(a, quiet)
	}
}

// deliverSyncAndLog is enqueueAndLog's synchronous twin, used ONLY as the fleet master's
// alerting-engine delivery hook (fleetDeps.alert).
func deliverSyncAndLog(alog *AlertLog, bus *eventBus, q *NotifierQueue, a Alert, quiet bool) bool {
	if alog != nil {
		_ = alog.AppendAlertEvent(AlertEvent{
			Time: a.Time, Key: a.Key, Title: a.Title, Severity: a.Severity.String(),
			Kind: a.Kind, Source: a.Source, FiredAt: a.Time,
		})
	}
	bus.Publish(core.Event{
		Kind: alertEventKind(a), Severity: a.Severity.String(), Source: a.Source, Title: a.Title, Time: a.Time,
	})
	d := q.disp.Load()
	if d == nil {
		return false
	}
	for _, r := range d.Dispatch(a, quiet) {
		if r.Err == nil {
			return true
		}
	}
	return false
}

// deliverSyncAndLogTo is deliverSyncAndLog narrowed to a specific channel-name subset:
// identical alert-log/live-bus recording.
func deliverSyncAndLogTo(alog *AlertLog, bus *eventBus, q *NotifierQueue, a Alert, quiet bool, channels []string) bool {
	if alog != nil {
		_ = alog.AppendAlertEvent(AlertEvent{
			Time: a.Time, Key: a.Key, Title: a.Title, Severity: a.Severity.String(),
			Kind: a.Kind, Source: a.Source, FiredAt: a.Time,
		})
	}
	bus.Publish(core.Event{
		Kind: alertEventKind(a), Severity: a.Severity.String(), Source: a.Source, Title: a.Title, Time: a.Time,
	})
	d := q.disp.Load()
	if d == nil {
		return false
	}
	for _, r := range d.DispatchTo(a, quiet, channels) {
		if r.Err == nil {
			return true
		}
	}
	return false
}

// dispatchOnlyTo dispatches to a channel-name subset WITHOUT any alert-log/live-bus
// recording.
func dispatchOnlyTo(q *NotifierQueue, a Alert, quiet bool, channels []string) bool {
	d := q.disp.Load()
	if d == nil {
		return false
	}
	for _, r := range d.DispatchTo(a, quiet, channels) {
		if r.Err == nil {
			return true
		}
	}
	return false
}

// alertEventKind maps a dispatched Alert onto the Kind string its core.Event carries on the
// live event bus.
func alertEventKind(a Alert) string {
	if a.Source != "anomaly" {
		return a.Kind
	}
	switch a.Kind {
	case "fire":
		return "alert_fire"
	case "recover":
		return "alert_recover"
	default:
		return a.Kind
	}
}

func cmdDaemon(args []string) int {
	if e2eCrashOnStart == "1" {
		fmt.Fprintln(stderr, "e2e crash")
		return 1
	}
	// Never start empty next to an unmigrated serverwatch install: that would silently begin a
	// fresh history.
	if err := legacyInstallGuard(defaultMigrationPaths()); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// pidfile for SIGHUP reload
	_ = os.MkdirAll(stateDir, 0o755)
	_ = os.WriteFile(pidFile(), []byte(strconv.Itoa(os.Getpid())), 0o644)

	x := osExec{}
	fs := osFS{}
	clock := realClock{}
	st := NewStore(stateDir, clock)

	// daemonCtx bounds the lifetime of the daemon's background goroutines (the notifier worker
	// and the watchdog); the SIGTERM/SIGINT handler below cancels it.
	daemonCtx, daemonCancel := context.WithCancel(context.Background())
	defer daemonCancel()

	// config with hot reload
	var mu sync.RWMutex
	// config.Load returns (nil, err) on unreadable/corrupt JSON.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "config load failed, using defaults:", err)
		cfg = config.Default()
	}
	// Apply the operator-tuned per-command hang-breaker before ANY external collector runs
	// (probeDocker below and the slow-collector goroutine).
	if cfg.ExecTimeout > 0 {
		execTimeout = time.Duration(cfg.ExecTimeout) * time.Second
	}
	// SampleStore (raw appends + downsample + prune), opened once here at startup: this is now
	// the SOLE writer of samples/downtime events.
	store, err := openConfiguredStore(cfg)
	if err != nil {
		// Not fatal: degrade to store-writes-disabled.
		fmt.Fprintln(stderr, "sample store open failed, store writes disabled:", err)
		store = nil
	}
	if store != nil {
		defer store.Close()
	}
	// storeWriter funnels EVERY SampleStore write through one goroutine, off the sampler loop.
	var sw *storeWriter
	if store != nil {
		sw = newStoreWriter(store, 8)
		go sw.run(daemonCtx)
	}
	// NOTE: the store is opened once here and is NOT re-opened on a SIGHUP config reload below
	// -- if storage.backend/retention changes on reload.
	if migrateTelegramChannel(cfg) {
		_ = saveDaemonCfg(cfg)
	}
	// dispatcher fans outbound alerts (anomalies, boot report, digests) out to every
	// configured Channel.
	dispatcher := NewDispatcher(channelsFromConfig(cfg), dispatcherTimeout)
	// q is the async notifier queue: Enqueue is non-blocking, delivery happens on its own
	// worker goroutine, so a slow/hung Telegram uplink can never stall the sampler loop.
	q := NewNotifierQueue(dispatcher, 64)
	go q.Run(daemonCtx)
	// applyConfig swaps the shared cfg pointer and rebuilds the dispatcher under mu (mirroring
	// setChatID's pointer-swap pattern below).
	applyConfig := func(c *config.Config) {
		nd := NewDispatcher(channelsFromConfig(c), dispatcherTimeout)
		mu.Lock()
		cfg = c
		dispatcher = nd
		mu.Unlock()
		// Propagate the rebuilt dispatcher to the async notifier worker so a channel-set change
		// on reload takes effect for queued/future alerts.
		q.SetDispatcher(nd)
	}
	// SIGTERM/SIGINT: write the clean-stop marker (so the NEXT boot's downtime reconstruction
	// knows this stop was intentional, not a crash/power loss).
	var fleetStop atomic.Pointer[func()]
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-term
		_ = writeCleanStop(st.CleanStopPath(), clock.Now())
		daemonCancel()
		if stop := fleetStop.Load(); stop != nil {
			(*stop)()
		}
		os.Exit(0)
	}()
	getCfg := func() *config.Config { mu.RLock(); defer mu.RUnlock(); return cfg }
	// managedRef holds this child's managedChild once startFleet/startChild has built one (nil
	// forever on solo/master, and briefly nil on a child too, until the Store below runs).
	var managedRef atomic.Pointer[managedChild]
	// reload persists newCfg to disk then applies it in-process: the closure newInprocAPI's
	// ApplyConfig exposes to the control socket.
	reload := func(newCfg *config.Config) error {
		reimposeManagedValues(managedRef.Load(), newCfg)
		if err := saveDaemonCfg(newCfg); err != nil {
			return err
		}
		applyConfig(newCfg)
		return nil
	}
	// SIGHUP: re-read cfgPath and apply it via reload, not applyConfig directly: an external
	// edit can diverge a managed key from its committed value.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, sighup)
	go func() {
		for range hup {
			msg, err := reloadOnHUP(cfgPath, reload)
			if err != nil {
				fmt.Fprintf(stderr, "config reload failed: %v\n", err)
				continue
			}
			if msg != "" {
				fmt.Fprintln(stdout, msg)
			}
		}
	}()
	// setChatID race-safely records an auto-captured chat id by pointer-SWAPPING the shared
	// cfg (mirroring the SIGHUP reload above).
	setChatID := func(id string) {
		mu.Lock()
		nc := *withChatID(cfg, id)
		// Saved (which overlays the on-disk fleet keys onto nc, so a `fleet join` made since
		// start is never wiped) BEFORE the swap.
		_ = saveDaemonCfg(&nc)
		cfg = &nc // swap pointer; existing readers keep old struct
		nd := NewDispatcher(channelsFromConfig(cfg), dispatcherTimeout)
		dispatcher = nd
		mu.Unlock()
		// Propagate the rebuilt dispatcher to the async notifier worker, mirroring applyConfig:
		// delivery runs off the queue's own dispatcher pointer.
		q.SetDispatcher(nd)
	}

	// state
	baseline := NewBaseline()
	baseline.LoadFrom(st.BaselinePath(), fs)
	alerts := LoadAlertState(st.AlertStatePath(), fs)
	alog := NewAlertLog(st.AlertLogPath())
	var net NetTracker
	// prevCPU belongs solely to the sampler goroutine (this function).
	var prevCPU CPUStat
	// netRate/procCPU/smartState belong solely to the SLOW-COLLECTOR goroutine (started
	// below), which is the only goroutine that runs collectSlow + NetRates + Processes now.
	var netRate NetRateCalc
	// procCPU accumulates the previous per-pid jiffies sample across slow cycles
	// so collectProcesses can diff cumulative CPU jiffies into a per-process CPU%.
	var procCPU ProcCPUCalc
	// smartState persists the last SMART scan across slow cycles so collectSlow can throttle
	// smartctl calls to c.SmartIntervalSec() instead of scanning every cycle.
	var smartState smartCache
	// collHealth/prevSlow make slow collection fail-visible (#110): collHealth tracks
	// per-collector consecutive failures across cycles.
	collHealth := newCollectorHealth()
	var prevSlow Snapshot
	da := probeDocker(x, fs)

	cfgAtStart := getCfg()

	// bus is the daemon's live in-process event fan-out (eventbus.go): enqueueAndLog publishes
	// every alert onto it.
	bus := newEventBus()

	// control socket: serves the daemon's own core.API over a unix socket under
	// RUNTIME_DIRECTORY (or /run/trinetra) for the out-of-process plugins.
	enroll := &enrollState{}
	controlAPI := newInprocAPI(latestSnapshot, getCfg, store, stateDir, reload, bus, enroll)
	// fleet: the ONE place the fleet role is honoured (fleet_daemon.go).
	fleetRT := startFleet(daemonCtx, cfgAtStart, fleetDeps{
		stateDir: stateDir, getCfg: getCfg, self: controlAPI, latestSnapshot: latestSnapshot,
		store: store, alog: alog, alertStatePath: st.AlertStatePath(),
		alert: func(a Alert) {
			enqueueAndLog(alog, bus, q, a, inQuietHours(getCfg().QuietHours, time.Now()))
		},
		deliverSync: func(a Alert) bool {
			return deliverSyncAndLog(alog, bus, q, a, inQuietHours(getCfg().QuietHours, time.Now()))
		},
		deliverSyncTo: func(a Alert, channels []string) bool {
			return deliverSyncAndLogTo(alog, bus, q, a, inQuietHours(getCfg().QuietHours, time.Now()), channels)
		},
		dispatchOnly: func(a Alert, channels []string) bool {
			return dispatchOnlyTo(q, a, inQuietHours(getCfg().QuietHours, time.Now()), channels)
		},
		alertFallback: func(a Alert, silences *pushedSilences, prefix string) {
			deliverFallback(silences, alog, bus, q, a, inQuietHours(getCfg().QuietHours, time.Now()), time.Now().Unix(), prefix)
		},
		logf: func(format string, args ...any) { fmt.Fprintf(stderr, format+"\n", args...) },
	})
	fleetStop.Store(&fleetRT.stop)
	defer fleetRT.stop()
	// managedRef is now live for reload's reimposeManagedValues above: nil on solo/master
	// (fleetRT.provider.managed is only ever set for a child, fleet_daemon.go's startChild).
	managedRef.Store(fleetRT.provider.managed)
	if fleetRT.tee != nil {
		if sw != nil {
			sw.setTee(fleetRT.tee)
		}
		alog.SetTee(fleetRT.tee.Alert)
	}
	statusRT := newStatusPageRuntime(statusPageDir(stateDir),
		func() string { return fleetRT.provider.role },
		getCfg,
		nil, // the daemon ticks via TickWith, passing inputs from the sampler loop
		func(text string, channels []string) {
			go func() {
				if !dispatchOnlyTo(q, Alert{Key: "status-page", Title: text, Severity: SevWarning, Kind: "fire", Source: "status-page", Time: time.Now().Unix()},
					inQuietHours(getCfg().QuietHours, time.Now()), channels) {
					fmt.Fprintf(stderr, "status page: echo to %s not delivered (filtered by routes/quiet hours, or every channel failed)\n", strings.Join(channels, ","))
				}
			}()
		},
		func(f string, a ...any) { fmt.Fprintf(stderr, f+"\n", a...) })
	stopControl, socketPath, token, err := serveControlSocket(&fleetAwareAPI{API: controlAPI, fleetProvider: fleetRT.provider, status: statusRT})
	if err != nil {
		fmt.Fprintln(stderr, "control socket: failed to start, continuing without it:", err)
	} else {
		defer stopControl()
		// web UI: when enabled, supervise the trinetra-web plugin as a verified child process (it
		// dials the control socket above).
		if shouldStartWeb(cfgAtStart, true) {
			stopWeb := startWeb(socketPath, token)
			defer stopWeb()
		}
	}

	// Liveness handoff shared with the slow-collector and watchdog goroutines. hub carries the
	// last-good slow Snapshot (atomic, versioned).
	hub := &slowHub{}
	var lastTick, lastSlowSuccess atomic.Int64
	now0 := clock.Now().Unix()
	lastTick.Store(now0)
	lastSlowSuccess.Store(now0) // grace: don't trip the watchdog before the first slow cycle

	// slow-collector: runs collectSlow + NetRates + Processes off the sampler loop, bounded by
	// an overall deadline.
	collector := &slowCollector{
		hub:             hub,
		lastSlowSuccess: &lastSlowSuccess,
		now:             func() int64 { return clock.Now().Unix() },
		deadlineSec: func() int64 {
			d := getCfg().SampleInterval
			if d > 60 || d <= 0 {
				d = 60
			}
			return int64(d)
		},
		collect: func() Snapshot {
			c := getCfg()
			n := clock.Now().Unix()
			s := collectSlow(x, fs, da, c, store, n, &smartState, c.SmartIntervalSec())
			// net throughput + processes: stateful, owned here, and excluded from mergeSlowFields,
			// so they are populated directly onto the published snapshot.
			s.NetRates = nil
			if c.NetThroughputEnabled() {
				if b, err := fs.Read("/proc/net/dev"); err == nil {
					s.NetRates = netRate.Rates(parseNetDev(string(b)), n)
				}
			}
			s.Processes = ProcSnapshot{}
			if c.ProcessesEnabled() {
				s.Processes = collectProcesses(fs, &procCPU, os.Getpagesize()/1024)
			}
			// Fail-visible collection (#110): update per-collector health from this cycle's outcome,
			// carry last-known values forward for any collector that errored.
			collHealth.observe(s.collectorsAttempted, s.CollectorErrors, n)
			carryForwardFailedCollectors(&prevSlow, &s)
			s.CollectorHealth = collHealth.snapshot()
			prevSlow = s
			return s
		},
	}
	go func() {
		collector.runOnce()
		ticker := time.NewTicker(time.Duration(getCfg().SampleInterval) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			collector.runOnce()
		}
	}()

	// healthchecks-pinger goroutine: the external healthchecks.io dead-man ping stays on the
	// slow cadence but off the sampler loop -- httpPing is bounded at 10s.
	go func() {
		for {
			c := getCfg()
			interval := c.SampleInterval
			if interval <= 0 {
				interval = 60
			}
			block := c.Notify.BlockPrivateTargets
			pingHealthchecks(c.Healthchecks.URL, func(u string) error { return httpPingGuarded(u, block) })
			time.Sleep(time.Duration(interval) * time.Second)
		}
	}()

	// watchdog goroutine: the SOLE sender of the systemd WATCHDOG=1 ping now (removed from the
	// sampler loop).
	go runWatchdog(daemonCtx,
		livenessGate{lastTick: &lastTick, lastSlowSuccess: &lastSlowSuccess, samplerStaleSec: 30, collectorStaleSec: 300},
		watchdogPeriodSec(), func() int64 { return clock.Now().Unix() },
		func() error { return sdNotify("WATCHDOG=1") })

	// boot/recovery report from heartbeat gap, deduped against a previous start the same gap,
	// recomputed from an unchanged heartbeat on a restart, reports only once.
	c0 := getCfg()
	cleanStopPath := st.CleanStopPath()
	// bootReportCh carries the boot/recovery report from its background collector goroutine
	// (below) to the sampler loop, which delivers it alongside its other alerts.
	bootReportCh := make(chan Alert, 1)
	if last, ok := readHeartbeat(st.HeartbeatPath(), fs); ok {
		if _, stopped := readCleanStop(cleanStopPath, fs); !stopped {
			hostBoot, hostBootOK := hostBootTime(fs)
			if ev, ok := reconstructPowerDown(last, clock.Now(), hostBoot, hostBootOK, time.Duration(c0.HeartbeatInterval)*time.Second); ok {
				lastRep, _ := readCleanStop(st.LastReportedDowntimePath(), fs) // reuse int-file helpers
				if shouldReportDowntime(ev, lastRep) {
					if store != nil {
						_ = store.AppendEvent(ev)
					}
					_ = writeCleanStop(st.LastReportedDowntimePath(), time.Unix(ev.End, 0))
					// Collect the report's status snapshot on a goroutine and hand the finished Alert back
					// to the sampler loop. collectSnapshot does an uncached full SMART scan.
					go func() {
						var bootCPU CPUStat
						now := clock.Now().Unix()
						snap := collectSnapshot(x, fs, &bootCPU, da, c0, store, now)
						bootReportCh <- Alert{
							Title:    formatBootReport([]DownEvent{ev}, renderStatus(snap, c0)),
							Severity: SevInfo,
							Kind:     "fire",
							Source:   "boot",
							Time:     now,
						}
					}()
				}
			}
		}
	}
	// clear any stale clean-stop marker now that we've started, so an unclean
	// stop after this point IS reported on the next boot.
	_ = os.Remove(cleanStopPath)

	// self-update: start the background loop that checks for a new release on the configured
	// cadence and turns update.State transitions into Alerts (update_daemon.go).
	go startUpdateLoop(daemonCtx, getCfg, newUpdater(getCfg()), func(a Alert) {
		enqueueAndLog(alog, bus, q, a, false)
	})

	// telegram long-poller (owns its own prevCPU internally)
	go pollLoop(getCfg, setChatID, store, x, fs, da, enroll, fleetRT.provider.Fleet())

	// sampler loop: ticks at fast_interval and does NO blocking subprocess or network I/O --
	// that all lives on the slow-collector / healthchecks / notifier goroutines now.
	var lastDaily, lastWeekly time.Time
	var lastStoreMaint time.Time // throttles the fsync-heavy Downsample+Prune
	fastTicker := time.NewTicker(time.Duration(getCfg().FastInterval) * time.Second)
	defer fastTicker.Stop()
	lastFast := getCfg().FastInterval
	var merged Snapshot
	var lastHeartbeat int64 // unix seconds; 0 sentinel forces an immediate first heartbeat
	var lastSlowVer uint64  // slow-hub version whose slow checks were last evaluated
	for {
		// Deliver the boot/recovery report once its background collector has finished (see
		// bootReportCh above), in line with the loop's other alerts.
		select {
		case a := <-bootReportCh:
			enqueueAndLog(alog, bus, q, a, false) // reports bypass quiet hours
		default:
		}
		c := getCfg()
		if c.FastInterval != lastFast {
			fastTicker.Reset(time.Duration(c.FastInterval) * time.Second)
			lastFast = c.FastInterval
		}
		now := clock.Now()

		fast := collectFast(x, fs, &prevCPU)
		merged.CPU, merged.MemPct, merged.SwapPct, merged.Load1, merged.Load5, merged.Load15, merged.TempC =
			fast.CPU, fast.MemPct, fast.SwapPct, fast.Load1, fast.Load5, fast.Load15, fast.TempC

		// Read the last-good slow snapshot from the collector goroutine and merge it onto merged
		// every tick, so status.json/anomaly eval always see a full.
		s, ver, haveSlow := hub.latest()
		newSlow := false
		if haveSlow {
			mergeSlowFields(&merged, s)
			merged.NetRates = s.NetRates
			merged.Processes = s.Processes
			newSlow = ver != lastSlowVer
			merged.SlowStale = !newSlow
			lastSlowVer = ver
		} else {
			merged.SlowStale = true // no slow snapshot yet
		}

		merged.TS = now.Unix()
		// Publish a COPY of merged into snapshotHub for lock-free readers (latestSnapshot,
		// ultimately the control socket's Snapshot handler).
		snap := merged
		snapshotHub.Store(&snap)
		// Tell any live control-socket subscribers a fresh Snapshot is ready, without pushing the
		// Snapshot itself onto the bus: a subscriber.
		bus.Publish(core.Event{Kind: "snapshot", Time: now.Unix()})

		// Liveness: stamp this sampler tick for the watchdog goroutine, which is now the SOLE
		// sender of the systemd WATCHDOG=1 ping.
		lastTick.Store(now.Unix())

		// heartbeat has its own cadence (HeartbeatInterval), independent of fast/slow: it exists
		// only so a future boot can measure how long the process was gone.
		if shouldHeartbeat(lastHeartbeat, now.Unix(), c.HeartbeatInterval) {
			_ = writeHeartbeat(st.HeartbeatPath(), now)
			lastHeartbeat = now.Unix()
		}
		_ = st.WriteStatus(merged) // every fast tick: status.json is the live view

		// SampleStore write path: every fast tick appends the cheap fast-tier metrics as raw
		// samples.
		var netEvents []DownEvent
		if newSlow {
			if ev, closed := net.Update(merged.Online, now.Unix()); closed {
				netEvents = append(netEvents, ev)
			}
		}

		// Assemble this cycle's SampleStore batch and hand it to the off-thread storeWriter
		// (storewriter.go).
		if sw != nil {
			sets := []MetricSet{fastMetricSet(merged)}
			if newSlow {
				sets = append(sets, slowMetricSet(merged))
				if cm := containerMetricSet(merged); len(cm) > 0 {
					sets = append(sets, cm)
				}
				// nm is empty on the very first slow cycle (netRate has no prior
				// sample yet) and whenever collect.net_throughput is disabled.
				if nm := netRateMetricSet(merged.NetRates); len(nm) > 0 {
					sets = append(sets, nm)
				}
				// sm is empty whenever no discovered SMART device reported a
				// parseable temperature attribute this cycle.
				if sm := smartMetricSet(merged); len(sm) > 0 {
					sets = append(sets, sm)
				}
			}
			// Run the fsync-heavy Downsample+Prune only every storeMaintenanceInterval, not every
			// slow tick.
			doMaint := newSlow && now.Sub(lastStoreMaint) >= storeMaintenanceInterval
			if doMaint {
				lastStoreMaint = now
			}
			sw.submit(storeWrite{ts: merged.TS, sets: sets, events: netEvents, maintain: doMaint, maintainNow: now.Unix()})
		}

		// anomalies: each channel's own Route (severity/kind filters, CriticalOverridesQuiet) now
		// decides delivery.
		quiet := inQuietHours(c.QuietHours, now)
		events := alerts.Evaluate(buildFastChecks(merged, c), baseline, c.BaselineSigma, c.BaselineMinPct, c.BaselineAlerts, now.Unix())
		for _, e := range events {
			enqueueAndLog(alog, bus, q, eventToAlert(e, now.Unix()), quiet)
		}
		stateChanged := len(events) > 0
		if newSlow {
			slowEvents := alerts.Evaluate(buildSlowChecks(merged, c, alerts.Active), baseline, c.BaselineSigma, c.BaselineMinPct, c.BaselineAlerts, now.Unix())
			for _, e := range slowEvents {
				enqueueAndLog(alog, bus, q, eventToAlert(e, now.Unix()), quiet)
			}
			stateChanged = stateChanged || len(slowEvents) > 0
		}
		// scheduled digests bypass quiet hours, like the boot report.
		if matchDaily(c.Schedule.Daily, now, lastDaily) {
			lastDaily = now
			enqueueAndLog(alog, bus, q, Alert{Title: digestNow(store, now, 1, "📊 daily digest", configuredRawRetention(c)), Severity: SevInfo, Kind: "fire", Source: "digest", Time: now.Unix()}, false)
		}
		if matchWeekly(c.Schedule.Weekly, now, lastWeekly) {
			lastWeekly = now
			enqueueAndLog(alog, bus, q, Alert{Title: digestNow(store, now, 7, "📆 weekly rollup", configuredRawRetention(c)), Severity: SevInfo, Kind: "fire", Source: "digest", Time: now.Unix()}, false)
		}
		// alerts.json only changes on a fire/recover transition; baseline.json's stats change
		// every fast tick in memory but only need to hit disk at the slow cadence.
		if stateChanged {
			// Pull any CLI-written ack flags back onto the in-memory state before saving.
			alerts.MergeAckFromDisk(st.AlertStatePath(), fs)
			_ = alerts.Save(st.AlertStatePath())
		}
		if newSlow {
			// baseline.Save and PruneAlertLog do not fsync (page-cache writes), so they stay on the
			// sampler.
			_ = baseline.Save(st.BaselinePath())
			_ = alog.PruneAlertLog(now.Add(-alertLogRetention).Unix())
			// A child's handoff-receipts sidecar (fleet_lease.go) needs the same periodic pruning as
			// the alert log, not only the one-shot prune startChild does at reconciliation time.
			if fleetRT.provider.role == config.RoleChild {
				fallbackAfter := getCfg().FleetFallbackAfter()
				_ = pruneHandoffReceipts(handoffReceiptsPath(stateDir), now.Add(-10*fallbackAfter).Unix())
			} else {
				// Public status page (#157): masters and solo hosts evaluate; a child never serves one.
				var in statusInputs
				if fleetRT.provider.master != nil {
					in = gatherMaster(fleetRT.provider.master, alerts.Active, merged, now)
				} else {
					in = gatherStandalone(alerts.Active, merged)
				}
				statusRT.TickWith(now, in)
			}
		}

		<-fastTicker.C
	}
}

// configuredRawRetention returns cfg.Storage.RawRetention parsed as a Duration, falling
// back to defaultRawRetention when unset/unparseable.
func configuredRawRetention(cfg *config.Config) time.Duration {
	d, err := time.ParseDuration(cfg.Storage.RawRetention)
	if err != nil || d <= 0 {
		return defaultRawRetention
	}
	return d
}

// digestNow builds a digest by querying store's cpu/mem series and downtime events over the
// window [now-days*24h, now]. rawRetention.
func digestNow(store SampleStore, now time.Time, days int, title string, rawRetention time.Duration) string {
	since := now.AddDate(0, 0, -days).Unix()
	nowUnix := now.Unix()
	var window string
	switch days {
	case 1:
		window = "24h"
	case 7:
		window = "7d"
	default:
		window = fmt.Sprintf("%dd", days)
	}
	if store == nil {
		return buildDigest(title, window, 0, 0, 0, nil)
	}
	// Peaks use the coarsest resolution that still covers the window (raw for recent windows
	// → finer recent peaks; 1m for windows older than raw retention).
	res := PickResolution(since, nowUnix, nowUnix, rawRetention)
	cpuPts, _ := store.Query("cpu", since, nowUnix, res)
	memPts, _ := store.Query("mem", since, nowUnix, res)
	downs, _ := store.Events(since, nowUnix)
	var peakCPU, peakMem float64
	for _, p := range cpuPts {
		if p.Max > peakCPU {
			peakCPU = p.Max
		}
	}
	for _, p := range memPts {
		if p.Max > peakMem {
			peakMem = p.Max
		}
	}
	// Sample count is always taken at 1m resolution so the reported "samples: N" is
	// cadence-stable: querying peaks at RAW.
	countPts, _ := store.Query("cpu", since, nowUnix, Res1m)
	return buildDigest(title, window, peakCPU, peakMem, len(countPts), downs)
}

func pollLoop(getCfg func() *config.Config, setChatID func(string), store SampleStore, x Exec, fs FileSource, da dockerAccess, enroll *enrollState, fleetAPI core.FleetAPI) {
	// The poller owns its CPUStat; it is never shared with the sampler goroutine.
	offset := 0
	var prevCPU CPUStat
	announced := false
	for {
		c := getCfg()
		// GetUpdates needs only a token; gate on token so we can still learn
		// the owner chat id via enrollment once a token is configured.
		if c.Telegram.Token == "" {
			time.Sleep(5 * time.Second)
			continue
		}
		// The enrollment PIN for the zero-config setup path (#78): an unclaimed bot is claimed
		// only by "/start <pin>", not by whoever messages first. enroll.
		pin, _ := enroll.PIN(c)
		if c.Telegram.ChatID == "" && !announced {
			fmt.Fprintf(stderr, "telegram: bot not yet enrolled. From your Telegram account, message the bot: /start %s\n", pin)
			announced = true
		}
		tg := telegram.New(c.Telegram.Token, c.Telegram.ChatID)
		ups, err := tg.GetUpdates(offset, 50)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		// reply collects a snapshot and answers a single authorized command.
		reply := func(cc *config.Config, u telegram.Update) {
			nowUnix := time.Now().Unix()
			snap := collectSnapshot(x, fs, &prevCPU, da, cc, store, nowUnix)
			snap.TS = nowUnix
			client := telegram.New(cc.Telegram.Token, cc.Telegram.ChatID)
			if err := client.SendMessage(handleCommand(u.Text, store, snap, cc)); err != nil {
				fmt.Fprintln(stderr, "telegram send:", err)
			}
		}
		// setChatID's reload already tells the daemon it's enrolled; wrap it to also clear
		// enroll's cached pin on success, so a later re-enrollment.
		onEnroll := func(id string) {
			setChatID(id)
			enroll.Reset()
		}
		// onCallback answers a button tap: unlike reply, it runs for EVERY callback_query
		// processUpdates sees, authorized or not, since Telegram requires an answer either way.
		onCallback := func(cc *config.Config, u telegram.Update) {
			text := telegramCallbackAnswer(fleetAPI, cc.Telegram.ChatID, u, time.Now)
			client := telegram.New(cc.Telegram.Token, cc.Telegram.ChatID)
			if err := client.AnswerCallbackQuery(context.Background(), u.CallbackID, text); err != nil {
				fmt.Fprintln(stderr, "telegram answer callback:", err)
			}
		}
		offset, c = processUpdates(ups, offset, c, enroll, time.Now, getCfg, onEnroll, reply, onCallback)
	}
}

// processUpdates handles one batch of inbound Telegram updates.
func processUpdates(ups []telegram.Update, offset int, c *config.Config, enroll *enrollState, now func() time.Time, getCfg func() *config.Config, setChatID func(string), reply func(*config.Config, telegram.Update), onCallback func(*config.Config, telegram.Update)) (int, *config.Config) {
	for _, u := range ups {
		offset = u.UpdateID + 1
		if u.CallbackID != "" {
			// A button tap is never a text command and never participates in enrollment: it carries
			// its own authorization check.
			if onCallback != nil {
				onCallback(c, u)
			}
			continue
		}
		if c.Telegram.ChatID == "" {
			// Unclaimed: ownership is granted ONLY by a correct "/start <pin>" (#78 Scenario A).
			if u.ChatID != "" && enroll.Attempt(c, u.Text, now()) {
				setChatID(u.ChatID)
				c = getCfg()
				reply(c, u)
			}
			continue
		}
		// Claimed: only ever act on the owner chat.
		if u.ChatID != c.Telegram.ChatID {
			continue
		}
		reply(c, u)
	}
	return offset, c
}

// enrollMatch reports whether text is exactly "/start <pin>" for a non-empty pin.
func enrollMatch(text, pin string) bool {
	if pin == "" {
		return false
	}
	f := strings.Fields(text)
	return len(f) == 2 && f[0] == "/start" && f[1] == pin
}

// newEnrollPIN returns a fresh 6-digit enrollment PIN from crypto/rand.
func newEnrollPIN() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%06d", n.Int64())
}
