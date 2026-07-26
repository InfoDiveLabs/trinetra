//go:build web

package serverwatch

import (
	"fmt"
	"runtime"
	"sort"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/web"
)

// maybeStartWeb is the `-tags web` half of the seam: it is the ONLY file in
// this package allowed to import internal/web (see web_deps.go's design
// note). It adapts serverwatch's native WebDeps into web.Deps — a
// serverwatch-free type — and delegates to web.Start.
//
// web.Start can legitimately fail even when Enabled is true (issue #59's
// validateOrigin rejecting a passkey-unsafe or incomplete web.* config)
// before it ever binds a listener, in which case it returns a nil stop and
// a non-nil error. cmdDaemon (daemon.go) unconditionally defers whatever
// stop func this returns, so passing that nil straight through would panic
// the daemon on shutdown; instead, log the failure to stderr and hand back
// a safe no-op so the daemon keeps running without the web UI.
func maybeStartWeb(d WebDeps) func() {
	wd := web.Deps{
		Cfg:            d.Cfg,
		Reload:         d.Reload,
		Store:          seriesStoreFor(d.Store, d.Cfg),
		Snapshot:       func() web.DashboardView { return buildDashboardView(d.Snapshot()) },
		StateDir:       d.StateDir,
		AlertLogPath:   d.AlertLogPath,
		AlertStatePath: d.AlertStatePath,
		Enabled:        d.Enabled,
		Listen:         d.Listen,
	}
	stop, err := web.Start(wd)
	if err != nil {
		fmt.Fprintln(stderr, "web: failed to start, web UI disabled:", err)
		return func() {}
	}
	return stop
}

