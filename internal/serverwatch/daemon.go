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

func collectSnapshot(x Exec, fs FileSource, prev *CPUStat, da dockerAccess) Snapshot {
	var snap Snapshot
	snap.DockerAccess = da.method
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
	if out, err := x.Run("df", "-PB1"); err == nil {
		snap.Disks = map[string]float64{}
		for _, d := range mustDF(string(out)) {
			if isRealMount(d.Mount) {
				snap.Disks[d.Mount] = d.UsedPct
			}
		}
	}
	if zones, _ := fs.Glob("/sys/class/thermal/thermal_zone*/temp"); len(zones) > 0 {
		if b, err := fs.Read(zones[0]); err == nil {
			snap.TempC, _ = parseThermal(string(b))
		}
	}
	snap.Online = checkOnline(defaultConnHosts, realDial)

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
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, sighup)
	go func() {
		for range hup {
			if c, err := config.Load(cfgPath); err == nil {
				mu.Lock()
				cfg = c
				mu.Unlock()
				fmt.Fprintln(stdout, "config reloaded")
			}
		}
	}()
	getCfg := func() *config.Config { mu.RLock(); defer mu.RUnlock(); return cfg }
	// setChatID race-safely records an auto-captured chat id by pointer-SWAPPING
	// the shared cfg (mirroring the SIGHUP reload above). Never mutate a field on
	// the in-use struct: getCfg readers read fields after releasing the RLock.
	setChatID := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		nc := *cfg // shallow struct copy
		nc.Telegram.ChatID = id
		cfg = &nc // swap pointer; existing readers keep old struct
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
			if tg := tgClient(c0); tg != nil {
				snap := collectSnapshot(x, fs, &prevCPU, da)
				_ = tg.SendMessage(formatBootReport([]DownEvent{ev}, renderStatus(snap)))
			}
		}
	}

	// telegram long-poller (owns its own prevCPU internally)
	go pollLoop(getCfg, setChatID, st, x, fs, da)

	// sampler loop
	var lastDaily, lastWeekly time.Time
	ticker := time.NewTicker(time.Duration(getCfg().SampleInterval) * time.Second)
	defer ticker.Stop()
	lastInterval := getCfg().SampleInterval
	for {
		c := getCfg()
		if c.SampleInterval != lastInterval {
			ticker.Reset(time.Duration(c.SampleInterval) * time.Second)
			lastInterval = c.SampleInterval
		}
		now := clock.Now()
		snap := collectSnapshot(x, fs, &prevCPU, da)
		snap.TS = now.Unix()

		_ = writeHeartbeat(st.HeartbeatPath(), now)
		_ = st.AppendSample(Sample{TS: snap.TS, CPU: snap.CPU, MemPct: snap.MemPct, SwapPct: snap.SwapPct, Load1: snap.Load1, TempC: snap.TempC, Disks: snap.Disks})
		_ = st.WriteStatus(snap)
		pingHealthchecks(c.Healthchecks.URL, httpPing)

		// net_down interval tracking
		if ev, closed := net.Update(snap.Online, now.Unix()); closed {
			_ = st.AppendDown(ev)
		}

		// anomalies
		events := alerts.Evaluate(buildChecks(snap, c, alerts.Active), baseline, c.BaselineSigma, now.Unix())
		if tg := tgClient(c); tg != nil {
			quiet := inQuietHours(c.QuietHours, now)
			for _, e := range events {
				if quiet && !(e.Critical && c.CriticalOverridesQuiet) {
					continue
				}
				if e.Kind == "fire" {
					_ = tg.SendMessage(formatFire(e))
				} else {
					_ = tg.SendMessage(formatRecover(e))
				}
			}
			// scheduled digests
			if matchDaily(c.Schedule.Daily, now, lastDaily) {
				lastDaily = now
				_ = tg.SendMessage(digestNow(st, now, 1, "📊 daily digest"))
			}
			if matchWeekly(c.Schedule.Weekly, now, lastWeekly) {
				lastWeekly = now
				_ = tg.SendMessage(digestNow(st, now, 7, "📆 weekly rollup"))
			}
		}
		_ = baseline.Save(st.BaselinePath())
		_ = alerts.Save(st.AlertStatePath())
		_ = st.PruneOlderThan(30)

		<-ticker.C
	}
}

func tgClient(c *config.Config) *telegram.Client {
	if c.Telegram.Token == "" || c.Telegram.ChatID == "" {
		return nil
	}
	return telegram.New(c.Telegram.Token, c.Telegram.ChatID)
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
