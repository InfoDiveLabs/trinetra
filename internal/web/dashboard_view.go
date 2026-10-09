package web

import "github.com/InfoDiveLabs/trinetra/internal/core"

// DashboardView and its nested types moved to internal/core/dto.go so both this
// package and internal/cli can consume the same projection of the daemon's live
// Snapshot without internal/web depending on internal/trinetra. These are Go
// type aliases, not new types, so every existing handler/template reference in
// this package (v.CPU, v.Disks, etc.) keeps compiling unchanged.
type DashboardView = core.DashboardView
type ProcessCounts = core.ProcessCounts
type ContainerView = core.ContainerView
type DiskView = core.DiskView
type NetIfaceView = core.NetIfaceView

// dashboardTopN bounds TopCPUContainers/TopMemContainers (the dashboard's
// 4-row "Top containers" hbar panels). Unexported and unused outside this
// file's own doc comments; internal/trinetra/coreapi_inproc.go's
// buildDashboardView adapter mirrors this value as its own
// dashboardTopContainerCap since it cannot reach an unexported identifier
// here.
const dashboardTopN = 4

// DiskCriticalPct is the usage percentage at/above which a mount counts
// toward DashboardView.DisksCritical and renders with the "crit" meter/led
// color -- a display-only threshold for the dashboard's summary tiles,
// independent of (and not a substitute for) the daemon's own configurable
// alert thresholds (internal/config), which keep driving real alert
// delivery. Exported so internal/trinetra/coreapi_inproc.go's
// buildDashboardView adapter can count DisksCritical using the exact same
// cutoff this package's template uses to color the same mounts.
const DiskCriticalPct = core.DiskCriticalPct

// DiskWarnPct is the warn-level counterpart to DiskCriticalPct, same
// display-only caveat.
const DiskWarnPct = core.DiskWarnPct
