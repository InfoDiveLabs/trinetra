// Package trinetra: coreapi_inproc.go implements core.API in-process,
// adapting the running daemon's live state (Snapshot, SampleStore, config,
// alert files) directly -- no HTTP/socket round-trip. This is the daemon's
// own consumer of the core.API contract: the
// trinetra-web binary, over the control socket, and, eventually, an
// in-process CLI path both read through this contract rather than reaching
// into trinetra internals themselves.
//
// This file never imports internal/web: core.API and its DTOs live in
// internal/core, which imports nothing but stdlib + internal/config (see
// internal/core/doc.go), so building this adapter never pulls internal/web's
// third-party dependencies into the default build. That's also why
// buildDashboardView/buildMonitoringView (below) live here rather than in
// internal/web itself: both this in-process API and the trinetra-web
// binary (which gets its data through core.API over the control socket)
// need the exact same Snapshot -> view projection, and this package never
// has to import internal/web to provide it.
package trinetra

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/version"
)

// buildDashboardView adapts a trinetra.Snapshot into a core.DashboardView.
func buildDashboardView(snap Snapshot) core.DashboardView {
	v := core.DashboardView{
		TS:      snap.TS,
		Online:  snap.Online,
		CPU:     snap.CPU,
		MemPct:  snap.MemPct,
		SwapPct: snap.SwapPct,
		Load1:   snap.Load1,
		Load5:   snap.Load5,
		Load15:  snap.Load15,
		TempC:   snap.TempC,
		Cores:   runtime.NumCPU(),
		Processes: core.ProcessCounts{
			Total:    snap.Processes.Total,
			Running:  snap.Processes.Running,
			Sleeping: snap.Processes.Sleeping,
			Zombie:   snap.Processes.Zombie,
		},
		UnitsFailed: len(snap.FailedUnits),
		UnitsTotal:  len(snap.Units),
	}

	for _, state := range snap.Containers {
		v.ContainersTotal++
		if state == "running" {
			v.ContainersRunning++
		}
	}
	v.TopCPUContainers, v.TopMemContainers = topContainers(snap.Containers, snap.ContainerStats)

	v.Disks = diskViews(snap.Disks, snap.DiskDetail)
	for _, d := range v.Disks {
		if d.UsagePct >= core.DiskCriticalPct {
			v.DisksCritical++
		}
	}

	v.NetIfaces = netIfaceViews(snap.NetRates)
	for _, n := range v.NetIfaces {
		v.NetRxBps += n.RxBps
		v.NetTxBps += n.TxBps
	}

	v.DegradedCollectors = degradedCollectorViews(snap.CollectorHealth)

	return v
}

// degradedCollectorViews projects the currently-failing slow-tier collectors (Fails > 0)
// into DTO views for the dashboard/ctl (#110).
func degradedCollectorViews(health map[string]CollectorStat) []core.CollectorHealthView {
	var out []core.CollectorHealthView
	for _, name := range slowCollectorKeys {
		st, ok := health[name]
		if !ok || st.Fails == 0 {
			continue
		}
		out = append(out, core.CollectorHealthView{
			Name: name, Fails: st.Fails, LastError: st.LastError, LastSuccessUnix: st.LastSuccessUnix,
		})
	}
	return out
}

// buildMonitoringView adapts a trinetra.Snapshot plus the daemon's
// current config (for the collect.services/collect.processes opt-in
// toggles -- see the doc atop core.MonitoringView) into a core.MonitoringView,
// the /monitoring detail page's counterpart to buildDashboardView above.
// Same CONCURRENCY contract as buildDashboardView: every access below is a
// read-only range/index against snap's map/slice fields, never an
// assignment into them.
//
// cfg may be nil (a caller without a config handy, e.g. some tests): both
// collector toggles then default to "disabled" -- the safer read when the
// actual setting is unknown, rather than assuming the collector ran and
// showing an empty table as if it deliberately reported zero units/processes.
// targetViewsFromTargets maps Discover/DiscoverLocal's []Target to
// []core.TargetView field for field, shared by inprocAPI.MonitorTargets and
// fileAPI.MonitorTargets so both core.API implementations report identical
// target lists from one mapping. It intentionally does NOT merge in
// config.Config.TargetEnabled/TargetThreshold overrides: MonitorTargets is
// the discovery half only (what does this host have), the same split
// Config()/ApplyConfig() already draw for the enable/threshold half (see
// core.API.MonitorTargets's doc).
func targetViewsFromTargets(targets []Target) []core.TargetView {
	out := make([]core.TargetView, 0, len(targets))
	for _, t := range targets {
		out = append(out, core.TargetView{ID: t.ID, Kind: t.Kind, Display: t.Display, Available: t.Available})
	}
	return out
}

