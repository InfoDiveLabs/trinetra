// Package serverwatch: coreapi_inproc.go implements core.API in-process,
// adapting the running daemon's live state (Snapshot, SampleStore, config,
// alert files) directly -- no HTTP/socket round-trip. This is the daemon's
// own consumer of the core.API contract (internal/core/api.go, task 3): the
// serverwatch-web binary, over the control socket, and, eventually, an
// in-process CLI path both read through this contract rather than reaching
// into serverwatch internals themselves.
//
// This file never imports internal/web: core.API and its DTOs live in
// internal/core, which imports nothing but stdlib + internal/config (see
// internal/core/doc.go), so building this adapter never pulls internal/web's
// third-party dependencies into the default build. That's also why
// buildDashboardView/buildMonitoringView (below) live here rather than in
// internal/web itself: both this in-process API and the serverwatch-web
// binary (which gets its data through core.API over the control socket)
// need the exact same Snapshot -> view projection, and this package never
// has to import internal/web to provide it.
package serverwatch

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// buildDashboardView adapts a serverwatch.Snapshot (native to this package)
// into a core.DashboardView -- the Task 8 (#64) resolution of the Task 1
// placeholder that made Deps.Snapshot return `any`, re-homed here (task 4)
// so the default build can construct one too, not just the serverwatch-web
// binary.
//
// CONCURRENCY: snap is a value the caller (d.Snapshot(), ultimately
// latestSnapshot(), or inprocAPI.getSnap) already copied out of snapshotHub
// -- see that function's doc. Its map fields (Disks/Containers/
// ContainerStats/etc.), however, are still the very maps the sampler loop's
// last-published *Snapshot points at: copying a struct copies map HEADERS,
// not their contents. That's exactly what snapshotHub's "collectors replace
// map fields wholesale, never mutate in place" contract makes safe -- but
// only as long as every reader, including this function, is READ-ONLY
// against those maps. Every access below is a plain read (range/index);
// nothing here ever assigns into snap.Disks, snap.Containers,
// snap.ContainerStats, snap.NetRates, or snap.DiskDetail -- doing so would
// race against the sampler loop the next time it replaces that field. New
// slices built here (v.Disks, v.NetIfaces, v.TopCPUContainers, ...) are this
// function's own, never aliases into snap.
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

	return v
}

// buildMonitoringView adapts a serverwatch.Snapshot plus the daemon's
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

	// FailedUnits is always collected (systemctl --failed, the same
	// alerting input service:* checks use) regardless of collect.services --
	// copy it out (never alias snap.FailedUnits) so this stays read-only
	// against the published Snapshot, same as every other field here.
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

// monitoringDiskViews is diskViews' (below) counterpart for the Monitoring
// page's richer filesystems table: same mount-sorted merge of Snapshot.Disks
// + Snapshot.DiskDetail, but keeping FSType/InodePct too (which
// buildDashboardView's compact table doesn't need).
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

// topContainers builds the "top by CPU%"/"top by memory" container lists
// (each capped to dashboardTopContainerCap) from snap.ContainerStats --
// read-only, see buildDashboardView's CONCURRENCY note. states supplies each
// container's running/exited/... state (snap.Containers); a stats entry
// with no matching state (a container docker stats saw but the plain state
// listing didn't, a narrow race between the two shell-outs within one slow
// tick) just renders with an empty State rather than being dropped.
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

// dashboardTopContainerCap mirrors core.dashboardTopN (unexported there, so
// duplicated here as a plain constant rather than reached into across the
// package boundary) -- the mockup dashboard.html's "Top containers" panels
// show exactly 4 rows each.
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

// netIfaceViews converts Snapshot.NetRates (interface -> IfaceRate) into a
// slice sorted by interface name -- read-only, see buildDashboardView's
// CONCURRENCY note.
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

// errCoreNotImplemented is the shared sentinel Subscribe returns on both
// core.API implementations (inprocAPI here, fileAPI in coreapi_file.go) --
// the one method still deferred, to S5 (the alerting event-bus inversion).
// Every other method (reads: tasks 4/6; writes/Doctor: tasks 7/8) is
// implemented for real.
var errCoreNotImplemented = errors.New("serverwatch: core.API method not implemented yet")

