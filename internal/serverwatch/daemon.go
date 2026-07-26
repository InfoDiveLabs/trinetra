package serverwatch

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/telegram"
)

// buildFastChecks builds anomaly Checks for the cheap, high-frequency
// metrics (cpu/mem/swap/temp) that collectFast populates every fast tick.
// These are safe to evaluate on every fast_interval tick.
func buildFastChecks(snap Snapshot, c *config.Config) []Check {
	var checks []Check
	fastInterval := c.FastInterval
	add := func(key string, val, thr float64, hasThr, crit bool) {
		if !c.TargetEnabled(key) {
			return
		}
		if o, ok := c.TargetThreshold(key); ok {
			thr, hasThr = o, true
		}
		checks = append(checks, Check{Key: key, Value: val, Threshold: thr, HasThreshold: hasThr, Critical: crit, Interval: fastInterval})
	}
	add("cpu", snap.CPU, c.Thresholds.CPUPct, true, false)
	add("mem", snap.MemPct, c.Thresholds.MemPct, true, false)
	add("swap", snap.SwapPct, c.Thresholds.SwapPct, true, false)
	if snap.TempC > 0 {
		add("temp", snap.TempC, c.Thresholds.TempC, true, false)
	}
	return checks
}

// binaryCheck builds a Check for a binary health state (docker/service/
// smart: either "ok" or "bad", never a graduated numeric reading), so
// fire/recover text can be human wording (e.g. "container web is down
// (exited)") instead of the generic "<key> = 1.0 ≥ threshold 1.0" numeric
// format. Kept as one helper so docker/service/smart wording stays DRY
// across the primary loops and the recovery-sweep sites below.
func binaryCheck(key string, bad bool, fireMsg, recoverMsg string, interval int) Check {
	v := 0.0
	if bad {
		v = 1
	}
	return Check{Key: key, Value: v, Threshold: 1, HasThreshold: true, Critical: true,
		Interval: interval, FireMsg: fireMsg, RecoverMsg: recoverMsg}
}

// buildSlowChecks builds anomaly Checks for the expensive metrics
// (disk/docker/service/smart) that collectSlow only refreshes on slow
// ticks. active is the current AlertState.Active map: it drives the
// service/docker/smart recovery sweeps below, which must fire a value=0
// check for any active alert whose target has disappeared from the
// snapshot entirely (removed container, vanished device, recovered
// systemd unit), since the loop only calls this on slow ticks and an
// alert must not stay stuck active between them.
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
	// systemd: only failed units appear in the list. Emit value=1 for each,
	// AND value=0 for any active service:* alert no longer failed (so it recovers).
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
	// docker/smart recovery sweep: if a container is REMOVED (not just stopped)
	// or `docker ps`/`smartctl --scan` starts erroring, the target disappears
	// from the snapshot maps entirely, so no check is emitted above and an
	// active alert would stay stuck forever. Mirror the service: sweep and emit
	// value=0 for any active docker:/smart: alert whose target is gone.
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
	return checks
}

// buildChecks returns the full fast+slow check set. It exists for callers
// that want a single, complete evaluation (tests comparing against the
// per-tier split); the sampler loop in cmdDaemon calls buildFastChecks and
// buildSlowChecks directly so it can run the slow half only on slow ticks.
func buildChecks(snap Snapshot, c *config.Config, active map[string]ActiveAlert) []Check {
	checks := buildFastChecks(snap, c)
	checks = append(checks, buildSlowChecks(snap, c, active)...)
	return checks
}

// fastMetricSet converts the fast-tier fields of a Snapshot into the
// MetricSet a SampleStore Append expects, for the write-path counterpart of
// buildFastChecks: every fast tick appends these as raw samples. temp is
// omitted when TempC<=0 (no thermal zone discovered), mirroring
// buildFastChecks' own "if snap.TempC > 0" gate.
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

// slowMetricSet converts the slow-tier fields of a Snapshot into a
// MetricSet, for the write-path counterpart of buildSlowChecks: one
// "disk:<mount>" metric per discovered mount. Scope is deliberately narrower
// than buildSlowChecks: docker/service/smart are binary health states
// already handled by the alert system, not numeric series worth storing in
// the SampleStore, so only disk usage percentages are appended here.
func slowMetricSet(s Snapshot) MetricSet {
	ms := make(MetricSet, len(s.Disks))
	for mount, pct := range s.Disks {
		ms["disk:"+mount] = pct
	}
	return ms
}