func buildMonitoringView(snap Snapshot, cfg *config.Config) core.MonitoringView {
	var v core.MonitoringView

	if len(snap.Containers) > 0 {
		names := make([]string, 0, len(snap.Containers))
		for name := range snap.Containers {
			names = append(names, name)
		}
		sort.Strings(names)
		v.Containers = make([]core.MonitoringContainerView, 0, len(names))
		for _, name := range names {
			row := core.MonitoringContainerView{Name: name, State: snap.Containers[name]}
			if cs, ok := snap.ContainerStats[name]; ok {
				row.CPUPct = cs.CPUPct
				row.MemMiB = cs.MemMiB
				row.NetRxMB = cs.NetRxMB
				row.NetTxMB = cs.NetTxMB
				row.HasStats = true
			}
			v.Containers = append(v.Containers, row)
		}
	}

	// FailedUnits is always collected (systemctl --failed, the same alerting input service:*
	// checks use) regardless of collect.services -- copy it out.
	if len(snap.FailedUnits) > 0 {
		v.FailedUnits = append([]string(nil), snap.FailedUnits...)
	}

	if cfg != nil && cfg.ServicesEnabled() {
		v.UnitsEnabled = true
		v.Units = make([]core.MonitoringUnitView, 0, len(snap.Units))
		for _, u := range snap.Units {
			v.Units = append(v.Units, core.MonitoringUnitView{
				Name: u.Name, Load: u.Load, Active: u.Active, Sub: u.Sub, Description: u.Description,
			})
		}
		sort.Slice(v.Units, func(i, j int) bool { return v.Units[i].Name < v.Units[j].Name })
	}

	if cfg != nil && cfg.ProcessesEnabled() {
		v.ProcessesEnabled = true
		v.ProcessesTotal = snap.Processes.Total
		v.Processes = make([]core.MonitoringProcessView, 0, len(snap.Processes.Top))
		for _, p := range snap.Processes.Top {
			v.Processes = append(v.Processes, core.MonitoringProcessView{
				PID: p.PID, Name: p.Name, State: p.State, CPUPct: p.CPUPct, MemMiB: p.MemMiB, Threads: p.Threads,
			})
		}
	}

	v.Disks = monitoringDiskViews(snap.Disks, snap.DiskDetail)

	return v
}

// monitoringDiskViews is diskViews' (below) counterpart for the Monitoring page's richer
// filesystems table: same mount-sorted merge of Snapshot.Disks + Snapshot.DiskDetail.
func monitoringDiskViews(disks map[string]float64, detail map[string]DiskDetail) []core.MonitoringDiskView {
	if len(disks) == 0 {
		return nil
	}
	mounts := make([]string, 0, len(disks))
	for m := range disks {
		mounts = append(mounts, m)
	}
	sort.Strings(mounts)

	out := make([]core.MonitoringDiskView, 0, len(mounts))
	for _, m := range mounts {
		dv := core.MonitoringDiskView{Mount: m, UsagePct: disks[m]}
		if dd, ok := detail[m]; ok {
			dv.Device = dd.Device
			dv.FSType = dd.FsType
			dv.InodePct = dd.InodePct
			dv.FreeBytes = dd.FreeBytes
			dv.SizeBytes = dd.SizeBytes
			dv.DaysToFull = dd.DaysToFull
			dv.DaysToFullKnown = dd.DaysToFullKnown
		}
		out = append(out, dv)
	}
	return out
}