// inprocAPI is the in-process core.API implementation: it reads the running
// daemon's own state directly (no socket/HTTP hop) by holding closures onto
// the daemon's live Snapshot/config plus its SampleStore and state
// directory. newInprocAPI is the only constructor; every field is set once
// at construction and never mutated afterward, so the type needs no lock of
// its own -- concurrency safety instead rests on getSnap/getCfg/store being
// individually safe for concurrent use (getSnap is backed by snapshotHub's
// atomic.Pointer, getCfg mirrors the daemon's own SIGHUP-safe config
// access, and SampleStore implementations are documented safe for
// concurrent use).
type inprocAPI struct {
	getSnap  func() Snapshot
	getCfg   func() *config.Config
	store    SampleStore
	stateDir string
	// reload is the daemon's own reload closure (cmdDaemon's `reload` in
	// daemon.go: saveCfg then the applyConfig pointer-swap) -- ApplyConfig
	// below just calls through to it, so a config posted through core.API
	// takes effect exactly the way cmdDaemon's own reload always has:
	// persisted to disk, then applied in-process without a SIGHUP round-trip.
	reload func(*config.Config) error
	// enroll is the shared Telegram enrollment-pin holder cmdDaemon also
	// hands to pollLoop (daemon.go, enroll.go) -- EnrollmentPIN below just
	// reads through it, so a socket caller sees the exact pin the poll loop
	// is matching /start <pin> against, not a separately generated one.
	enroll *enrollState
}

// newInprocAPI builds a core.API backed directly by the running daemon's
// state: getSnap/getCfg are the same race-safe closures cmdDaemon (daemon.go)
// hands the control socket (latestSnapshot/getCfg), store is the daemon's
// SampleStore (nil in store-writes-disabled mode -- every read method below
// degrades to "no data" rather than panicking), stateDir is the directory
// alerts.json/alertlog.jsonl live in (mirroring Store's own
// AlertStatePath/AlertLogPath, store.go), reload is cmdDaemon's own
// save-then-apply closure that ApplyConfig delegates to, and enroll is the
// same enrollState instance cmdDaemon hands to pollLoop.
func newInprocAPI(getSnap func() Snapshot, getCfg func() *config.Config, store SampleStore, stateDir string, reload func(*config.Config) error, enroll *enrollState) core.API {
	return &inprocAPI{getSnap: getSnap, getCfg: getCfg, store: store, stateDir: stateDir, reload: reload, enroll: enroll}
}

// alertStatePath/alertLogPath mirror Store.AlertStatePath/Store.AlertLogPath
// (store.go) exactly -- same filenames under the same state directory --
// since inprocAPI is handed a bare stateDir rather than a *Store.
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

// Series implements core.API: core.ResAuto resolves to raw-vs-1m via the
// existing PickResolution (the same age/config-dependent picker) against
// the daemon's configured storage.raw_retention; core.ResRaw/core.Res1m map
// straight onto their serverwatch.Resolution counterparts. A nil store (
// store-writes-disabled mode) degrades to an empty result rather than a
// panic, mirroring every other store-backed method here.
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

// Events implements core.API (and, via the same signature, core.EventsSource
// for Snapshot's Availability computation above): it delegates to the
// SampleStore's Events and widens each DownEvent into a core.DownEventView.
// A nil store degrades to an empty result, not a panic.
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

// ActiveAlerts implements core.API: it loads alerts.json (LoadAlertState)
// and maps it via the shared activeAlertRecords helper (coreapi_alerts.go),
// the same one fileAPI.ActiveAlerts (coreapi_file.go) calls -- see that
// helper's doc for the field-mapping rationale. A missing/corrupt
// alerts.json is not an error -- LoadAlertState already degrades that to an
// empty AlertState, mirroring every other read here.
func (a *inprocAPI) ActiveAlerts() ([]core.AlertRecord, error) {
	state := LoadAlertState(a.alertStatePath(), osFS{})
	return activeAlertRecords(state), nil
}