// containerMetricSet converts a Snapshot's per-container docker stats
// (s.ContainerStats, filled by collectSlow when collect.container_stats is
// enabled) into a MetricSet: one "docker:<name>:cpu" and one
// "docker:<name>:mem" entry per container. This is the write-path
// counterpart of the docker-stats collector, appended to the SampleStore
// alongside slowMetricSet on slow ticks. Net rx/tx are surfaced live via
// Snapshot.ContainerStats/status.json but deliberately NOT persisted as
// series here: one cpu + one mem series per running container is the
// cardinality this design accepts (docs/ROADMAP.md #71/#76); adding net
// series per container would double it again for comparatively low value.
func containerMetricSet(s Snapshot) MetricSet {
	ms := make(MetricSet, len(s.ContainerStats)*2)
	for name, cs := range s.ContainerStats {
		ms["docker:"+name+":cpu"] = cs.CPUPct
		ms["docker:"+name+":mem"] = cs.MemMiB
	}
	return ms
}

// netRateMetricSet converts a NetRateCalc.Rates() result into a MetricSet:
// one "net:<iface>:rx" and one "net:<iface>:tx" entry (bytes/sec) per
// interface, for the write-path counterpart of the net-throughput collector
// (opt-in via collect.net_throughput). rates is nil/empty on the first slow
// tick after startup (NetRateCalc has no prior sample to diff against yet)
// and whenever the collector is disabled, in which case this returns an
// empty MetricSet — the sampler loop's caller skips the Append entirely in
// that case, same as containerMetricSet's len()>0 guard.
func netRateMetricSet(rates map[string]IfaceRate) MetricSet {
	ms := make(MetricSet, len(rates)*2)
	for iface, r := range rates {
		ms["net:"+iface+":rx"] = r.RxBps
		ms["net:"+iface+":tx"] = r.TxBps
	}
	return ms
}

// smartMetricSet converts a Snapshot's per-device SMART attributes
// (s.SmartAttrs, filled by collectSlow for every discovered SMART device)
// into a MetricSet: one "smart:<dev>:temp" entry per device whose parsed
// Temperature_Celsius/Airflow_Temperature attribute is known (TempC>0).
// Devices with TempC==0 (attribute absent, or smartctl -A failed for that
// device this tick) are omitted rather than appending a misleading zero —
// this is the write-path counterpart of the SMART-attribute collector,
// appended to the SampleStore alongside slowMetricSet on slow ticks, mirroring
// containerMetricSet/netRateMetricSet's len()>0-guarded Append pattern.
func smartMetricSet(s Snapshot) MetricSet {
	ms := make(MetricSet, len(s.SmartAttrs))
	for dev, a := range s.SmartAttrs {
		if a.TempC > 0 {
			ms["smart:"+dev+":temp"] = float64(a.TempC)
		}
	}
	return ms
}

// shouldHeartbeat reports whether at least intervalSec seconds have passed
// since lastUnix (the last written heartbeat), given the current time
// nowUnix. lastUnix==0 (no heartbeat written yet) always returns true so the
// very first tick after startup writes one immediately. intervalSec<=0 is
// treated as "always heartbeat" rather than dividing by/blocking on a
// misconfigured interval.
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

