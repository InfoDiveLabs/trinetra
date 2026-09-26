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

// buildFastChecks builds anomaly Checks for the cheap, high-frequency
// metrics (cpu/mem/swap/temp) that collectFast populates every fast tick.
// These are safe to evaluate on every fast_interval tick.
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
		// Name the resource culprit in the fire message (#103) when there is
		// one to name; breach() uses FireMsg verbatim over the numeric format,
		// so it must include the same "key = val >= threshold thr" prefix.
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
	// collector:<name> health checks (#110): a slow-tier collector failing for
	// collectorAlertThreshold consecutive cycles fires; recovers on success.
	checks = append(checks, buildCollectorChecks(snap, slowInterval)...)
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
// cardinality this design accepts (docs/handbook/12-roadmap-and-status.md #71/#76); adding net
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
// empty MetricSet -- the sampler loop's caller skips the Append entirely in
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
// device this tick) are omitted rather than appending a misleading zero --
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

func httpPing(url string) error { return httpPingGuarded(url, false) }

// httpPingGuarded is httpPing with the opt-in SSRF guard (#97): when block is
// true it refuses to ping a loopback/link-local/private healthchecks URL, the
// same guard the channel notifiers use (outbound_guard.go).
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
// smartScanAttempt marks the SMART collector as attempted this cycle (#110) and
// runs `smartctl --scan`. Split out so the scan can be the init statement of
// collectSlow's smart if/else chain while still recording the attempt.
func smartScanAttempt(x Exec, snap *Snapshot) ([]byte, error) {
	snap.collectorsAttempted["smart"] = true
	return runMaybeSudo(x, "smartctl", "--scan")
}