// topContainers builds the "top by CPU%"/"top by memory" container lists (each capped to
// dashboardTopContainerCap) from snap.ContainerStats -- read-only.
func topContainers(states map[string]string, stats map[string]ContainerStat) (byCPU, byMem []core.ContainerView) {
	if len(stats) == 0 {
		return nil, nil
	}
	all := make([]core.ContainerView, 0, len(stats))
	for name, cs := range stats {
		all = append(all, core.ContainerView{
			Name:   name,
			State:  states[name],
			CPUPct: cs.CPUPct,
			MemMiB: cs.MemMiB,
		})
	}

	byCPUAll := append([]core.ContainerView(nil), all...)
	sort.Slice(byCPUAll, func(i, j int) bool {
		if byCPUAll[i].CPUPct != byCPUAll[j].CPUPct {
			return byCPUAll[i].CPUPct > byCPUAll[j].CPUPct
		}
		return byCPUAll[i].Name < byCPUAll[j].Name
	})
	byMemAll := append([]core.ContainerView(nil), all...)
	sort.Slice(byMemAll, func(i, j int) bool {
		if byMemAll[i].MemMiB != byMemAll[j].MemMiB {
			return byMemAll[i].MemMiB > byMemAll[j].MemMiB
		}
		return byMemAll[i].Name < byMemAll[j].Name
	})

	return capContainers(byCPUAll), capContainers(byMemAll)
}

// dashboardTopContainerCap mirrors core.dashboardTopN (unexported there, so duplicated here
// as a plain constant rather than reached into across the package boundary).
const dashboardTopContainerCap = 4

func capContainers(all []core.ContainerView) []core.ContainerView {
	if len(all) > dashboardTopContainerCap {
		return all[:dashboardTopContainerCap]
	}
	return all
}

// diskViews merges Snapshot.Disks (mount -> usage%, always populated) with
// Snapshot.DiskDetail (mount -> device/size/free/fill-projection, additive)
// into one sorted-by-mount slice -- read-only against both maps, see
// buildDashboardView's CONCURRENCY note.
func diskViews(disks map[string]float64, detail map[string]DiskDetail) []core.DiskView {
	if len(disks) == 0 {
		return nil
	}
	mounts := make([]string, 0, len(disks))
	for m := range disks {
		mounts = append(mounts, m)
	}
	sort.Strings(mounts)

	out := make([]core.DiskView, 0, len(mounts))
	for _, m := range mounts {
		dv := core.DiskView{Mount: m, UsagePct: disks[m]}
		if dd, ok := detail[m]; ok {
			dv.Device = dd.Device
			dv.FreeBytes = dd.FreeBytes
			dv.SizeBytes = dd.SizeBytes
			dv.DaysToFull = dd.DaysToFull
			dv.DaysToFullKnown = dd.DaysToFullKnown
		}
		out = append(out, dv)
	}
	return out
}

// netIfaceViews converts Snapshot.NetRates (interface -> IfaceRate) into a slice sorted by
// interface name -- read-only, see buildDashboardView's CONCURRENCY note.
func netIfaceViews(rates map[string]IfaceRate) []core.NetIfaceView {
	if len(rates) == 0 {
		return nil
	}
	names := make([]string, 0, len(rates))
	for n := range rates {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]core.NetIfaceView, 0, len(names))
	for _, n := range names {
		out = append(out, core.NetIfaceView{Name: n, RxBps: rates[n].RxBps, TxBps: rates[n].TxBps})
	}
	return out
}

// errCoreNotImplemented was the shared sentinel both core.API implementations' Subscribe
// returned before the A2 live-push work: it is no longer used by Subscribe.
var errCoreNotImplemented = errors.New("trinetra: core.API method not implemented yet")

// errStreamRequiresDaemon is returned by Subscribe when there is no live daemon event bus
// to subscribe to: fileAPI (coreapi_file.go) always hits this.
var errStreamRequiresDaemon = errors.New("trinetra: live event streaming requires a running daemon")