// AlertHistory implements core.API: it delegates to the shared
// alertHistoryRecords helper (coreapi_alerts.go), the same one
// fileAPI.AlertHistory (coreapi_file.go) calls -- see that helper's doc for
// the field-mapping rationale (newest-first, limit cap, Acked always false).
func (a *inprocAPI) AlertHistory(since int64, limit int) ([]core.AlertRecord, error) {
	return alertHistoryRecords(NewAlertLog(a.alertLogPath()), since, limit)
}

// Config implements core.API: it just calls through to getCfg, the same
// race-safe (against SIGHUP reload) accessor the rest of the daemon uses.
func (a *inprocAPI) Config() (*config.Config, error) {
	return a.getCfg(), nil
}

// Doctor implements core.API via the shared buildDoctorReport (systemd.go),
// the same probe orchestration `serverwatch doctor` (cmdDoctor) runs --
// x/fs are the real osExec{}/osFS{} (there is no injected Exec/FileSource on
// inprocAPI, same as cmdDoctor itself), but the SampleStore is this
// inprocAPI's own live a.store (possibly nil in store-writes-disabled mode,
// which buildDoctorReport already degrades to a "unavailable" StoreStats)
// rather than a freshly-opened one -- the daemon already has it open, unlike
// fileAPI's CLI-process Doctor below.
func (a *inprocAPI) Doctor() (core.DoctorReport, error) {
	return buildDoctorReport(osExec{}, osFS{}, a.getCfg(), a.store), nil
}

// EnrollmentPIN implements core.API: it reads through a.enroll (enroll.go)
// against the daemon's current live config, the exact same call pollLoop
// (daemon.go) makes each iteration -- so a socket caller (`telegram
// set-token`, ctl) always sees the pin the daemon will actually accept in
// "/start <pin>", never a separately generated one.
func (a *inprocAPI) EnrollmentPIN(ctx context.Context) (pin string, enrolled bool, err error) {
	pin, enrolled = a.enroll.PIN(a.getCfg())
	return pin, enrolled, nil
}

// ApplyConfig implements core.API: it delegates straight to a.reload, the
// daemon's own save-then-apply closure (see the field's doc), now reachable
// over the control socket.
func (a *inprocAPI) ApplyConfig(c *config.Config) error { return a.reload(c) }

// AckAlert implements core.API: it loads alerts.json (LoadAlertState, same
// helper ActiveAlerts/AlertHistory above use), acks key via AlertState.Ack
// (anomaly.go), and saves it back -- the in-process counterpart of
// cmdAlertsAck (alerts_cli.go)/fileAPI.AckAlert (coreapi_file.go). Unlike
// those CLI-process paths, this does NOT SIGHUP: this IS the daemon process,
// so there is nothing to signal -- the next fire/recover transition simply
// reads the freshly-saved ack straight off the same in-memory AlertState via
// MergeAckFromDisk (anomaly.go), no round-trip needed.
func (a *inprocAPI) AckAlert(key string) error {
	statePath := a.alertStatePath()
	state := LoadAlertState(statePath, osFS{})
	if err := state.Ack(key, time.Now().Unix()); err != nil {
		return err
	}
	return state.Save(statePath)
}

// UnackAlert implements core.API: AckAlert's mirror image, via
// AlertState.Unack.
func (a *inprocAPI) UnackAlert(key string) error {
	statePath := a.alertStatePath()
	state := LoadAlertState(statePath, osFS{})
	if err := state.Unack(key); err != nil {
		return err
	}
	return state.Save(statePath)
}

// TestChannel implements core.API: it calls sendTestNotification
// (channel.go) against the LIVE config (a.getCfg(), race-safe against a
// concurrent SIGHUP/Reload), mirroring `serverwatch channel test <name>`
// (channel.go's cmdChannelTest) -- for the web channels page's "Send test"
// button (issue #66), reached over the control socket rather than a
// daemon-local closure now that the web UI is out-of-process.
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

// Subscribe is deferred to a later stage (S5, the alerting event-bus
// inversion -- see the epic's design doc): it returns errCoreNotImplemented
// for now so *inprocAPI satisfies core.API today.
func (a *inprocAPI) Subscribe(ctx context.Context) (<-chan core.Event, error) {
	return nil, errCoreNotImplemented
}