func collectSlow(x Exec, fs FileSource, da dockerAccess, c *config.Config, store SampleStore, nowUnix int64, sc *smartCache, smartIntervalSec int) Snapshot {
	var snap Snapshot
	snap.DockerAccess = da.method
	// Per-collector attempt/error tracking (#110): disk and the failed-units
	// check always run; docker/smart are marked attempted only where they
	// actually shell out below. recordCollectorErr notes a command
	// error/timeout so the slow-collector goroutine can carry last-known values
	// forward and drive a collector:<name> alert instead of publishing a
	// healthy target flipped to gone.
	snap.collectorsAttempted = map[string]bool{"disk": true, "services": true}
	// snap.Disks and snap.DiskDetail are BOTH derived from the single typed
	// `df -PT -B1` call (device/fstype/usage/size/free per mount), gated by
	// isRealMount && isRealFsType. This used to be two separate df calls --
	// snap.Disks from untyped `df -PB1` (path-prefix filtering only) and
	// snap.DiskDetail from the typed call -- which meant snap.Disks had no
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
	// Swarm task -> service collapse (#118): key containers and their stats by
	// stable service name instead of ephemeral task name, so a rolling deploy
	// neither creates new per-task series (bounding cardinality, #112) nor flaps
	// down/recover on disappearing task names. Only runs on an active Swarm
	// node; plain-docker hosts are untouched.
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
	} else if out, err := smartScanAttempt(x, &snap); err != nil {
		recordCollectorErr(&snap, "smart", err)
	} else {
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
	merged.CollectorHealth = slow.CollectorHealth
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

// eventToAlert converts an anomaly Event (the internal alert-state
// transition) into the channel-agnostic Alert the Dispatcher understands.
// Body is left empty: e.Text already carries the full human-readable
// message and is used verbatim as the Title.
//
// e.Text is PLAIN text that embeds live container/unit/device names (via
// buildSlowChecks' FireMsg/RecoverMsg and breach()), so it is HTML-escaped
// here at the source. The Telegram sink now sends parse_mode=HTML, and a
// name containing <, >, or & would otherwise produce unbalanced HTML → a
// Telegram 400 → the alert (the core alerting path) silently dropped. This
// escaping is done at the plain-text source rather than in formatAlert,
// because other dispatched alerts (the boot report, digests) carry
// INTENTIONAL HTML from renderStatus that must not be escaped -- see
// formatBootReport / the boot/digest Alert{} sites in cmdDaemon.
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

// alertLogRetention bounds how long AlertEvents are kept in the alert log
// (see PruneAlertLog), mirroring the ~30-day retention used elsewhere for
// similar append-only histories.
const alertLogRetention = 30 * 24 * time.Hour

// storeMaintenanceInterval throttles the fsync-heavy store maintenance
// (Downsample + Prune). It runs on this cadence rather than every slow tick:
// Prune rewrites+fsyncs every series file, so on a store with many series a
// single pass can take much longer than a slow tick, and running it every tick
// pins the store lock continuously (starving history/Series reads for the web
// UI). Retention/downsampling only need to be approximately current, so a
// coarse cadence is fine and keeps the store lock free for reads the rest of
// the time.
const storeMaintenanceInterval = 15 * time.Minute

// alertRoute, when set, decides whether an Alert reaching enqueueAndLog is
// delivered locally (true) or held back because the fleet master is
// expected to deliver it instead (false, while it holds a valid delivery
// lease -- see fleet_lease.go). It is nil for solo and for a master
// (enqueueAndLog then behaves exactly as it always has: everything is
// delivered locally) and is set only by a child, in startChild, to its
// *handoff.Route. This package-level hook -- rather than threading a route
// func through enqueueAndLog's signature or fleetDeps -- is deliberate:
// enqueueAndLog is called from half a dozen sites across this file (the
// anomaly sampler, boot report, digests, and fleetDeps.alert itself), most
// of which have no fleetDeps in scope at all, and every one of them must
// keep working byte-for-byte unchanged for solo and master. A signature or
// fleetDeps-threading change would touch every call site just to reach the
// one (startChild) that needs it; a hook that defaults to nil touches none
// of them and leaves solo/master's control flow through enqueueAndLog
// identical to before this file changed.
//
// A pointer-to-func (not a plain func) behind atomic.Pointer so startChild's
// wiring (and rt.stop's teardown, and a test's cleanup) can install/clear it
// without racing a concurrent read from the sampler goroutine.
var alertRoute atomic.Pointer[func(Alert) bool]

// setAlertRoute installs f (nil clears it) as alertRoute and returns a
// restore func that puts back whatever was installed before -- so
// startChild's shutdown, and a test's t.Cleanup, can undo exactly their own
// wiring rather than unconditionally clearing a route something else in the
// same process installed.
func setAlertRoute(f func(Alert) bool) (restore func()) {
	var prev *func(Alert) bool
	if f == nil {
		prev = alertRoute.Swap(nil)
	} else {
		prev = alertRoute.Swap(&f)
	}
	return func() { alertRoute.Store(prev) }
}

// enqueueAndLog records the alert to the AlertLog and the live event bus, then
// hands delivery to the async notifier queue. This is the single choke point
// every alert in the daemon (anomaly fire/recover, boot report, digests) goes
// through so the alert log and the live bus stay a complete history, while the
// actual delivery happens off the sampler goroutine on the NotifierQueue
// worker. Delivery result is no longer recorded synchronously (delivery is
// off-thread now); the AlertLog entry marks it as queued (Delivered nil).
// alog may be nil (kept symmetrical with the store's nil-degrades-gracefully
// convention elsewhere in this file), in which case logging is skipped; bus
// may also be nil (eventBus.Publish's own nil-guard). alertEventKind maps the
// bus event Kind exactly as before, so control-socket subscribers see the
// same alert_fire/alert_recover/digest shapes.
//
// alertRoute (see above), if set, can turn the local delivery
// (q.Enqueue) off: AppendAlertEvent and bus.Publish always run regardless,
// so a routed-to-master alert is still recorded and visible everywhere it
// always was, just not queued for this host's own notifier channels.
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

// deliverSyncAndLog is enqueueAndLog's synchronous twin, used ONLY as the
// fleet master's alerting-engine delivery hook (fleetDeps.alert,
// fleetAlertEngine.deliver in fleet_engine.go). The engine must know
// whether at least one channel actually accepted the alert before it pushes
// a receipt down to a child node (B3 review round 1: a receipt must never
// go out before delivery has actually completed) -- something
// enqueueAndLog's fire-and-forget NotifierQueue.Enqueue cannot report. It
// logs and publishes exactly like enqueueAndLog (so the master's own
// alertlog.jsonl/live event bus stay a complete history for its own alerts,
// same as before this existed), then dispatches synchronously against q's
// CURRENT Dispatcher -- bypassing the async queue and its drop policy
// entirely, which exists for the high-volume local anomaly path this isn't
// -- and reports whether at least one channel succeeded.
//
// The engine already runs this off its own goroutine per alert (see
// Submit), so blocking here for up to the Dispatcher's ~15s-per-channel
// timeout does not stall the engine's caller.
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

// deliverSyncAndLogTo is deliverSyncAndLog narrowed to a specific
// channel-name subset (fleet routing, task 5): identical alert-log/live-bus
// recording, but dispatches via the Dispatcher's DispatchTo rather than
// Dispatch -- used as fleetAlertEngine.SetRouting's deliverNamed once a
// routing config is wired (fleetDeps.deliverSyncTo below).
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

// dispatchOnlyTo dispatches to a channel-name subset WITHOUT any
// alert-log/live-bus recording (fleet routing, task 5): used as
// fleetAlertEngine.SetRouting's dispatchOnly for escalation/repeat
// notifications, which re-send an alert that was already recorded once (at
// its own fire) rather than logging a new record every time.
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

// alertEventKind maps a dispatched Alert onto the Kind string its
// core.Event carries on the live event bus. Only anomaly-sourced alerts
// (Source "anomaly", from eventToAlert above) have a real fire/recover
// distinction worth naming -- those map "fire"/"recover" to
// "alert_fire"/"alert_recover" so a stream consumer can tell an anomaly
// transition from the "snapshot" ticks the sampler loop also publishes.
// Every other alert source (boot report, daily/weekly digest) hard-codes
// Alert.Kind to "fire" only because the struct field has to be something,
// not because it's semantically a fire/recover transition -- those pass
// a.Kind straight through unmapped ("digests keep their kind", per the A2
// live-push design doc).
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
	// Never start empty next to an unmigrated serverwatch install: that would
	// silently begin a fresh history (and a fresh Telegram enrollment) while
	// the real data sits in the old paths. Exit non-zero so systemd shows it.
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

	// daemonCtx bounds the lifetime of the daemon's background goroutines (the
	// notifier worker and the watchdog); the SIGTERM/SIGINT handler below
	// cancels it (after writing the clean-stop marker) on the way out.
	daemonCtx, daemonCancel := context.WithCancel(context.Background())
	defer daemonCancel()

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
	// Apply the operator-tuned per-command hang-breaker before ANY external
	// collector runs (probeDocker below and the slow-collector goroutine). Set
	// once here, before goroutines spawn, so concurrent osExec.Run reads need no
	// lock. Takes effect on restart, not SIGHUP reload (documented on the field).
	if cfg.ExecTimeout > 0 {
		execTimeout = time.Duration(cfg.ExecTimeout) * time.Second
	}
	// SampleStore (raw appends + downsample + prune), opened once here at
	// startup: this is now the SOLE writer of samples/downtime events, and the
	// sole read path for history/handlers/digests below. The old JSONL Store
	// (st) above remains only for status.json, the heartbeat file, and the
	// baseline/alert-state path helpers -- none of which are sample data.
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
	// storeWriter funnels EVERY SampleStore write through one goroutine, off the
	// sampler loop: the tsFileStore's Prune/Downsample fsync each rewritten
	// series file while holding the store lock, so an inline store.Append/Prune
	// on the sampler can block for far longer than the systemd watchdog window
	// on a slow disk or a large post-downtime prune backlog, freezing liveness.
	// The sampler submit()s non-blockingly; a stuck fsync now only delays sample
	// persistence, never the sampler. nil when store writes are disabled.
	var sw *storeWriter
	if store != nil {
		sw = newStoreWriter(store, 8)
		go sw.run(daemonCtx)
	}
	// NOTE: the store is opened once here and is NOT re-opened on a SIGHUP
	// config reload below -- if storage.backend/retention changes on reload,
	// the running store keeps its original settings until next restart. Kept
	// intentionally simple; revisit if that proves surprising in practice.
	// Back-fill a "telegram" channel from legacy telegram.token/chat_id, if
	// any, so it's visible to `channel list` from the moment the daemon next
	// touches this config. Best-effort: a save failure here must not stop
	// the daemon from starting.
	if migrateTelegramChannel(cfg) {
		_ = saveDaemonCfg(cfg)
	}
	// dispatcher fans outbound alerts (anomalies, boot report, digests) out to
	// every configured Channel. It is stored alongside cfg, guarded by the
	// same mutex, and rebuilt-then-pointer-swapped (never mutated in place)
	// whenever the channel set could have changed, mirroring the cfg
	// pointer-swap pattern below.
	dispatcher := NewDispatcher(channelsFromConfig(cfg), dispatcherTimeout)
	// q is the async notifier queue: Enqueue is non-blocking, delivery happens
	// on its own worker goroutine, so a slow/hung Telegram uplink can never
	// stall the sampler loop. Constructed HERE -- before applyConfig and any
	// signal/control-socket handler that could trigger a reload -- so the plain
	// `q` var is only ever written once (before any goroutine reads it) and
	// applyConfig can propagate dispatcher rebuilds into it without a nil-guard.
	q := NewNotifierQueue(dispatcher, 64)
	go q.Run(daemonCtx)
	// applyConfig swaps the shared cfg pointer and rebuilds the dispatcher
	// under mu (mirroring setChatID's pointer-swap pattern below). It does
	// NOT persist: callers that already have c on disk (the SIGHUP handler,
	// which just re-read cfgPath) call this directly; reload (below) persists
	// first, then applies.
	applyConfig := func(c *config.Config) {
		nd := NewDispatcher(channelsFromConfig(c), dispatcherTimeout)
		mu.Lock()
		cfg = c
		dispatcher = nd
		mu.Unlock()
		// Propagate the rebuilt dispatcher to the async notifier worker so a
		// channel-set change on reload takes effect for queued/future alerts.
		// Use the local nd, not the shared `dispatcher` field, which a
		// concurrent applyConfig may already be rewriting under mu.
		q.SetDispatcher(nd)
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
	// SIGTERM/SIGINT: write the clean-stop marker (so the NEXT boot's downtime
	// reconstruction knows this stop was intentional, not a crash/power loss),
	// cancel the daemon context to unwind the background goroutines, and exit.
	// fleetStop holds the fleet runtime's stop (set once startFleet has run
	// below). os.Exit skips deferred calls, so the term handler runs it
	// explicitly: registry flush / shipper drain, bounded to a few seconds.
	// For solo it is a no-op.
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
	// reload persists newCfg to disk then applies it in-process: the closure
	// newInprocAPI's ApplyConfig exposes to the control socket (and, through
	// it, the web config editor, issue #66) so writes take effect
	// immediately, without a SIGHUP round-trip.
	// saveDaemonCfg keeps the on-disk fleet identity keys: this daemon's
	// in-memory config (or a plugin's) may predate a `trinetra fleet`
	// command, which must be the only thing that changes them.
	reload := func(newCfg *config.Config) error {
		if err := saveDaemonCfg(newCfg); err != nil {
			return err
		}
		applyConfig(newCfg)
		return nil
	}
	// setChatID race-safely records an auto-captured chat id by pointer-SWAPPING
	// the shared cfg (mirroring the SIGHUP reload above). Never mutate a field on
	// the in-use struct: getCfg readers read fields after releasing the RLock.
	// The dispatcher is rebuilt here too: a zero-config Telegram channel built
	// before the chat id was auto-captured would otherwise bake in an empty
	// chat id and keep failing silently until the next SIGHUP.
	setChatID := func(id string) {
		mu.Lock()
		nc := *cfg // shallow struct copy
		nc.Telegram.ChatID = id
		// Saved (which overlays the on-disk fleet keys onto nc, so a
		// `fleet join` made since start is never wiped) BEFORE the swap:
		// nc must not be mutated once other goroutines can read it.
		_ = saveDaemonCfg(&nc)
		cfg = &nc // swap pointer; existing readers keep old struct
		nd := NewDispatcher(channelsFromConfig(cfg), dispatcherTimeout)
		dispatcher = nd
		mu.Unlock()
		// Propagate the rebuilt dispatcher to the async notifier worker, mirroring
		// applyConfig: delivery runs off the queue's own dispatcher pointer, so a
		// chat id auto-captured via /start enrollment would otherwise never reach
		// the queue until an unrelated reload/restart -- every alert after a fresh
		// zero-config enrollment would dispatch to the stale channel-less
		// dispatcher and silently not deliver. Called after Unlock, never under mu.
		q.SetDispatcher(nd)
	}

	// state
	baseline := NewBaseline()
	baseline.LoadFrom(st.BaselinePath(), fs)
	alerts := LoadAlertState(st.AlertStatePath(), fs)
	alog := NewAlertLog(st.AlertLogPath())
	var net NetTracker
	// prevCPU belongs solely to the sampler goroutine (this function). The
	// poller keeps its OWN CPUStat so no *CPUStat is shared across goroutines.
	// It is a FAST-tier calc (collectFast every tick), so unlike the slow-tier
	// calcs below it stays with the sampler loop.
	var prevCPU CPUStat
	// netRate/procCPU/smartState belong solely to the SLOW-COLLECTOR goroutine
	// (started below), which is the only goroutine that runs collectSlow +
	// NetRates + Processes now. They are declared here only so that goroutine's
	// closure can capture them; nothing else touches them, so no locking is
	// needed. netRate accumulates the previous /proc/net/dev sample across slow
	// cycles to diff cumulative counters into bytes/sec rates.
	var netRate NetRateCalc
	// procCPU accumulates the previous per-pid jiffies sample across slow cycles
	// so collectProcesses can diff cumulative CPU jiffies into a per-process CPU%.
	var procCPU ProcCPUCalc
	// smartState persists the last SMART scan across slow cycles so collectSlow
	// can throttle smartctl calls to c.SmartIntervalSec() instead of scanning
	// every cycle.
	var smartState smartCache
	// collHealth/prevSlow make slow collection fail-visible (#110): collHealth
	// tracks per-collector consecutive failures across cycles (owned by the
	// slow-collector goroutine, like the calcs above), and prevSlow holds the
	// last published slow snapshot so a collector that errors this cycle carries
	// its last-known values forward instead of publishing a healthy target
	// flipped to gone.
	collHealth := newCollectorHealth()
	var prevSlow Snapshot
	da := probeDocker(x, fs)

	cfgAtStart := getCfg()

	// bus is the daemon's live in-process event fan-out (eventbus.go):
	// enqueueAndLog publishes every alert onto it, the sampler
	// loop below publishes a "snapshot" tick after every snapshotHub.Store,
	// and inprocAPI.Subscribe (coreapi_inproc.go) hands each control-socket
	// subscriber its own subscription onto this same bus.
	bus := newEventBus()

	// control socket: serves the daemon's own core.API over a unix socket
	// under RUNTIME_DIRECTORY (or /run/trinetra) for the out-of-process
	// plugins (trinetra-ctl, trinetra-web). newInprocAPI is untagged,
	// so this carries no third-party dependency. Non-fatal: a bind failure
	// just logs and leaves the daemon running without the socket (and thus
	// without the web UI, which dials it).
	// enroll is the shared Telegram enrollment-pin holder (#90, enroll.go):
	// one instance for this daemon launch, handed to both the poll loop
	// (which generates/announces/matches against it) and the control socket
	// (so a separate process, e.g. `telegram set-token`, can read back the
	// exact same pin instead of only ever seeing it in the daemon's log).
	enroll := &enrollState{}
	controlAPI := newInprocAPI(latestSnapshot, getCfg, store, stateDir, reload, bus, enroll)
	// fleet: the ONE place the fleet role is honoured (fleet_daemon.go). For
	// solo this only builds an in-memory provider reporting this host -- no
	// files, no listener, no goroutines. The role is read once at start;
	// `trinetra fleet init|join|leave|disable` tell the operator to restart.
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
		alertFallback: func(a Alert, silences *pushedSilences) {
			deliverFallback(silences, alog, bus, q, a, inQuietHours(getCfg().QuietHours, time.Now()), time.Now().Unix())
		},
		logf: func(format string, args ...any) { fmt.Fprintf(stderr, format+"\n", args...) },
	})
	fleetStop.Store(&fleetRT.stop)
	defer fleetRT.stop()
	if fleetRT.tee != nil {
		if sw != nil {
			sw.setTee(fleetRT.tee)
		}
		alog.SetTee(fleetRT.tee.Alert)
	}
	stopControl, socketPath, token, err := serveControlSocket(&fleetAwareAPI{API: controlAPI, fleetProvider: fleetRT.provider})
	if err != nil {
		fmt.Fprintln(stderr, "control socket: failed to start, continuing without it:", err)
	} else {
		defer stopControl()
		// web UI: when enabled, supervise the trinetra-web plugin as a
		// verified child process (it dials the control socket above). Nothing
		// is embedded in the daemon anymore -- see web_supervisor.go.
		if shouldStartWeb(cfgAtStart, true) {
			stopWeb := startWeb(socketPath, token)
			defer stopWeb()
		}
	}

	// Liveness handoff shared with the slow-collector and watchdog goroutines.
	// hub carries the last-good slow Snapshot (atomic, versioned); lastTick and
	// lastSlowSuccess are the two liveness clocks the watchdog gates its systemd
	// ping on. Both are seeded to now so a slow first cycle can't trip the
	// watchdog before the collector has had a chance to run once.
	hub := &slowHub{}
	var lastTick, lastSlowSuccess atomic.Int64
	now0 := clock.Now().Unix()
	lastTick.Store(now0)
	lastSlowSuccess.Store(now0) // grace: don't trip the watchdog before the first slow cycle

	// slow-collector: runs collectSlow + NetRates + Processes off the sampler
	// loop, bounded by an overall deadline, publishing each result to hub for the
	// sampler to merge. slowCollector.runOnce serializes collection so only one
	// is ever in flight -- collect is the sole owner of netRate/procCPU/
	// smartState (declared above) and two overlapping collections would
	// data-race on their maps. Ticks immediately, then every SampleInterval.
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
			// net throughput + processes: stateful, owned here, and excluded
			// from mergeSlowFields, so they are populated directly onto the
			// published snapshot (the sampler copies them across alongside the
			// mergeSlowFields call).
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
			// Fail-visible collection (#110): update per-collector health from
			// this cycle's outcome, carry last-known values forward for any
			// collector that errored (so a failed docker/df/smartctl does not
			// blank a healthy target), and publish the health on the snapshot so
			// it drives collector:<name> alerts and shows in status.json.
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

	// healthchecks-pinger goroutine: the external healthchecks.io dead-man ping
	// stays on the slow cadence but off the sampler loop -- httpPing is bounded
	// at 10s, and a hung endpoint must not stall fast ticks. An empty URL is a
	// no-op inside pingHealthchecks.
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

	// watchdog goroutine: the SOLE sender of the systemd WATCHDOG=1 ping now
	// (removed from the sampler loop). It pings only while BOTH the sampler loop
	// (lastTick) and the slow collector (lastSlowSuccess) have made recent
	// progress, so a merely-slow slow cycle no longer starves the ping while a
	// genuinely wedged loop/collector still lets systemd restart the unit.
	go runWatchdog(daemonCtx,
		livenessGate{lastTick: &lastTick, lastSlowSuccess: &lastSlowSuccess, samplerStaleSec: 30, collectorStaleSec: 300},
		watchdogPeriodSec(), func() int64 { return clock.Now().Unix() },
		func() error { return sdNotify("WATCHDOG=1") })

	// boot/recovery report from heartbeat gap, deduped against a previous start
	// (Task 5): the same gap, recomputed from an unchanged heartbeat on a
	// restart, reports only once. A clean-stop marker written by the SIGTERM
	// handler suppresses the report entirely (the stop was intentional).
	c0 := getCfg()
	cleanStopPath := st.CleanStopPath()
	// bootReportCh carries the boot/recovery report from its background collector
	// goroutine (below) to the sampler loop, which delivers it alongside its
	// other alerts. This is a hand-off for ordering, not for safety: the
	// AlertLog serializes appends with its own mutex and enqueueAndLog is
	// already called from other goroutines (the fleet master's alerts). Cap 1:
	// at most one boot report is ever produced per process start, so the send
	// never blocks even if the loop is slow to drain it.
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
					// Collect the report's status snapshot on a goroutine and hand
					// the finished Alert back to the sampler loop. collectSnapshot
					// does an uncached full SMART scan (sc==nil) whose duration
					// grows with the host's disk count; running it inline here
					// would block the main goroutine from reaching the sampler loop
					// below, so lastTick would never advance, the watchdog gate
					// would go stale after samplerStaleSec, and systemd would kill
					// us mid-startup on any sufficiently large/slow host -- the
					// exact crash loop this change set exists to end. Its own
					// CPUStat (not the sampler's prevCPU) keeps it from racing the
					// sampler loop.
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

	// telegram long-poller (owns its own prevCPU internally)
	go pollLoop(getCfg, setChatID, store, x, fs, da, enroll)

	// sampler loop: ticks at fast_interval and does NO blocking subprocess or
	// network I/O -- that all lives on the slow-collector / healthchecks /
	// notifier goroutines now. Each tick it does the cheap fast collection,
	// reads the last-good slow snapshot from hub (published by the slow
	// collector), evaluates fast checks every tick and slow checks only when a
	// NEW slow snapshot has arrived (version-gated), stamps lastTick for the
	// watchdog, and enqueues any alerts. merged is local to this single
	// goroutine, same as prevCPU, so no locking is needed for it.
	var lastDaily, lastWeekly time.Time
	var lastStoreMaint time.Time // throttles the fsync-heavy Downsample+Prune
	fastTicker := time.NewTicker(time.Duration(getCfg().FastInterval) * time.Second)
	defer fastTicker.Stop()
	lastFast := getCfg().FastInterval
	var merged Snapshot
	var lastHeartbeat int64 // unix seconds; 0 sentinel forces an immediate first heartbeat
	var lastSlowVer uint64  // slow-hub version whose slow checks were last evaluated
	for {
		// Deliver the boot/recovery report once its background collector has
		// finished (see bootReportCh above), in line with the loop's other
		// alerts. Non-blocking: no report pending is the common case.
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

		// Read the last-good slow snapshot from the collector goroutine and merge
		// it onto merged every tick, so status.json/anomaly eval always see a
		// full (if not maximally fresh) Snapshot. NetRates/Processes are excluded
		// from mergeSlowFields (they're caller-populated), so copy them across
		// from the published snapshot explicitly. newSlow is true only on the
		// exact tick a fresh slow cycle first shows up here; it gates the slow
		// follow-up work below (slow-check eval, slow-series appends, net_down
		// tracking, baseline/prune) so that runs once per slow cycle rather than
		// every fast tick. SlowStale marks a snapshot whose slow tier wasn't
		// freshly collected this cycle (the common case between slow cycles, and
		// the signal that the collector has stalled).
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
		// Publish a COPY of merged into snapshotHub for lock-free readers
		// (latestSnapshot, ultimately the control socket's Snapshot handler):
		// merged itself stays exclusively owned by this goroutine, so every
		// other field mutation above is safe without a lock, but the published
		// pointer must not alias a struct this loop keeps mutating in place.
		snap := merged
		snapshotHub.Store(&snap)
		// Tell any live control-socket subscribers a fresh Snapshot is ready,
		// without pushing the Snapshot itself onto the bus: a subscriber (the
		// web UI's SSE handler, eventually) fetches the actual DashboardView
		// via Snapshot() only when this tick says to, keeping core.Event
		// alert-shaped rather than carrying a giant view in every frame.
		bus.Publish(core.Event{Kind: "snapshot", Time: now.Unix()})

		// Liveness: stamp this sampler tick for the watchdog goroutine, which is
		// now the SOLE sender of the systemd WATCHDOG=1 ping. A wedged sampler
		// loop stops advancing lastTick -> the watchdog stops pinging ->
		// systemd restarts us; a merely-slow slow collection no longer starves
		// the ping the way the old inline sdNotify here could.
		lastTick.Store(now.Unix())

		// heartbeat has its own cadence (HeartbeatInterval), independent of
		// fast/slow: it exists only so a future boot can measure how long the
		// process was gone, so writing it more often than that buys nothing.
		if shouldHeartbeat(lastHeartbeat, now.Unix(), c.HeartbeatInterval) {
			_ = writeHeartbeat(st.HeartbeatPath(), now)
			lastHeartbeat = now.Unix()
		}
		_ = st.WriteStatus(merged) // every fast tick: status.json is the live view

		// SampleStore write path: every fast tick appends the cheap fast-tier
		// metrics as raw samples. This is the sole write path for sample
		// data now -- the legacy JSONL Store's AppendSample/AppendDown are no
		// longer called (see the dual-write removal note at store opening
		// above); migrate.go's one-shot importer still reads any
		// already-on-disk legacy files via Store.SamplesSince/DownSince.
		// net_down interval tracking stays on the sampler: net.Update owns the
		// NetTracker state and must advance every slow cycle regardless of the
		// store. Its closed event (if any) rides along in the batch below.
		var netEvents []DownEvent
		if newSlow {
			if ev, closed := net.Update(merged.Online, now.Unix()); closed {
				netEvents = append(netEvents, ev)
			}
		}

		// Assemble this cycle's SampleStore batch and hand it to the off-thread
		// storeWriter (storewriter.go). ALL store writes -- fast-tier samples
		// every tick, the slow-tier sets and the fsync-heavy Downsample/Prune
		// maintenance on a slow cycle -- go through submit(), which never
		// blocks: a slow/stuck disk only delays or drops persistence, it can
		// never freeze the sampler and trip the watchdog (the crash-loop bug
		// this replaced inline store.Append/Prune to fix).
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
			// Run the fsync-heavy Downsample+Prune only every
			// storeMaintenanceInterval, not every slow tick: on a store with
			// many series a prune pass holds the store lock long enough to
			// starve web history reads, so keep it coarse.
			doMaint := newSlow && now.Sub(lastStoreMaint) >= storeMaintenanceInterval
			if doMaint {
				lastStoreMaint = now
			}
			sw.submit(storeWrite{ts: merged.TS, sets: sets, events: netEvents, maintain: doMaint, maintainNow: now.Unix()})
		}

		// anomalies: each channel's own Route (severity/kind filters,
		// CriticalOverridesQuiet) now decides delivery, so no gating happens
		// here beyond computing whether quiet hours are active. Fast checks
		// (cpu/mem/swap/temp) are evaluated every fast tick since they're
		// cheap and change every tick; slow checks (disk/docker/service/
		// smart, plus their recovery sweeps) only change when a fresh slow
		// snapshot arrives, so they're evaluated then. Evaluate only touches
		// keys present in the checks it's given, so the slow keys' active state
		// is left alone between slow cycles rather than being spuriously
		// re-fired/recovered. Delivery is off-thread now: enqueueAndLog records
		// the alert + publishes to the bus, then hands delivery to q's worker.
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
		// alerts.json only changes when a fire/recover transition happened;
		// baseline.json's stats are updated every fast tick in memory but only
		// need to hit disk at the slow cadence. Both were previously saved
		// unconditionally every fast tick, which is 12x today's default
		// (fast=5s, slow=60s) write volume for no benefit.
		if stateChanged {
			// Pull any CLI-written ack flags back onto the in-memory state
			// before saving, or this save would clobber a `trinetra alerts
			// ack` that landed on disk since the daemon last loaded.
			alerts.MergeAckFromDisk(st.AlertStatePath(), fs)
			_ = alerts.Save(st.AlertStatePath())
		}
		if newSlow {
			// baseline.Save and PruneAlertLog do not fsync (page-cache writes),
			// so they stay on the sampler. The store's fsync-heavy Downsample/
			// Prune moved to the storeWriter (submitted above via maintain), so
			// a stuck disk can't block the sampler here.
			_ = baseline.Save(st.BaselinePath())
			_ = alog.PruneAlertLog(now.Add(-alertLogRetention).Unix())
			// A child's handoff-receipts sidecar (fleet_lease.go) needs the
			// same periodic pruning as the alert log, not only the one-shot
			// prune startChild does at reconciliation time -- otherwise it
			// grows forever on a long-lived child. fleet.fallback_after is
			// re-read live (not RestartRequired), mirroring how startChild's
			// own prune window is computed.
			if fleetRT.provider.role == config.RoleChild {
				fallbackAfter := getCfg().FleetFallbackAfter()
				_ = pruneHandoffReceipts(handoffReceiptsPath(stateDir), now.Add(-10*fallbackAfter).Unix())
			}
		}

		<-fastTicker.C
	}
}