// buildDashboardView adapts a serverwatch.Snapshot (native to this package)
// into a web.DashboardView (native to internal/web) — the Task 8 (#64)
// resolution of the Task 1 placeholder that made Deps.Snapshot return `any`.
// This is the one place that widening happens, exactly where the design note
// atop web_deps.go said it would land, since only THIS file may import both
// packages.
//
// CONCURRENCY: snap is a value the caller (d.Snapshot(), ultimately
// latestSnapshot()) already copied out of snapshotHub — see that function's
// doc. Its map fields (Disks/Containers/ContainerStats/etc.), however, are
// still the very maps the sampler loop's last-published *Snapshot points at:
// copying a struct copies map HEADERS, not their contents. That's exactly
// what snapshotHub's "collectors replace map fields wholesale, never mutate
// in place" contract makes safe — but only as long as every reader,
// including this function, is READ-ONLY against those maps. Every access
// below is a plain read (range/index); nothing here ever assigns into
// snap.Disks, snap.Containers, snap.ContainerStats, snap.NetRates, or
// snap.DiskDetail — doing so would race against the sampler loop the next
// time it replaces that field. New slices built here (v.Disks, v.NetIfaces,
// v.TopCPUContainers, ...) are this function's own, never aliases into snap.
func buildDashboardView(snap Snapshot) web.DashboardView {
	v := web.DashboardView{
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
		Processes: web.ProcessCounts{
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
		if d.UsagePct >= web.DiskCriticalPct {
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

// seriesStoreFor adapts d.Store (a native serverwatch.SampleStore, possibly
// nil when store-writes-disabled mode leaves the daemon without one) into
// the web.SeriesStore interface (internal/web/series_store.go) — the Task 9
// (#65) resolution of the Task 1 placeholder that made WebDeps.Store widen
// straight into web.Deps.Store as `any`, mirroring buildDashboardView's role
// for Deps.Snapshot.
//
// A nil store returns a true nil web.SeriesStore, NOT a non-nil interface
// wrapping a nil *seriesStoreAdapter: assigning a typed nil pointer into an
// interface value produces a non-nil interface whose method set still
// panics on first use, and internal/web's `d.Store != nil` nil-check
// (handlers_history.go) exists precisely to treat "no store" as "empty
// series" without ever calling into one — so this indirection matters, not
// just style.
func seriesStoreFor(store SampleStore, cfg func() *config.Config) web.SeriesStore {
	if store == nil {
		return nil
	}
	return &seriesStoreAdapter{store: store, cfg: cfg}
}

// seriesStoreAdapter is the concrete web.SeriesStore seriesStoreFor builds:
// it wraps a native SampleStore and resolves the raw-vs-1m Resolution
// argument SampleStore.Query needs internally (via PickResolution and the
// daemon's configured storage.raw_retention), so internal/web — which holds
// this only as a web.SeriesStore — never needs a Resolution type of its own.
type seriesStoreAdapter struct {
	store SampleStore
	// cfg returns the daemon's current config (race-safe against SIGHUP
	// reload, same as WebDeps.Cfg) so Query can read the LIVE
	// storage.raw_retention on every call rather than a value captured once
	// at daemon startup. May be nil in tests that don't care about a
	// specific retention window; Query falls back to defaultRawRetention
	// then, mirroring configuredRawRetention's own nil-safety.
	cfg func() *config.Config
}

// Query implements web.SeriesStore: PickResolution picks raw vs 1m from the
// requested range, "now", and the configured raw retention, then delegates
// to the wrapped SampleStore.Query and widens each returned Point into a
// web.SeriesPoint. A store error is returned as-is (propagated, not
// swallowed) — internal/web's seriesAPIHandler is what decides an error
// still renders as an empty 200 response, not this adapter's job.
func (a *seriesStoreAdapter) Query(metric string, from, to int64) ([]web.SeriesPoint, error) {
	rawRetention := defaultRawRetention
	if a.cfg != nil {
		if cfg := a.cfg(); cfg != nil {
			rawRetention = configuredRawRetention(cfg)
		}
	}
	now := time.Now().Unix()
	res := PickResolution(from, to, now, rawRetention)

	pts, err := a.store.Query(metric, from, to, res)
	if err != nil {
		return nil, err
	}
	out := make([]web.SeriesPoint, len(pts))
	for i, p := range pts {
		out[i] = web.SeriesPoint{TS: p.TS, Min: p.Min, Avg: p.Avg, Max: p.Max}
	}
	return out, nil
}

// topContainers builds the "top by CPU%"/"top by memory" container lists
// (each capped to web.dashboardTopN) from snap.ContainerStats — read-only,
// see buildDashboardView's CONCURRENCY note. states supplies each
// container's running/exited/... state (snap.Containers); a stats entry
// with no matching state (a container docker stats saw but the plain state
// listing didn't, a narrow race between the two shell-outs within one slow
// tick) just renders with an empty State rather than being dropped.
func topContainers(states map[string]string, stats map[string]ContainerStat) (byCPU, byMem []web.ContainerView) {
	if len(stats) == 0 {
		return nil, nil
	}
	all := make([]web.ContainerView, 0, len(stats))
	for name, cs := range stats {
		all = append(all, web.ContainerView{
			Name:   name,
			State:  states[name],
			CPUPct: cs.CPUPct,
			MemMiB: cs.MemMiB,
		})
	}

	byCPUAll := append([]web.ContainerView(nil), all...)
	sort.Slice(byCPUAll, func(i, j int) bool {
		if byCPUAll[i].CPUPct != byCPUAll[j].CPUPct {
			return byCPUAll[i].CPUPct > byCPUAll[j].CPUPct
		}
		return byCPUAll[i].Name < byCPUAll[j].Name
	})
	byMemAll := append([]web.ContainerView(nil), all...)
	sort.Slice(byMemAll, func(i, j int) bool {
		if byMemAll[i].MemMiB != byMemAll[j].MemMiB {
			return byMemAll[i].MemMiB > byMemAll[j].MemMiB
		}
		return byMemAll[i].Name < byMemAll[j].Name
	})

	return capContainers(byCPUAll), capContainers(byMemAll)
}

// dashboardTopContainerCap mirrors web.dashboardTopN (unexported there, so
// duplicated here as a plain constant rather than reached into across the
// package boundary) — the mockup dashboard.html's "Top containers" panels
// show exactly 4 rows each.
const dashboardTopContainerCap = 4

func capContainers(all []web.ContainerView) []web.ContainerView {
	if len(all) > dashboardTopContainerCap {
		return all[:dashboardTopContainerCap]
	}
	return all
}

// diskViews merges Snapshot.Disks (mount -> usage%, always populated) with
// Snapshot.DiskDetail (mount -> device/size/free/fill-projection, additive)
// into one sorted-by-mount slice — read-only against both maps, see
// buildDashboardView's CONCURRENCY note.
func diskViews(disks map[string]float64, detail map[string]DiskDetail) []web.DiskView {
	if len(disks) == 0 {
		return nil
	}
	mounts := make([]string, 0, len(disks))
	for m := range disks {
		mounts = append(mounts, m)
	}
	sort.Strings(mounts)

	out := make([]web.DiskView, 0, len(mounts))
	for _, m := range mounts {
		dv := web.DiskView{Mount: m, UsagePct: disks[m]}
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
// slice sorted by interface name — read-only, see buildDashboardView's
// CONCURRENCY note.
func netIfaceViews(rates map[string]IfaceRate) []web.NetIfaceView {
	if len(rates) == 0 {
		return nil
	}
	names := make([]string, 0, len(rates))
	for n := range rates {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]web.NetIfaceView, 0, len(names))
	for _, n := range names {
		out = append(out, web.NetIfaceView{Name: n, RxBps: rates[n].RxBps, TxBps: rates[n].TxBps})
	}
	return out
}
