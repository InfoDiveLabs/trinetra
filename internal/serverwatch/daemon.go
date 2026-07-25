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

func buildChecks(snap Snapshot, c *config.Config, active map[string]ActiveAlert) []Check {
	var checks []Check
	add := func(key string, val, thr float64, hasThr, crit bool) {
		if !c.TargetEnabled(key) {
			return
		}
		if o, ok := c.TargetThreshold(key); ok {
			thr, hasThr = o, true
		}
		checks = append(checks, Check{Key: key, Value: val, Threshold: thr, HasThreshold: hasThr, Critical: crit})
	}
	add("cpu", snap.CPU, c.Thresholds.CPUPct, true, false)
	add("mem", snap.MemPct, c.Thresholds.MemPct, true, false)
	add("swap", snap.SwapPct, c.Thresholds.SwapPct, true, false)
	if snap.TempC > 0 {
		add("temp", snap.TempC, c.Thresholds.TempC, true, false)
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
		v := 0.0
		if state != "running" {
			v = 1
		}
		checks = append(checks, Check{Key: key, Value: v, Threshold: 1, HasThreshold: true, Critical: true})
	}
	// SMART: FAILED=bad(1), otherwise ok(0)
	for dev, health := range snap.SmartHealth {
		key := "smart:" + dev
		if !c.TargetEnabled(key) {
			continue
		}
		v := 0.0
		if health == "FAILED" {
			v = 1
		}
		checks = append(checks, Check{Key: key, Value: v, Threshold: 1, HasThreshold: true, Critical: true})
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
		checks = append(checks, Check{Key: key, Value: 1, Threshold: 1, HasThreshold: true, Critical: true})
	}
	for key := range active {
		if strings.HasPrefix(key, "service:") && !failed[key] {
			checks = append(checks, Check{Key: key, Value: 0, Threshold: 1, HasThreshold: true, Critical: true})
		}
	}
	// docker/smart recovery sweep: if a container is REMOVED (not just stopped)
	// or `docker ps`/`smartctl --scan` starts erroring, the target disappears
	// from the snapshot maps entirely, so no check is emitted above and an
	// active alert would stay stuck forever. Mirror the service: sweep and emit
	// value=0 for any active docker:/smart: alert whose target is gone.
	for key := range active {
		if strings.HasPrefix(key, "docker:") {
			if _, ok := snap.Containers[strings.TrimPrefix(key, "docker:")]; !ok {
				checks = append(checks, Check{Key: key, Value: 0, Threshold: 1, HasThreshold: true, Critical: true})
			}
		}
		if strings.HasPrefix(key, "smart:") {
			if _, ok := snap.SmartHealth[strings.TrimPrefix(key, "smart:")]; !ok {
				checks = append(checks, Check{Key: key, Value: 0, Threshold: 1, HasThreshold: true, Critical: true})
			}
		}
	}
	return checks
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
		snap.Load1, _, _, _ = parseLoadavg(string(b))
	}
	if zones, _ := fs.Glob("/sys/class/thermal/thermal_zone*/temp"); len(zones) > 0 {
		if b, err := fs.Read(zones[0]); err == nil {
			snap.TempC, _ = parseThermal(string(b))
		}
	}
	return snap
}