func httpPing(url string) error {
	// A bare http.Get has no timeout: a hung healthchecks endpoint would block
	// the sampler loop indefinitely. Bound it.
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// collectFast gathers the cheap, high-frequency resources: /proc reads for
// CPU/mem/swap/load, plus the /sys thermal read (also cheap). prev is the
// caller's single-goroutine-owned CPUStat used to compute CPU% as a delta
// across ticks; it is mutated in place. collectFast never shells out, so it
// is safe to call every fast_interval tick without load on the host.
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

// smartCache holds the last SMART scan so collectSlow can reuse it between
// scans (SMART health rarely changes and smartctl --scan + -H/-A per device
// is the heaviest slow-tier call). lastScan is the unix time of the last
// real scan; zero (or nil health) means "never scanned yet" so the first
// slow tick scans.
type smartCache struct {
	lastScan int64
	health   map[string]string
	attrs    map[string]SmartAttr
}

// collectSlow gathers the expensive resources: shelling out to df/docker/
// systemctl/smartctl, plus the network connectivity dial. These are all
// either subprocess spawns or (for the connectivity check) multi-second
// network timeouts, so they run on the slow tier (sample_interval) rather
// than every fast tick. c gates the opt-in docker-stats collector
// (collect.container_stats); it may be nil (treated as disabled) so tests
// and any future one-shot caller that doesn't have a config handy still get
// a Snapshot back rather than a panic. store/nowUnix feed the disk
// fill-rate projection (DiskDetail.DaysToFull*, via projectDaysToFull over
// the last ~7d of each mount's "disk:<mount>" series); store may be nil (no
// SampleStore configured, or a one-shot caller with none handy), in which
// case every mount's projection is simply left unknown. sc/smartIntervalSec
// throttle the SMART scan (see the SMART block below): sc may be nil for
// one-shot callers that always want a fresh scan.
func collectSlow(x Exec, fs FileSource, da dockerAccess, c *config.Config, store SampleStore, nowUnix int64, sc *smartCache, smartIntervalSec int) Snapshot {
	var snap Snapshot
	snap.DockerAccess = da.method
	// snap.Disks and snap.DiskDetail are BOTH derived from the single typed
	// `df -PT -B1` call (device/fstype/usage/size/free per mount), gated by
	// isRealMount && isRealFsType. This used to be two separate df calls —
	// snap.Disks from untyped `df -PB1` (path-prefix filtering only) and
	// snap.DiskDetail from the typed call — which meant snap.Disks had no
	// fstype to filter on. On a root daemon on a real docker host, `df`
	// lists one `overlay` mount per container (plus squashfs/tmpfs/nsfs
	// pseudo-mounts), so snap.Disks silently exploded to 70+ junk entries:
	// this fed 70+ bogus "disk:<mount>" series into the tsfile store, and
	// blew the /stats,/status,/disk Telegram replies past the 4096-char
	// limit (see fix-disk-telegram-brief.md). Deriving both maps from the
	// same typed+filtered source keeps them in lockstep and closes that gap.
	// `df -Pi` (inode-used%) is a separate call since -T and -i are mutually
	// exclusive df flags; either call failing just narrows what this tick
	// can report rather than failing collectSlow.
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
	}
	snap.Online = checkOnline(defaultConnHosts, connDial)

	// docker containers (ps -a lists all, so state is always known -> recovery works)
	if da.available {
		if cs, err := da.list(x); err == nil {
			snap.Containers = map[string]string{}
			for _, ct := range cs {
				snap.Containers[ct.Name] = ct.State
			}
		}
	}
	// per-container cpu/mem/net (opt-in, docker-availability-gated): a stats
	// error (docker daemon busy, container churn mid-call, etc.) just leaves
	// ContainerStats nil for this tick rather than failing the whole
	// collectSlow pass.
	if da.available && c != nil && c.ContainerStatsEnabled() {
		if cs, err := da.stats(x); err == nil {
			snap.ContainerStats = map[string]ContainerStat{}
			for _, s := range cs {
				snap.ContainerStats[s.Name] = s
			}
		}
	}
	// failed systemd units (alerting collection — always runs, unaffected by
	// collect.services below)
	if out, err := runMaybeSudo(x, "systemctl", "--failed", "--plain", "--no-legend"); err == nil {
		snap.FailedUnits = parseFailedUnits(string(out))
	}
	// full systemd unit inventory (opt-in via collect.services, snapshot-only:
	// see UnitInfo/listUnits in discover.go for why this is never persisted
	// to the SampleStore as a series). A nil config, disabled collector, or
	// systemctl error all just leave snap.Units nil for this tick rather than
	// failing the rest of collectSlow.
	if c != nil && c.ServicesEnabled() {
		if units, err := listUnits(x); err == nil {
			snap.Units = units
		}
	}
	// SMART health for every discovered device (all queried each cycle ->
	// recovery works), throttled to smartIntervalSec: smartctl --scan/-H/-A
	// is the heaviest slow-tier call and SMART health rarely changes, so
	// between scans this reuses sc's cached results instead of shelling out.
	// sc == nil or a never-yet-populated cache always scans (one-shot
	// callers pass sc == nil so they're never throttled).
	doScan := sc == nil || sc.health == nil || nowUnix-sc.lastScan >= int64(smartIntervalSec)
	if !doScan {
		snap.SmartHealth = sc.health
		if c != nil && c.SmartAttrsEnabled() {
			snap.SmartAttrs = sc.attrs
		}
	} else if out, err := runMaybeSudo(x, "smartctl", "--scan"); err == nil {
		snap.SmartHealth = map[string]string{}
		// SMART attribute detail (temp/wear/realloc, opt-in via
		// collect.smart_attrs) is the heaviest optional per-device call: one
		// extra smartctl invocation per discovered device, every slow tick. A
		// nil config or disabled collector skips it entirely (snap.SmartAttrs
		// stays nil), leaving the cheap --scan/-H health check above
		// unaffected.
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
			// Cached maps are read-only downstream (buildSlowChecks/
			// smartMetricSet only read them), so sharing the map reference
			// between sc and snap is safe: no deep-copy needed.
			sc.health = snap.SmartHealth
			sc.attrs = snap.SmartAttrs
			sc.lastScan = nowUnix
		}
	}
	return snap
}

// mergeSlowFields copies every field collectSlow can populate from slow onto
// merged, in place. Shared by collectSnapshot's one-shot merge below and the
// sampler loop's per-slow-tick merge in cmdDaemon so the two copies can never
// drift apart: a Snapshot field collectSlow starts setting that isn't added
// here would silently vanish from status.json (and from collectSnapshot's
// one-shot callers) until this function is updated too. See
// TestMergeSlowFieldsCopiesEverySlowTierField (status_test.go) for the
// regression guard. NetRates and Processes are deliberately excluded: they
// are populated directly by the caller (sampler loop / collectSnapshot) from
// stateful calculators (NetRateCalc/ProcCPUCalc) that collectSlow itself has
// no access to, not by collectSlow.
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
}