// inprocAPI is the in-process core.API implementation: it reads the running daemon's own
// state directly.
type inprocAPI struct {
	getSnap  func() Snapshot
	getCfg   func() *config.Config
	store    SampleStore
	stateDir string
	// reload is the daemon's own reload closure (cmdDaemon's `reload` in daemon.go: saveCfg
	// then the applyConfig pointer-swap) -- ApplyConfig below just calls through to it.
	reload func(*config.Config) error
	// bus is the daemon's live event bus (eventbus.go): Subscribe below delegates straight to
	// bus.Subscribe(). nil when this inprocAPI was built without a live daemon behind it.
	bus *eventBus
	// enroll is the shared Telegram enrollment-pin holder cmdDaemon also hands to pollLoop
	// (daemon.go, enroll.go) -- EnrollmentPIN below just reads through it.
	enroll *enrollState
	// updateMu/updateInProgress/updateLastErr track a background UpdateApply/UpdateRollback
	// goroutine: UpdateApply and UpdateRollback below run their fast checks synchronously.
	updateMu         sync.Mutex
	updateInProgress bool
	updateLastErr    string
	// newUpdaterFn is a test seam: it lets a test substitute a fully-controlled updater --
	// e.g. one whose Source blocks until the test releases it.
	newUpdaterFn func(*config.Config) updater
}

// newUpdaterFor returns a.newUpdaterFn(c) if set (test seam, see the field's
// doc), otherwise the real package-level newUpdater(c).
func (a *inprocAPI) newUpdaterFor(c *config.Config) updater {
	u := newUpdater(c)
	if a.newUpdaterFn != nil {
		u = a.newUpdaterFn(c)
	}
	u.actor = socketActor
	return u
}

// errUpdateAlreadyRunning is returned by UpdateApply/UpdateRollback while a previous call's
// background goroutine (beginUpdateWork) is unfinished.
var errUpdateAlreadyRunning = errors.New("update: an update is already in progress")

// beginUpdateWork claims the single in-flight apply/rollback slot, refusing with
// errUpdateAlreadyRunning if one is already running.
func (a *inprocAPI) beginUpdateWork() (done func(error), err error) {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	if a.updateInProgress {
		return nil, errUpdateAlreadyRunning
	}
	a.updateInProgress = true
	return func(opErr error) {
		a.updateMu.Lock()
		defer a.updateMu.Unlock()
		a.updateInProgress = false
		if opErr != nil {
			a.updateLastErr = opErr.Error()
		} else {
			a.updateLastErr = ""
		}
	}, nil
}

// abortUpdateWork releases the slot beginUpdateWork claimed WITHOUT recording anything in
// LastError: used when a synchronous preflight check.
func (a *inprocAPI) abortUpdateWork() {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	a.updateInProgress = false
}

// updateProgress reads the current InProgress/LastError pair for
// UpdateStatus, under the same mutex beginUpdateWork/its done func use.
func (a *inprocAPI) updateProgress() (inProgress bool, lastErr string) {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	return a.updateInProgress, a.updateLastErr
}

// newInprocAPI builds a core.API backed directly by the running daemon's state:
// getSnap/getCfg are the same race-safe closures cmdDaemon.
func newInprocAPI(getSnap func() Snapshot, getCfg func() *config.Config, store SampleStore, stateDir string, reload func(*config.Config) error, bus *eventBus, enroll *enrollState) core.API {
	return &inprocAPI{getSnap: getSnap, getCfg: getCfg, store: store, stateDir: stateDir, reload: reload, bus: bus, enroll: enroll}
}

// alertStatePath/alertLogPath mirror Store.AlertStatePath/Store.AlertLogPath (store.go)
// exactly -- same filenames under the same state directory.
func (a *inprocAPI) alertStatePath() string { return filepath.Join(a.stateDir, "alerts.json") }
func (a *inprocAPI) alertLogPath() string   { return filepath.Join(a.stateDir, "alertlog.jsonl") }