// configuredRawRetention returns cfg.Storage.RawRetention parsed as a
// Duration, falling back to defaultRawRetention when unset/unparseable --
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

func pollLoop(getCfg func() *config.Config, setChatID func(string), store SampleStore, x Exec, fs FileSource, da dockerAccess, enroll *enrollState) {
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
		// The enrollment PIN for the zero-config setup path (#78): an
		// unclaimed bot is claimed only by "/start <pin>", not by whoever
		// messages first. enroll (enroll.go) generates and caches it on
		// first call, so this stays stable for as long as the bot remains
		// unclaimed -- and is the SAME pin `trinetra telegram set-token`
		// can now read back over the control socket (core.API.
		// EnrollmentPIN), instead of only ever reaching the daemon's own
		// log.
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
		// It is only invoked by processUpdates for authorized senders, so an
		// unknown chat neither triggers host collection nor receives a reply.
		reply := func(cc *config.Config, u telegram.Update) {
			nowUnix := time.Now().Unix()
			snap := collectSnapshot(x, fs, &prevCPU, da, cc, store, nowUnix)
			snap.TS = nowUnix
			client := telegram.New(cc.Telegram.Token, cc.Telegram.ChatID)
			if err := client.SendMessage(handleCommand(u.Text, store, snap, cc)); err != nil {
				fmt.Fprintln(stderr, "telegram send:", err)
			}
		}
		// setChatID's reload already tells the daemon it's enrolled; wrap it
		// to also clear enroll's cached pin on success, so a later
		// re-enrollment (a new bot via `telegram set-token`) starts from a
		// fresh pin instead of reusing one that was already consumed.
		onEnroll := func(id string) {
			setChatID(id)
			enroll.Reset()
		}
		offset, c = processUpdates(ups, offset, c, enroll, time.Now, getCfg, onEnroll, reply)
	}
}