// collectSnapshot runs both tiers and merges them into one full Snapshot.
// It exists for the two one-shot callers that need a complete picture right
// now rather than a tiered cadence: the boot/recovery report (a single
// collection before the sampler loop starts) and pollLoop (each inbound
// Telegram command wants a fresh, complete status). The tiered sampler loop
// in cmdDaemon does NOT use this: it calls collectFast/collectSlow directly
// so it can run collectSlow only every Nth fast tick.
func collectSnapshot(x Exec, fs FileSource, prev *CPUStat, da dockerAccess, c *config.Config, store SampleStore, nowUnix int64) Snapshot {
	snap := collectFast(x, fs, prev)
	// nil, 0: one-shot callers (boot report, pollLoop) always want a fresh
	// SMART scan rather than sharing/throttling against the sampler loop's cache.
	slow := collectSlow(x, fs, da, c, store, nowUnix, nil, 0)
	mergeSlowFields(&snap, slow)
	return snap
}

// slowEvery returns N, the number of fast_interval ticks between slow-tier
// (collectSlow) collections: the sampler loop runs collectSlow on every Nth
// fast tick, computed from the configured cadence. Always returns >= 1 so
// the slow tier still runs (worst case, every fast tick) rather than the
// loop dividing by zero or never collecting slow data at all when the
// config is missing, zero, or an inverted/non-multiple ratio.
func slowEvery(fastInterval, sampleInterval int) int {
	if fastInterval <= 0 {
		return 1
	}
	n := sampleInterval / fastInterval
	if n < 1 {
		return 1
	}
	return n
}