// Snapshot implements core.API: it projects the daemon's live Snapshot via
// buildDashboardView, then computes Availability fresh (real "now", real
// events overlapping the trailing 24h) on top. a itself satisfies
// core.EventsSource (its Events method below has the exact signature
// ComputeAvailability wants), so no separate adapter type is
// needed; a nil a.store just makes a.Events degrade to "no events" the same
// way a nil store degrades everywhere else in this file.
func (a *inprocAPI) Snapshot() (core.DashboardView, error) {
	v := buildDashboardView(a.getSnap())
	v.Availability = core.ComputeAvailability(a, time.Now().Unix())
	return v, nil
}

// Monitoring implements core.API via the re-homed buildMonitoringView.
func (a *inprocAPI) Monitoring() (core.MonitoringView, error) {
	return buildMonitoringView(a.getSnap(), a.getCfg()), nil
}

// Series implements core.API: core.ResAuto resolves to raw-vs-1m via the existing
// PickResolution.
func (a *inprocAPI) Series(metric string, from, to int64, res core.Resolution) ([]core.SeriesPoint, error) {
	if a.store == nil {
		return nil, nil
	}

	var swRes Resolution
	switch res {
	case core.ResRaw:
		swRes = ResRaw
	case core.Res1m:
		swRes = Res1m
	default: // core.ResAuto (or any future/unrecognized value): let the picker decide
		rawRetention := defaultRawRetention
		if a.getCfg != nil {
			if cfg := a.getCfg(); cfg != nil {
				rawRetention = configuredRawRetention(cfg)
			}
		}
		swRes = PickResolution(from, to, time.Now().Unix(), rawRetention)
	}

	pts, err := a.store.Query(metric, from, to, swRes)
	if err != nil {
		return nil, err
	}
	out := make([]core.SeriesPoint, len(pts))
	for i, p := range pts {
		out[i] = core.SeriesPoint{TS: p.TS, Min: p.Min, Avg: p.Avg, Max: p.Max}
	}
	return out, nil
}

// Events implements core.API (and, via the same signature, core.EventsSource for Snapshot's
// Availability computation above).
func (a *inprocAPI) Events(from, to int64) ([]core.DownEventView, error) {
	if a.store == nil {
		return nil, nil
	}
	evs, err := a.store.Events(from, to)
	if err != nil {
		return nil, err
	}
	out := make([]core.DownEventView, len(evs))
	for i, e := range evs {
		out[i] = core.DownEventView{Type: e.Type, Start: e.Start, End: e.End, DurationSec: e.DurationSec}
	}
	return out, nil
}

// ActiveAlerts implements core.API: it loads alerts.json (LoadAlertState) and maps it via
// the shared activeAlertRecords helper (coreapi_alerts.go).
func (a *inprocAPI) ActiveAlerts() ([]core.AlertRecord, error) {
	state := LoadAlertState(a.alertStatePath(), osFS{})
	return activeAlertRecords(state), nil
}

// AlertHistory implements core.API: it delegates to the shared alertHistoryRecords helper
// (coreapi_alerts.go), the same one fileAPI.AlertHistory (coreapi_file.go) calls.
func (a *inprocAPI) AlertHistory(since int64, limit int) ([]core.AlertRecord, error) {
	return alertHistoryRecords(NewAlertLog(a.alertLogPath()), since, limit)
}

// Config implements core.API: it just calls through to getCfg, the same
// race-safe (against SIGHUP reload) accessor the rest of the daemon uses.
func (a *inprocAPI) Config() (*config.Config, error) {
	return a.getCfg(), nil
}

// Doctor implements core.API via the shared buildDoctorReport (systemd.go), the same probe
// orchestration `trinetra doctor` (cmdDoctor) runs -- x/fs are the real osExec{}/osFS{}.
func (a *inprocAPI) Doctor() (core.DoctorReport, error) {
	return buildDoctorReport(osExec{}, osFS{}, a.getCfg(), a.store), nil
}

// HostInfo implements core.API (#100).
func (a *inprocAPI) HostInfo() (core.HostInfoView, error) {
	return buildHostInfoView(collectHostInfoFor(a.getCfg()), time.Now().Unix()), nil
}