// processUpdates handles one batch of inbound Telegram updates. It advances
// the offset, enrolls the owner from a correct "/start <pin>" while the bot is
// unclaimed, drops any update whose sender is not the owner chat, and invokes
// reply for authorized commands. It returns the new offset and the (possibly
// reloaded) config so the caller can carry both into the next GetUpdates
// cycle.
func processUpdates(ups []telegram.Update, offset int, c *config.Config, enroll *enrollState, now func() time.Time, getCfg func() *config.Config, setChatID func(string), reply func(*config.Config, telegram.Update)) (int, *config.Config) {
	for _, u := range ups {
		offset = u.UpdateID + 1
		if c.Telegram.ChatID == "" {
			// Unclaimed: ownership is granted ONLY by a correct "/start <pin>"
			// (#78 Scenario A). Everything else is ignored, so an attacker who
			// merely messages the bot first cannot hijack it. enroll.Attempt
			// also rate-limits and rotates the pin under repeated wrong
			// guesses so the pin can't be brute-forced (#93).
			if u.ChatID != "" && enroll.Attempt(c, u.Text, now()) {
				setChatID(u.ChatID)
				c = getCfg()
				reply(c, u)
			}
			continue
		}
		// Claimed: only ever act on the owner chat. Any other sender is dropped
		// BEFORE collectSnapshot/SendMessage, so an unknown chat can neither
		// trigger host-side collection nor be answered (#78 Scenario B).
		if u.ChatID != c.Telegram.ChatID {
			continue
		}
		reply(c, u)
	}
	return offset, c
}

// enrollMatch reports whether text is exactly "/start <pin>" for a non-empty
// pin. An empty pin never matches, so a bot can never be claimed by an empty
// or missing PIN.
func enrollMatch(text, pin string) bool {
	if pin == "" {
		return false
	}
	f := strings.Fields(text)
	return len(f) == 2 && f[0] == "/start" && f[1] == pin
}

// newEnrollPIN returns a fresh 6-digit enrollment PIN from crypto/rand. On the
// (practically impossible) event of a rand failure it returns "", which
// enrollMatch treats as never-matching, so the bot fails closed rather than
// becoming claimable without a secret.
func newEnrollPIN() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%06d", n.Int64())
}