// eventToAlert converts an anomaly Event (the internal alert-state
// transition) into the channel-agnostic Alert the Dispatcher understands.
// Body is left empty: e.Text already carries the full human-readable
// message and is used verbatim as the Title.
func eventToAlert(e Event, nowUnix int64) Alert {
	sev := SevWarning
	if e.Critical {
		sev = SevCritical
	}
	return Alert{
		Key:      e.Key,
		Title:    e.Text,
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

// alertLogRetention bounds how long AlertEvents are kept in the alert log
// (see PruneAlertLog), mirroring the ~30-day retention used elsewhere for
// similar append-only histories.
const alertLogRetention = 30 * 24 * time.Hour

// dispatchAndLog calls disp.Dispatch and, best-effort, records the outcome
// as an AlertEvent in alog: this is the single choke point every alert
// dispatch in the daemon (anomaly fire/recover, boot report, digests) goes
// through so the alert log stays a complete history. alog may be nil (kept
// symmetrical with the store's nil-degrades-gracefully convention elsewhere
// in this file) in which case logging is simply skipped.
func dispatchAndLog(disp *Dispatcher, alog *AlertLog, a Alert, quiet bool) []DeliveryResult {
	results := disp.Dispatch(a, quiet)
	if alog != nil {
		_ = alog.AppendAlertEvent(AlertEvent{
			Time:      a.Time,
			Key:       a.Key,
			Title:     a.Title,
			Severity:  a.Severity.String(),
			Kind:      a.Kind,
			Source:    a.Source,
			Delivered: deliveriesFrom(results),
		})
	}
	return results
}

func cmdDaemon(args []string) int {
	// pidfile for SIGHUP reload
	_ = os.MkdirAll(stateDir, 0o755)
	_ = os.WriteFile(pidFile(), []byte(strconv.Itoa(os.Getpid())), 0o644)

	x := osExec{}
	fs := osFS{}
	clock := realClock{}
	st := NewStore(stateDir, clock)

	// config with hot reload
	var mu sync.RWMutex
	// config.Load returns (nil, err) on unreadable/corrupt JSON. Discarding the
	// error would leave cfg nil and panic on the next deref; under systemd
	// Restart=always that becomes an unrecoverable crash-loop. Fall back to
	// defaults so the daemon stays up and the config is CLI-repairable.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "config load failed, using defaults:", err)
		cfg = config.Default()
	}
	// SampleStore (raw appends + downsample + prune), opened once here at
	// startup: this is now the SOLE writer of samples/downtime events, and the
	// sole read path for history/handlers/digests below. The old JSONL Store
	// (st) above remains only for status.json, the heartbeat file, and the
	// baseline/alert-state path helpers — none of which are sample data.
	// openConfiguredStore (migrate.go) centralizes the backend/retention
	// lookup so `migrate`/`dump` open the exact same store this daemon writes
	// to.
	store, err := openConfiguredStore(cfg)
	if err != nil {
		// Not fatal: degrade to store-writes-disabled (and reads returning
		// empty via the nil-store guards in digestNow/handleCommand) rather
		// than crash-looping under systemd Restart=always.
		fmt.Fprintln(stderr, "sample store open failed, store writes disabled:", err)
		store = nil
	}
	if store != nil {
		defer store.Close()
	}
	// NOTE: the store is opened once here and is NOT re-opened on a SIGHUP
	// config reload below — if storage.backend/retention changes on reload,
	// the running store keeps its original settings until next restart. Kept
	// intentionally simple; revisit if that proves surprising in practice.
	// Back-fill a "telegram" channel from legacy telegram.token/chat_id, if
	// any, so it's visible to `channel list` from the moment the daemon next
	// touches this config. Best-effort: a save failure here must not stop
	// the daemon from starting.
	if migrateTelegramChannel(cfg) {
		_ = saveCfg(cfg)
	}
	// dispatcher fans outbound alerts (anomalies, boot report, digests) out to
	// every configured Channel. It is stored alongside cfg, guarded by the
	// same mutex, and rebuilt-then-pointer-swapped (never mutated in place)
	// whenever the channel set could have changed, mirroring the cfg
	// pointer-swap pattern below.
	dispatcher := NewDispatcher(channelsFromConfig(cfg), dispatcherTimeout)
	// applyConfig swaps the shared cfg pointer and rebuilds the dispatcher
	// under mu (mirroring setChatID's pointer-swap pattern below). It does
	// NOT persist: callers that already have c on disk (the SIGHUP handler,
	// which just re-read cfgPath) call this directly; reload (below) persists
	// first, then applies.
	applyConfig := func(c *config.Config) {
		mu.Lock()
		cfg = c
		dispatcher = NewDispatcher(channelsFromConfig(c), dispatcherTimeout)
		mu.Unlock()
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, sighup)
	go func() {
		for range hup {
			if c, err := config.Load(cfgPath); err == nil {
				applyConfig(c)
				fmt.Fprintln(stdout, "config reloaded")
			}
		}
	}()
	getCfg := func() *config.Config { mu.RLock(); defer mu.RUnlock(); return cfg }
	getDispatcher := func() *Dispatcher { mu.RLock(); defer mu.RUnlock(); return dispatcher }
	// reload persists newCfg to disk then applies it in-process: the closure
	// WebDeps.Reload exposes to a future web config editor (issue #66) so
	// writes take effect immediately, without a SIGHUP round-trip.
	reload := func(newCfg *config.Config) error {
		if err := saveCfg(newCfg); err != nil {
			return err
		}
		applyConfig(newCfg)
		return nil
	}
	// testChannel is the closure WebDeps.TestChannel exposes to a future web
	// channels page's "Send test" button (issue #66): it reads the LIVE
	// config (getCfg, race-safe against a concurrent SIGHUP/Reload) so a
	// test-send always reflects whatever channel settings are currently
	// applied, not whatever was configured when the daemon started.
	testChannel := func(name string) error {
		return sendTestNotification(getCfg(), name, "web")
	}
	// setChatID race-safely records an auto-captured chat id by pointer-SWAPPING
	// the shared cfg (mirroring the SIGHUP reload above). Never mutate a field on
	// the in-use struct: getCfg readers read fields after releasing the RLock.
	// The dispatcher is rebuilt here too: a zero-config Telegram channel built
	// before the chat id was auto-captured would otherwise bake in an empty
	// chat id and keep failing silently until the next SIGHUP.
	setChatID := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		nc := *cfg // shallow struct copy
		nc.Telegram.ChatID = id
		cfg = &nc // swap pointer; existing readers keep old struct
		dispatcher = NewDispatcher(channelsFromConfig(cfg), dispatcherTimeout)
		_ = saveCfg(cfg)
	}

	// state
	baseline := NewBaseline()
	baseline.LoadFrom(st.BaselinePath(), fs)
	alerts := LoadAlertState(st.AlertStatePath(), fs)
	alog := NewAlertLog(st.AlertLogPath())
	var net NetTracker
	// prevCPU belongs solely to the sampler goroutine (this function). The
	// poller keeps its OWN CPUStat so no *CPUStat is shared across goroutines.
	var prevCPU CPUStat
	// netRate belongs solely to the sampler goroutine too (same pattern as
	// prevCPU): it accumulates the previous /proc/net/dev sample across slow
	// ticks so it can diff cumulative counters into bytes/sec rates.
	var netRate NetRateCalc
	// procCPU is the sampler goroutine's single ProcCPUCalc instance (same
	// single-owner pattern as prevCPU/netRate): it accumulates the previous
	// per-pid jiffies sample across slow ticks so collectProcesses can diff
	// cumulative CPU jiffies into a per-process CPU%.
	var procCPU ProcCPUCalc
	// smartState is the sampler goroutine's single smartCache instance (same
	// single-owner pattern as netRate/procCPU): it persists the last SMART
	// scan across slow ticks so collectSlow can throttle smartctl calls to
	// c.SmartIntervalSec() instead of scanning every slow tick.
	var smartState smartCache
	da := probeDocker(x, fs)

	// maybeStartWeb is always called, in both build variants: the default
	// (no `web` tag) build's maybeStartWeb (daemon_noweb.go) is a no-op that
	// never references internal/web, so this call site itself carries no
	// third-party dependency. Only the `-tags web` build (daemon_web.go)
	// actually starts anything, and only when Enabled (cfg.Web.Enabled) does
	// it bind a listener. Enabled/Listen are read once at daemon startup
	// (like c0 below) rather than through getCfg on every access: a SIGHUP
	// reload that flips web.enabled/web.listen takes effect on the next
	// daemon restart, not in-process — starting/stopping the listener
	// live is out of scope for this seam. See web_deps.go for the full
	// design note.
	cfgAtStart := getCfg()
	stopWeb := maybeStartWeb(WebDeps{
		Cfg:            getCfg,
		Reload:         reload,
		Store:          store,
		Snapshot:       latestSnapshot,
		StateDir:       stateDir,
		AlertLogPath:   st.AlertLogPath(),
		AlertStatePath: st.AlertStatePath(),
		TestChannel:    testChannel,
		Enabled:        cfgAtStart.Web.Enabled,
		Listen:         cfgAtStart.Web.Listen,
	})
	defer stopWeb()

	// boot/recovery report from heartbeat gap
	c0 := getCfg()
	if last, ok := readHeartbeat(st.HeartbeatPath(), fs); ok {
		if ev, ok := reconstructPowerDown(last, clock.Now(), time.Duration(c0.HeartbeatInterval)*time.Second); ok {
			if store != nil {
				_ = store.AppendEvent(ev)
			}
			now := clock.Now().Unix()
			snap := collectSnapshot(x, fs, &prevCPU, da, c0, store, now)
			dispatchAndLog(getDispatcher(), alog, Alert{
				Title:    formatBootReport([]DownEvent{ev}, renderStatus(snap)),
				Severity: SevInfo,
				Kind:     "fire",
				Source:   "boot",
				Time:     now,
			}, false) // reports bypass quiet hours
		}
	}

	// telegram long-poller (owns its own prevCPU internally)
	go pollLoop(getCfg, setChatID, store, x, fs, da)

	// sampler loop: ticks at fast_interval. Every Nth fast tick (N computed by
	// slowEvery from fast_interval/sample_interval) ALSO runs collectSlow and
	// refreshes the slow-tier fields on merged; between slow ticks, merged
	// keeps the last-collected slow values (status.json/anomaly eval always
	// see a full, if not maximally fresh, Snapshot). merged is local to this
	// single goroutine, same as prevCPU, so no locking is needed for it.
	var lastDaily, lastWeekly time.Time
	fastTicker := time.NewTicker(time.Duration(getCfg().FastInterval) * time.Second)
	defer fastTicker.Stop()
	lastFast := getCfg().FastInterval
	lastSlow := getCfg().SampleInterval
	n := slowEvery(lastFast, lastSlow)
	tick := 0
	var merged Snapshot
	var lastHeartbeat int64 // unix seconds; 0 sentinel forces an immediate first heartbeat
	for {
		c := getCfg()
		if c.FastInterval != lastFast || c.SampleInterval != lastSlow {
			fastTicker.Reset(time.Duration(c.FastInterval) * time.Second)
			lastFast = c.FastInterval
			lastSlow = c.SampleInterval
			n = slowEvery(lastFast, lastSlow)
			tick = 0 // realign: the new N is measured from this reload point
		}
		now := clock.Now()

		fast := collectFast(x, fs, &prevCPU)
		merged.CPU, merged.MemPct, merged.SwapPct, merged.Load1, merged.Load5, merged.Load15, merged.TempC =
			fast.CPU, fast.MemPct, fast.SwapPct, fast.Load1, fast.Load5, fast.Load15, fast.TempC

		// tick==0 also runs the slow tier so the very first status.json/
		// anomaly eval after startup or a reload is already fully populated,
		// rather than waiting up to N-1 fast ticks for disks/docker/etc.
		isSlowTick := tick%n == 0
		if isSlowTick {
			slow := collectSlow(x, fs, da, c, store, now.Unix(), &smartState, c.SmartIntervalSec())
			mergeSlowFields(&merged, slow)

			// net throughput (opt-in via collect.net_throughput): /proc/net/dev
			// holds cumulative counters, so netRate.Rates diffs this sample
			// against the one from the previous slow tick to get bytes/sec.
			// Disabled or a read/parse failure just clears NetRates for this
			// tick rather than failing the rest of the slow collection.
			merged.NetRates = nil
			if c.NetThroughputEnabled() {
				if b, err := fs.Read("/proc/net/dev"); err == nil {
					merged.NetRates = netRate.Rates(parseNetDev(string(b)), now.Unix())
				}
			}

			// process-table overview (opt-in via collect.processes): like net
			// throughput above, this is stateful across slow ticks (procCPU
			// diffs cumulative per-pid jiffies into CPU%), so it's called
			// directly here rather than folded into collectSlow, which has no
			// persistent-calc parameter. Disabled just leaves merged.Processes
			// at its zero value for this tick; collectProcesses itself already
			// guards against vanished/malformed /proc entries.
			merged.Processes = ProcSnapshot{}
			if c.ProcessesEnabled() {
				merged.Processes = collectProcesses(fs, &procCPU, os.Getpagesize()/1024)
			}
		}
		merged.TS = now.Unix()
		// Publish a COPY of merged into snapshotHub for lock-free readers
		// (WebDeps.Snapshot, ultimately a future web dashboard/SSE handler):
		// merged itself stays exclusively owned by this goroutine, so every
		// other field mutation above is safe without a lock, but the published
		// pointer must not alias a struct this loop keeps mutating in place.
		snap := merged
		snapshotHub.Store(&snap)

		// heartbeat has its own cadence (HeartbeatInterval), independent of
		// fast/slow: it exists only so a future boot can measure how long the
		// process was gone, so writing it more often than that buys nothing.
		if shouldHeartbeat(lastHeartbeat, now.Unix(), c.HeartbeatInterval) {
			_ = writeHeartbeat(st.HeartbeatPath(), now)
			lastHeartbeat = now.Unix()
		}
		_ = st.WriteStatus(merged) // every fast tick: status.json is the live view

		// feed the systemd watchdog (WatchdogSec in the unit). No-op when not run
		// under systemd. A wedged sampler loop stops pinging -> systemd restarts us.
		_ = sdNotify("WATCHDOG=1")

		// SampleStore write path: every fast tick appends the cheap fast-tier
		// metrics as raw samples. This is the sole write path for sample
		// data now — the legacy JSONL Store's AppendSample/AppendDown are no
		// longer called (see the dual-write removal note at store opening
		// above); migrate.go's one-shot importer still reads any
		// already-on-disk legacy files via Store.SamplesSince/DownSince.
		if store != nil {
			_ = store.Append(merged.TS, fastMetricSet(merged))
		}

		if isSlowTick {
			// Healthchecks pings stay on the slow cadence: pinging every fast
			// tick would be 12x today's volume (default fast=5s, slow=60s)
			// for no added signal.
			pingHealthchecks(c.Healthchecks.URL, httpPing)

			if store != nil {
				_ = store.Append(merged.TS, slowMetricSet(merged))
				if cm := containerMetricSet(merged); len(cm) > 0 {
					_ = store.Append(merged.TS, cm)
				}
				// nm is empty on the very first slow tick (netRate has no
				// prior sample yet) and whenever collect.net_throughput is
				// disabled, so the len()>0 guard skips the Append then too.
				if nm := netRateMetricSet(merged.NetRates); len(nm) > 0 {
					_ = store.Append(merged.TS, nm)
				}
				// sm is empty whenever no discovered SMART device reported a
				// parseable temperature attribute this tick (SmartAttrs nil/
				// empty, or every device's TempC==0), so the len()>0 guard
				// skips the Append then too, mirroring netRateMetricSet above.
				if sm := smartMetricSet(merged); len(sm) > 0 {
					_ = store.Append(merged.TS, sm)
				}
			}

			// net_down interval tracking: Online is a slow-tier field, only
			// meaningfully updated on slow ticks, so only feed the tracker
			// when it was actually just refreshed.
			if ev, closed := net.Update(merged.Online, now.Unix()); closed {
				if store != nil {
					_ = store.AppendEvent(ev)
				}
			}
		}

		// anomalies: each channel's own Route (severity/kind filters,
		// CriticalOverridesQuiet) now decides delivery, so no gating happens
		// here beyond computing whether quiet hours are active. Fast checks
		// (cpu/mem/swap/temp) are evaluated every fast tick since they're
		// cheap and change every tick; slow checks (disk/docker/service/
		// smart, plus their recovery sweeps) only change on slow ticks, so
		// they're evaluated then. Evaluate only touches keys present in the
		// checks it's given, so the slow keys' active state is left alone
		// between slow ticks rather than being spuriously re-fired/recovered.
		disp := getDispatcher()
		quiet := inQuietHours(c.QuietHours, now)
		events := alerts.Evaluate(buildFastChecks(merged, c), baseline, c.BaselineSigma, now.Unix())
		for _, e := range events {
			dispatchAndLog(disp, alog, eventToAlert(e, now.Unix()), quiet)
		}
		stateChanged := len(events) > 0
		if isSlowTick {
			slowEvents := alerts.Evaluate(buildSlowChecks(merged, c, alerts.Active), baseline, c.BaselineSigma, now.Unix())
			for _, e := range slowEvents {
				dispatchAndLog(disp, alog, eventToAlert(e, now.Unix()), quiet)
			}
			stateChanged = stateChanged || len(slowEvents) > 0
		}
		// scheduled digests bypass quiet hours, like the boot report.
		if matchDaily(c.Schedule.Daily, now, lastDaily) {
			lastDaily = now
			dispatchAndLog(disp, alog, Alert{Title: digestNow(store, now, 1, "📊 daily digest", configuredRawRetention(c)), Severity: SevInfo, Kind: "fire", Source: "digest", Time: now.Unix()}, false)
		}
		if matchWeekly(c.Schedule.Weekly, now, lastWeekly) {
			lastWeekly = now
			dispatchAndLog(disp, alog, Alert{Title: digestNow(store, now, 7, "📆 weekly rollup", configuredRawRetention(c)), Severity: SevInfo, Kind: "fire", Source: "digest", Time: now.Unix()}, false)
		}
		// alerts.json only changes when a fire/recover transition happened;
		// baseline.json's stats are updated every fast tick in memory but only
		// need to hit disk at the slow cadence. Both were previously saved
		// unconditionally every fast tick, which is 12x today's default
		// (fast=5s, slow=60s) write volume for no benefit.
		if stateChanged {
			// Pull any CLI-written ack flags back onto the in-memory state
			// before saving, or this save would clobber a `serverwatch alerts
			// ack` that landed on disk since the daemon last loaded.
			alerts.MergeAckFromDisk(st.AlertStatePath(), fs)
			_ = alerts.Save(st.AlertStatePath())
		}
		if isSlowTick {
			_ = baseline.Save(st.BaselinePath())
			_ = alog.PruneAlertLog(now.Add(-alertLogRetention).Unix())
			if store != nil {
				_ = store.Downsample(now.Unix())
				_ = store.Prune(now.Unix())
			}
		}

		tick++
		<-fastTicker.C
	}
}

