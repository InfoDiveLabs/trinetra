package serverwatch

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/telegram"
)

func buildChecks(snap Snapshot, c *config.Config) []Check {
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
	return checks
}

func pingHealthchecks(url string, httpGet func(string) error) {
	if url == "" {
		return
	}
	_ = httpGet(url)
}

func httpPing(url string) error {
	resp, err := http.Get(url)
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
	cfg, _ := config.Load(cfgPath)
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
	go pollLoop(getCfg, st, x, fs, da)

	// sampler loop
	var lastDaily, lastWeekly time.Time
	ticker := time.NewTicker(time.Duration(getCfg().SampleInterval) * time.Second)
	defer ticker.Stop()
	for {
		c := getCfg()
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
		events := alerts.Evaluate(buildChecks(snap, c), baseline, c.BaselineSigma, now.Unix())
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
				_ = tg.SendMessage(dailyDigestNow(st, now))
			}
			if matchWeekly(c.Schedule.Weekly, now, lastWeekly) {
				lastWeekly = now
				_ = tg.SendMessage(dailyDigestNow(st, now))
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

func dailyDigestNow(st *Store, now time.Time) string {
	downs, _ := st.DownSince(now.AddDate(0, 0, -1).Unix())
	return buildDailyDigest(nil, downs)
}

func pollLoop(getCfg func() *config.Config, st *Store, x Exec, fs FileSource, da dockerAccess) {
	// The poller owns its CPUStat; it is never shared with the sampler goroutine.
	var prevCPU CPUStat
	offset := 0
	for {
		c := getCfg()
		tg := tgClient(c)
		if tg == nil {
			// no token yet; try to capture chat id is impossible without token.
			time.Sleep(5 * time.Second)
			continue
		}
		ups, err := tg.GetUpdates(offset, 50)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			// auto-capture chat id on first message
			if c.Telegram.ChatID == "" && u.ChatID != "" {
				c.Telegram.ChatID = u.ChatID
				_ = saveCfg(c)
			}
			snap := collectSnapshot(x, fs, &prevCPU, da)
			snap.TS = time.Now().Unix()
			_ = tg.SendMessage(handleCommand(u.Text, st, snap))
		}
	}
}