// Version implements core.API: the daemon's own build-stamped version (#107).
func (a *inprocAPI) Version() (string, error) { return version.String(), nil }

// updateCheckTimeout bounds UpdateCheck over the control socket.
const updateCheckTimeout = 20 * time.Second

// UpdateStatus implements core.API: this host's persisted self-update posture via
// coreUpdateStatus.
func (a *inprocAPI) UpdateStatus() (core.UpdateStatusView, error) {
	view, err := coreUpdateStatus(a.getCfg())
	if err != nil {
		return view, err
	}
	view.InProgress, view.LastError = a.updateProgress()
	return view, nil
}

// UpdateCheck implements core.API: fetch/verify the channel's latest release, record the
// outcome and return the status view.
func (a *inprocAPI) UpdateCheck(ctx context.Context) (core.UpdateStatusView, error) {
	ctx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()
	return coreUpdateCheck(ctx, a.getCfg())
}

// UpdateApply implements core.API over the control socket.
func (a *inprocAPI) UpdateApply(ctx context.Context, version string) error {
	c := a.getCfg()
	u := a.newUpdaterFor(c)
	opts := applyOptions{Version: version}

	done, err := a.beginUpdateWork()
	if err != nil {
		return err
	}
	if err := u.preflightApply(c, opts); err != nil {
		a.abortUpdateWork()
		return err
	}
	go func() {
		_, applyErr := u.apply(context.Background(), c, opts)
		done(applyErr)
	}()
	return nil
}

// UpdateRollback implements core.API over the control socket, the same
// claim-the-slot-first-then-preflight split as UpdateApply above.
func (a *inprocAPI) UpdateRollback() error {
	c := a.getCfg()
	u := a.newUpdaterFor(c)

	done, err := a.beginUpdateWork()
	if err != nil {
		return err
	}
	if err := u.preflightRollback(); err != nil {
		a.abortUpdateWork()
		return err
	}
	go func() {
		done(u.rollback())
	}()
	return nil
}

// ContainerLogs implements core.API: it snapshots the last `lines` log lines of a live
// docker container.
func (a *inprocAPI) ContainerLogs(name string, lines int) (string, error) {
	return collectContainerLogs(osExec{}, osFS{}, name, lines)
}

// buildHostInfoView adapts the trinetra HostInfo into the core DTO, deriving UptimeSec from
// BootTime and nowUnix (a cached BootTime therefore yields a correct uptime on every read).
func buildHostInfoView(h HostInfo, nowUnix int64) core.HostInfoView {
	var uptime int64
	if h.BootTime > 0 && nowUnix > h.BootTime {
		uptime = nowUnix - h.BootTime
	}
	disks := make([]core.HostDiskView, 0, len(h.Disks))
	for _, d := range h.Disks {
		disks = append(disks, core.HostDiskView{
			Device:     d.Device,
			Model:      d.Model,
			Rotational: d.Rotational,
			SizeBytes:  d.SizeBytes,
			FSType:     d.FSType,
			Mount:      d.Mount,
		})
	}
	return core.HostInfoView{
		Hostname:      h.Hostname,
		Kernel:        h.Kernel,
		OS:            h.OS,
		CPUModel:      h.CPUModel,
		CPUSockets:    h.CPUSockets,
		CPUCores:      h.CPUCores,
		CPUThreads:    h.CPUThreads,
		CPUBaseMHz:    h.CPUBaseMHz,
		MemTotalBytes: h.MemTotalBytes,
		BootTime:      h.BootTime,
		UptimeSec:     uptime,
		LocalIP:       h.LocalIP,
		PublicIP:      h.PublicIP,
		Disks:         disks,
	}
}