// collectSlow gathers the expensive resources: shelling out to df/docker/
// systemctl/smartctl, plus the network connectivity dial. These are all
// either subprocess spawns or (for the connectivity check) multi-second
// network timeouts, so they run on the slow tier (sample_interval) rather
// than every fast tick.
func collectSlow(x Exec, fs FileSource, da dockerAccess) Snapshot {
	var snap Snapshot
	snap.DockerAccess = da.method
	if out, err := x.Run("df", "-PB1"); err == nil {
		snap.Disks = map[string]float64{}
		for _, d := range mustDF(string(out)) {
			if isRealMount(d.Mount) {
				snap.Disks[d.Mount] = d.UsedPct
			}
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
	// failed systemd units
	if out, err := runMaybeSudo(x, "systemctl", "--failed", "--plain", "--no-legend"); err == nil {
		snap.FailedUnits = parseFailedUnits(string(out))
	}
	// SMART health for every discovered device (all queried each cycle -> recovery works)
	if out, err := runMaybeSudo(x, "smartctl", "--scan"); err == nil {
		snap.SmartHealth = map[string]string{}
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
		}
	}
	return snap
}

// collectSnapshot runs both tiers and merges them into one full Snapshot.
// It exists for the two one-shot callers that need a complete picture right
// now rather than a tiered cadence: the boot/recovery report (a single
// collection before the sampler loop starts) and pollLoop (each inbound
// Telegram command wants a fresh, complete status). The tiered sampler loop
// in cmdDaemon does NOT use this: it calls collectFast/collectSlow directly
// so it can run collectSlow only every Nth fast tick.
func collectSnapshot(x Exec, fs FileSource, prev *CPUStat, da dockerAccess) Snapshot {
	snap := collectFast(x, fs, prev)
	slow := collectSlow(x, fs, da)
	snap.Disks = slow.Disks
	snap.Online = slow.Online
	snap.DockerAccess = slow.DockerAccess
	snap.Containers = slow.Containers
	snap.FailedUnits = slow.FailedUnits
	snap.SmartHealth = slow.SmartHealth
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
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, sighup)
	go func() {
		for range hup {
			if c, err := config.Load(cfgPath); err == nil {
				nd := NewDispatcher(channelsFromConfig(c), dispatcherTimeout)
				mu.Lock()
				cfg = c
				dispatcher = nd
				mu.Unlock()
				fmt.Fprintln(stdout, "config reloaded")
			}
		}
	}()
	getCfg := func() *config.Config { mu.RLock(); defer mu.RUnlock(); return cfg }
	getDispatcher := func() *Dispatcher { mu.RLock(); defer mu.RUnlock(); return dispatcher }
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
	var net NetTracker
	// prevCPU belongs solely to the sampler goroutine (this function). The
	// poller keeps its OWN CPUStat so no *CPUStat is shared across goroutines.
	var prevCPU CPUStat
	da := probeDocker(x, fs)

	// boot/recovery report from heartbeat gap
	c0 := getCfg()
	if last, ok := readHeartbeat(st.HeartbeatPath(), fs); ok {
		if ev, ok := reconstructPowerDown(last, clock.Now(), time.Duration(c0.SampleInterval)*time.Second); ok {
			_ = st.AppendDown(ev)
			snap := collectSnapshot(x, fs, &prevCPU, da)
			now := clock.Now().Unix()
			getDispatcher().Dispatch(Alert{
				Title:    formatBootReport([]DownEvent{ev}, renderStatus(snap)),
				Severity: SevInfo,
				Kind:     "fire",
				Source:   "boot",
				Time:     now,
			}, false) // reports bypass quiet hours
		}
	}

	// telegram long-poller (owns its own prevCPU internally)
	go pollLoop(getCfg, setChatID, st, x, fs, da)

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
		merged.CPU, merged.MemPct, merged.SwapPct, merged.Load1, merged.TempC =
			fast.CPU, fast.MemPct, fast.SwapPct, fast.Load1, fast.TempC

		// tick==0 also runs the slow tier so the very first status.json/
		// anomaly eval after startup or a reload is already fully populated,
		// rather than waiting up to N-1 fast ticks for disks/docker/etc.
		isSlowTick := tick%n == 0
		if isSlowTick {
			slow := collectSlow(x, fs, da)
			merged.Disks = slow.Disks
			merged.Online = slow.Online
			merged.DockerAccess = slow.DockerAccess
			merged.Containers = slow.Containers
			merged.FailedUnits = slow.FailedUnits
			merged.SmartHealth = slow.SmartHealth
		}
		merged.TS = now.Unix()

		_ = writeHeartbeat(st.HeartbeatPath(), now)
		_ = st.WriteStatus(merged) // every fast tick: status.json is the live view

		if isSlowTick {
			// Persisted Sample + pruning + healthchecks stay on the slow
			// cadence: a Sample/ping every fast tick would be 12x today's
			// volume (default fast=5s, slow=60s) for no added signal, since
			// only the slow fields (and CPU/mem/swap/load1, unchanged in
			// their statistical meaning at either cadence) feed digests.
			_ = st.AppendSample(Sample{TS: merged.TS, CPU: merged.CPU, MemPct: merged.MemPct, SwapPct: merged.SwapPct, Load1: merged.Load1, TempC: merged.TempC, Disks: merged.Disks})
			pingHealthchecks(c.Healthchecks.URL, httpPing)

			// net_down interval tracking: Online is a slow-tier field, only
			// meaningfully updated on slow ticks, so only feed the tracker
			// when it was actually just refreshed.
			if ev, closed := net.Update(merged.Online, now.Unix()); closed {
				_ = st.AppendDown(ev)
			}
		}

		// anomalies: each channel's own Route (severity/kind filters,
		// CriticalOverridesQuiet) now decides delivery, so no gating happens
		// here beyond computing whether quiet hours are active. Re-evaluating
		// every fast tick against slow-tier values that haven't changed since
		// the last slow tick is harmless: AlertState's fire-once dedup means
		// re-evaluating an unchanged check is a no-op.
		events := alerts.Evaluate(buildChecks(merged, c, alerts.Active), baseline, c.BaselineSigma, now.Unix())
		disp := getDispatcher()
		quiet := inQuietHours(c.QuietHours, now)
		for _, e := range events {
			disp.Dispatch(eventToAlert(e, now.Unix()), quiet)
		}
		// scheduled digests bypass quiet hours, like the boot report.
		if matchDaily(c.Schedule.Daily, now, lastDaily) {
			lastDaily = now
			disp.Dispatch(Alert{Title: digestNow(st, now, 1, "📊 daily digest"), Severity: SevInfo, Kind: "fire", Source: "digest", Time: now.Unix()}, false)
		}
		if matchWeekly(c.Schedule.Weekly, now, lastWeekly) {
			lastWeekly = now
			disp.Dispatch(Alert{Title: digestNow(st, now, 7, "📆 weekly rollup"), Severity: SevInfo, Kind: "fire", Source: "digest", Time: now.Unix()}, false)
		}
		_ = baseline.Save(st.BaselinePath())
		_ = alerts.Save(st.AlertStatePath())
		if isSlowTick {
			_ = st.PruneOlderThan(30)
		}

		tick++
		<-fastTicker.C
	}
}

func digestNow(st *Store, now time.Time, days int, title string) string {
	since := now.AddDate(0, 0, -days).Unix()
	samples, _ := st.SamplesSince(since)
	downs, _ := st.DownSince(since)
	var window string
	switch days {
	case 1:
		window = "24h"
	case 7:
		window = "7d"
	default:
		window = fmt.Sprintf("%dd", days)
	}
	return buildDigest(title, window, samples, downs)
}

func pollLoop(getCfg func() *config.Config, setChatID func(string), st *Store, x Exec, fs FileSource, da dockerAccess) {
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
			snap := collectSnapshot(x, fs, &prevCPU, da)
			snap.TS = time.Now().Unix()
			_ = tg.SendMessage(handleCommand(u.Text, st, snap))
		}
	}
}