// configuredRawRetention returns cfg.Storage.RawRetention parsed as a
// Duration, falling back to defaultRawRetention when unset/unparseable —
// mirroring the fallback each SampleStore backend applies internally via
// StoreOptions.withDefaults. Needed here too since PickResolution takes the
// raw duration directly rather than going through a backend.
func configuredRawRetention(cfg *config.Config) time.Duration {
	d, err := time.ParseDuration(cfg.Storage.RawRetention)
	if err != nil || d <= 0 {
		return defaultRawRetention
	}
	return d
}

// digestNow builds a digest by querying store's cpu/mem series and downtime
// events over the window [now-days*24h, now]. rawRetention (the configured
// raw-resolution retention window) drives PickResolution so the query uses
// raw points when the window fits inside it and 1m rollups otherwise. When
// store is nil (OpenStore failed at daemon startup) it degrades to an
// all-zero/empty digest rather than crashing.
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
	// Peaks use the coarsest resolution that still covers the window (raw for
	// recent windows → finer recent peaks; 1m for windows older than raw
	// retention).
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
	// Sample count is always taken at 1m resolution so the reported "samples:
	// N" is cadence-stable: querying peaks at RAW (5s) resolution for a recent
	// window would otherwise inflate the count ~12x versus the old per-minute
	// persisted-sample cadence. (memStore ignores res and serves one series,
	// so the count there reflects whatever was appended.)
	countPts, _ := store.Query("cpu", since, nowUnix, Res1m)
	return buildDigest(title, window, peakCPU, peakMem, len(countPts), downs)
}

func pollLoop(getCfg func() *config.Config, setChatID func(string), store SampleStore, x Exec, fs FileSource, da dockerAccess) {
	// The poller owns its CPUStat; it is never shared with the sampler goroutine.
	offset := 0
	var prevCPU CPUStat
	for {
		c := getCfg()
		// GetUpdates needs only a token; gate on token so we can learn the chat
		// id from the first inbound message (zero-config Telegram setup).
		if c.Telegram.Token == "" {
			time.Sleep(5 * time.Second)
			continue
		}
		tg := telegram.New(c.Telegram.Token, c.Telegram.ChatID)
		ups, err := tg.GetUpdates(offset, 50)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			// auto-capture chat id on first message (race-safe pointer swap)
			if c.Telegram.ChatID == "" && u.ChatID != "" {
				setChatID(u.ChatID)
				c = getCfg()
				tg = telegram.New(c.Telegram.Token, c.Telegram.ChatID)
			}
			nowUnix := time.Now().Unix()
			snap := collectSnapshot(x, fs, &prevCPU, da, c, store, nowUnix)
			snap.TS = nowUnix
			_ = tg.SendMessage(handleCommand(u.Text, store, snap))
		}
	}
}