// collectContainerLogs validates name and returns a `docker logs --tail lines` snapshot for
// it.
func collectContainerLogs(x Exec, fs FileSource, name string, lines int) (string, error) {
	if !validContainerName(name) {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	da := probeDocker(x, fs)
	if !da.available {
		return "", errors.New("docker is not available on this host")
	}
	list, err := da.list(x)
	if err != nil {
		return "", err
	}
	known := false
	for _, c := range list {
		if c.Name == name {
			known = true
			break
		}
	}
	if !known {
		return "", fmt.Errorf("no such container %q", name)
	}
	return da.logs(x, name, lines)
}

// collectHostInfoFor collects the host inventory and, when cfg opts into the public-IP
// lookup (collect.public_ip, #102), performs that one outbound call.
func collectHostInfoFor(cfg *config.Config) HostInfo {
	h := collectHostInfo(osExec{}, osFS{})
	if cfg != nil && cfg.PublicIPEnabled() {
		h.PublicIP = lookupPublicIP()
	}
	return h
}

// EnrollmentPIN implements core.API: it reads through a.enroll (enroll.go) against the
// daemon's current live config, the exact same call pollLoop.
func (a *inprocAPI) EnrollmentPIN(ctx context.Context) (pin string, enrolled bool, err error) {
	pin, enrolled = a.enroll.PIN(a.getCfg())
	return pin, enrolled, nil
}

// MonitorTargets implements core.API: it runs DiscoverLocal() (the daemon's
// own osExec{}/osFS{}-backed probes -- docker ps / df -PT / smartctl --scan
// / the thermal-zone glob) in the daemon's own process, so a socket caller
// (ctl's monitor-thresholds screen) sees exactly what this host's daemon can
// see, including anything gated behind the daemon's own root/sudo access
// that a separate, less-privileged CLI process (fileAPI.MonitorTargets,
// coreapi_file.go) might not.
func (a *inprocAPI) MonitorTargets(ctx context.Context) ([]core.TargetView, error) {
	return targetViewsFromTargets(DiscoverLocal()), nil
}

// ApplyConfig implements core.API: it delegates straight to a.reload, the daemon's own
// save-then-apply closure (see the field's doc), now reachable over the control socket.
func (a *inprocAPI) ApplyConfig(c *config.Config) error { return a.reload(c) }

// AckAlert implements core.API: it loads alerts.json (LoadAlertState, same helper
// ActiveAlerts/AlertHistory above use), acks key via AlertState.Ack (anomaly.go).
func (a *inprocAPI) AckAlert(key string) error {
	statePath := a.alertStatePath()
	state := LoadAlertState(statePath, osFS{})
	if err := state.Ack(key, time.Now().Unix()); err != nil {
		return err
	}
	return state.Save(statePath)
}

// UnackAlert implements core.API: AckAlert's mirror image, via AlertState.Unack.
func (a *inprocAPI) UnackAlert(key string) error {
	statePath := a.alertStatePath()
	state := LoadAlertState(statePath, osFS{})
	if err := state.Unack(key); err != nil {
		return err
	}
	return state.Save(statePath)
}

// TestChannel implements core.API: it calls sendTestNotification (channel.go) against the
// LIVE config (a.getCfg(), race-safe against a concurrent SIGHUP/Reload).
func (a *inprocAPI) TestChannel(name string) error {
	return sendTestNotification(a.getCfg(), name, "web")
}

// ValidateChannel implements core.API: it calls buildNotifier (channels.go)
// against cc and the LIVE config (a.getCfg(), same race-safe accessor
// TestChannel above uses) and reports only whether a Notifier could be
// built, not sending anything. This validates cc against the daemon's
// current saved config -- a caller mid-edit of an unsaved config (e.g. a
// telegram channel meant to lean on a global token/chat_id being changed in
// the same in-flight edit) is checked against what's live now, not the
// edit-in-progress; see the ValidateChannel doc on core.API for that
// accepted limitation.
func (a *inprocAPI) ValidateChannel(cc config.ChannelConfig) error {
	_, err := buildNotifier(cc, a.getCfg())
	return err
}

// Subscribe implements core.API: it registers a subscription on the live daemon bus.
func (a *inprocAPI) Subscribe(ctx context.Context) (<-chan core.Event, error) {
	if a.bus == nil {
		return nil, errStreamRequiresDaemon
	}
	ch, cancel := a.bus.Subscribe()
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return ch, nil
}
