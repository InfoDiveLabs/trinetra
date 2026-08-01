# Core Contract (S1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Introduce a single `core.API` Go interface (with serverwatch-free DTOs) that both the web UI and the CLI consume, replacing today's `web.Deps` seam and the CLI's ad-hoc per-command file opens, with zero behavior change.

**Architecture:** A new stdlib-only `internal/core` package holds the `core.API` interface and its data types (moved from `internal/web`'s mirror types). Two implementations satisfy it: an in-process one inside the daemon (used by the embedded web UI, adapting the live `Snapshot`/`SampleStore`) and a file-backed one (used by the separate CLI process, reading `status.json`/the store/the alert log). This is the seam a later socket-backed implementation slots into unchanged.

**Tech Stack:** Go 1.22, stdlib only for the core package, existing `internal/config`, `-tags web` for the web consumer.

## Global Constraints

- Go 1.22 (module floor; do not raise).
- `internal/core` must import ONLY the Go standard library and `serverwatch/internal/config`. No third-party, no `internal/web`, no `internal/serverwatch`. Enforced by `internal/serverwatch/buildtag_test.go` (`TestDefaultBuildIsStdlibOnly`), which scans the untagged `go list -deps ./...` and fails on any non-stdlib, non-`serverwatch/` package.
- `internal/web` must never import `internal/serverwatch` (would create an import cycle under `-tags web`). `internal/web` may import `internal/core`.
- No em dashes or en dashes in any doc, comment, or string (project style rule).
- TDD for every change: failing test first, watch it fail, minimal code, watch it pass.
- After each task: `go test ./...`, `go test -tags web ./internal/serverwatch/ ./internal/web/`, `go test -race ./internal/serverwatch/`, `go vet ./...` all green. Commit per task.
- No behavior change: the web UI and every CLI command produce identical output/effects before and after S1.

## File Structure

- Create `internal/core/doc.go` — package doc stating the stdlib-only + no-serverwatch-import contract.
- Create `internal/core/dto.go` — the data types: `Resolution`, `SeriesPoint`, `DownEventView`, `DashboardView` (+ its nested `ProcessCounts`/`ContainerView`/`DiskView`/`NetIfaceView`), `MonitoringView` (+ nested), `Availability`, `AlertRecord`, `DoctorReport`. All plain data, JSON-tagged, moved from `internal/web`.
- Create `internal/core/availability.go` — `ComputeAvailability` + `PickResolution`, moved from `internal/web`.
- Create `internal/core/api.go` — the `core.API` interface.
- Modify `internal/web/dashboard_view.go`, `monitoring_view.go`, `series_store.go`, `events_store.go`, `availability.go` — replace the moved struct/func definitions with type aliases / thin forwards to `internal/core` so existing web handler code compiles unchanged.
- Create `internal/serverwatch/coreapi_inproc.go` (untagged) — in-process `core.API` over the live `snapshotHub`/`SampleStore`/`cfg`.
- Create `internal/serverwatch/coreapi_file.go` (untagged) — file-backed `core.API` over `status.json`/the configured store/the alert log.
- Modify `internal/serverwatch/daemon_web.go` / `web_deps.go` — build the in-process `core.API` and hand it to `web.Deps`.
- Modify `internal/web/server.go` — `Deps` gains an `API core.API` field; handlers keep using the existing typed accessors, now backed by it.
- Modify `internal/serverwatch/systemd.go` (`cmdStatus`, `cmdDoctor` + extracted `buildDoctorReport`), `dump.go` (`cmdDump`), `alerts_cli.go` (`cmdAlerts`) — route through the file-backed `core.API`.

Each task ends green and committed. Tasks 1 to 5 establish the read contract and the web path; 6 to 8 route the CLI and add the mutating methods.

---

### Task 1: Scaffold `internal/core` with `Resolution`, `SeriesPoint`, `DownEventView`, `PickResolution`

**Files:**
- Create: `internal/core/doc.go`
- Create: `internal/core/dto.go`
- Create: `internal/core/availability.go`
- Test: `internal/core/core_stdlib_test.go`

**Interfaces:**
- Produces: `core.Resolution` (`type Resolution int` with `ResRaw`, `Res1m`); `core.SeriesPoint struct{ TS int64; Min, Avg, Max float64 }`; `core.DownEventView struct{ Type string; Start, End, DurationSec int64 }`; `core.PickResolution(from, to int64) Resolution`.

- [ ] **Step 1: Write the failing test** that the package exists and `PickResolution` matches the current web behavior for a short and a long window.

```go
package core

import "testing"

func TestPickResolution(t *testing.T) {
	// Copy the exact thresholds from internal/web/series_store.go's current
	// PickResolution before deleting it there (Task pulls it here verbatim).
	if got := PickResolution(0, 3600); got != ResRaw {
		t.Errorf("1h window: got %v want ResRaw", got)
	}
	if got := PickResolution(0, 60*60*24*40); got != Res1m {
		t.Errorf("40d window: got %v want Res1m", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/ -run TestPickResolution`
Expected: FAIL (package/symbols not defined).

- [ ] **Step 3: Write minimal implementation.** In `dto.go` define `Resolution`, `SeriesPoint`, `DownEventView`. In `availability.go` copy `PickResolution` verbatim from `internal/web/series_store.go` (find its current threshold logic there and reproduce it exactly). In `doc.go` write the package contract comment (stdlib + config only; never import serverwatch/web).

```go
// dto.go
package core

type Resolution int

const (
	ResRaw Resolution = iota
	Res1m
)

type SeriesPoint struct {
	TS  int64   `json:"ts"`
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

type DownEventView struct {
	Type        string `json:"type"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	DurationSec int64  `json:"duration_sec"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/core/ -run TestPickResolution`
Expected: PASS.

- [ ] **Step 5: Verify the stdlib guard still holds** (core is now in the default graph).

Run: `go test ./internal/serverwatch/ -run TestDefaultBuildIsStdlibOnly`
Expected: PASS (core imports only stdlib + config).

- [ ] **Step 6: Commit**

```bash
git add internal/core/
git commit -m "core: scaffold package with Resolution/SeriesPoint/DownEventView/PickResolution"
```

---

### Task 2: Move the view DTOs and `ComputeAvailability` into `core`; alias them in `internal/web`

**Files:**
- Modify: `internal/core/dto.go` (add `DashboardView` + nested, `MonitoringView` + nested, `Availability`)
- Modify: `internal/core/availability.go` (add `ComputeAvailability`, moved)
- Modify: `internal/web/dashboard_view.go`, `internal/web/monitoring_view.go`, `internal/web/availability.go`, `internal/web/series_store.go`, `internal/web/events_store.go` (replace moved definitions with aliases)
- Test: `internal/web/availability_test.go` (existing; must stay green)

**Interfaces:**
- Produces: `core.DashboardView`, `core.MonitoringView`, `core.Availability`, `core.ContainerView`, `core.DiskView`, `core.NetIfaceView`, `core.ProcessCounts`, `core.MonitoringContainerView`, `core.MonitoringUnitView`, `core.MonitoringProcessView`, `core.MonitoringDiskView`, `core.ComputeAvailability(events EventsSource, now int64) Availability`.
- Consumes: nothing new.

**Note on aliases:** to avoid churning every web handler reference, `internal/web` keeps the same exported names via Go type aliases, e.g. `type DashboardView = core.DashboardView`. Handler code (`{{.CPU}}`, `v.Disks`, etc.) is unchanged because an alias is the same type.

- [ ] **Step 1: Write the failing test** in `internal/core` that `ComputeAvailability` over one downtime event yields the same uptime percentage the web test asserts today.

```go
package core

import "testing"

func TestComputeAvailabilityOneOutage(t *testing.T) {
	// Reproduce the scenario from internal/web/availability_test.go: a single
	// net_down event of known duration inside the trailing 24h window.
	evs := staticEvents([]DownEventView{{Type: "net_down", Start: 1000, End: 1000 + 3600, DurationSec: 3600}})
	a := ComputeAvailability(evs, 1000+3600)
	if a.UptimePct <= 0 || a.UptimePct >= 100 {
		t.Fatalf("expected a partial uptime pct, got %v", a.UptimePct)
	}
}
```

(Define a tiny `staticEvents` test helper implementing the `EventsSource` the function takes; mirror whatever `ComputeAvailability` consumes today in web.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/ -run TestComputeAvailabilityOneOutage`
Expected: FAIL (symbols undefined).

- [ ] **Step 3: Move the definitions.** Cut `DashboardView` + nested types (`internal/web/dashboard_view.go`), `MonitoringView` + nested (`internal/web/monitoring_view.go`), `Availability` and `ComputeAvailability` (`internal/web/availability.go`), and the `SeriesPoint`/`DownEventView` mirrors already handled in Task 1, into `internal/core`. Preserve field names, JSON tags, and logic verbatim. In each web file, replace the removed definition with an alias, for example in `internal/web/dashboard_view.go`:

```go
package web

import "serverwatch/internal/core"

type DashboardView = core.DashboardView
type ContainerView = core.ContainerView
type DiskView = core.DiskView
type NetIfaceView = core.NetIfaceView
type ProcessCounts = core.ProcessCounts
```

Do the equivalent alias set in `monitoring_view.go`, `availability.go`, `series_store.go` (`type SeriesPoint = core.SeriesPoint`), `events_store.go` (`type DownEventView = core.DownEventView`).

- [ ] **Step 4: Run tests to verify green**

Run: `go test ./internal/core/ -run TestComputeAvailability` then `go test -tags web ./internal/web/`
Expected: PASS both (web handlers compile unchanged via aliases; existing web availability test still passes against the aliased type).

- [ ] **Step 5: Full build + guard**

Run: `go build ./... && go build -tags web ./... && go test ./internal/serverwatch/ -run TestDefaultBuildIsStdlibOnly`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/core/ internal/web/
git commit -m "core: move view DTOs + ComputeAvailability into core, alias in web"
```

---

### Task 3: Define the `core.API` interface

**Files:**
- Modify: `internal/core/api.go`
- Modify: `internal/core/dto.go` (add `AlertRecord`, `DoctorReport`)
- Test: `internal/core/api_test.go`

**Interfaces:**
- Produces: the `core.API` interface and `core.AlertRecord`, `core.DoctorReport` DTOs.

```go
// api.go
package core

import (
	"context"

	"serverwatch/internal/config"
)

// API is the single boundary through which every consumer (the embedded web
// UI, the CLI, and later a socket client) reads daemon state and applies
// changes. Implementations: an in-process one inside the daemon and a
// file-backed one for the separate CLI process.
type API interface {
	// reads
	Snapshot() (DashboardView, error)
	Monitoring() (MonitoringView, error)
	Series(metric string, from, to int64, res Resolution) ([]SeriesPoint, error)
	Events(from, to int64) ([]DownEventView, error)
	ActiveAlerts() ([]AlertRecord, error)
	AlertHistory(since int64, limit int) ([]AlertRecord, error)
	Config() (*config.Config, error)
	Doctor() (DoctorReport, error)

	// writes
	ApplyConfig(*config.Config) error
	AckAlert(key string) error
	UnackAlert(key string) error
	TestChannel(name string) error
	Subscribe(ctx context.Context) (<-chan Event, error)
}
```

Add `AlertRecord` and `DoctorReport` structs (plain data) to `dto.go`, and an `Event` struct (fields: `Kind`, `Severity`, `Source`, `Title`, `Time int64`) for `Subscribe`. Field sets: mirror the data `cmdAlerts` prints (key, severity, kind, source, time, acked) for `AlertRecord`; and what `cmdDoctor` prints (docker access, smartctl availability, thermal-zone count, target count, per-collector on/off, store stats string) for `DoctorReport`.

- [ ] **Step 1: Write the failing test** that a compile-time assertion of interface shape holds (a nil-typed var).

```go
package core

var _ = func(a API) API { return a } // interface must be referenceable
```

Plus a real test asserting the DTOs marshal to JSON with expected keys:

```go
func TestAlertRecordJSONKeys(t *testing.T) {
	b, _ := json.Marshal(AlertRecord{Key: "cpu", Severity: "warning"})
	if !strings.Contains(string(b), `"key"`) || !strings.Contains(string(b), `"severity"`) {
		t.Fatalf("unexpected json: %s", b)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/ -run TestAlertRecordJSONKeys`
Expected: FAIL (types undefined).

- [ ] **Step 3: Write the interface + DTOs** as above.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/core/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/core/
git commit -m "core: define the core.API interface + AlertRecord/DoctorReport/Event DTOs"
```

---

### Task 4: In-process `core.API` read implementation (adapts the live daemon state)

**Files:**
- Create: `internal/serverwatch/coreapi_inproc.go` (untagged)
- Test: `internal/serverwatch/coreapi_inproc_test.go`

**Interfaces:**
- Consumes: `core.API` (Task 3); the existing `buildDashboardView(Snapshot) web.DashboardView` logic (daemon_web.go) which must be re-homed to return `core.DashboardView` (now the same type via alias); `SampleStore`, `snapshotHub`, `getCfg`.
- Produces: `newInprocAPI(getSnap func() Snapshot, getCfg func() *config.Config, store SampleStore, stateDir string) core.API`.

**Note:** `buildDashboardView`/`buildMonitoringView` currently live in `daemon_web.go` (`//go:build web`) and call `web.ComputeAvailability`. Move their bodies into untagged helpers in `coreapi_inproc.go` that build `core.DashboardView`/`core.MonitoringView` and call `core.ComputeAvailability`. The tagged `daemon_web.go` then just calls these.

- [ ] **Step 1: Write the failing test** that the in-process API projects a known Snapshot into a DashboardView with matching scalars.

```go
func TestInprocSnapshotProjectsScalars(t *testing.T) {
	snap := Snapshot{TS: 42, CPU: 12.5, MemPct: 30, Online: true}
	api := newInprocAPI(func() Snapshot { return snap }, func() *config.Config { return config.Default() }, nil, t.TempDir())
	v, err := api.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if v.TS != 42 || v.CPU != 12.5 || !v.Online {
		t.Fatalf("projection mismatch: %+v", v)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/serverwatch/ -run TestInprocSnapshotProjectsScalars`
Expected: FAIL (newInprocAPI undefined).

- [ ] **Step 3: Implement `newInprocAPI`** returning an unexported `inprocAPI` struct. Implement the read methods: `Snapshot`/`Monitoring` via the re-homed builders; `Series` via `store.Query(metric, from, to, res)` mapped to `[]core.SeriesPoint`; `Events` via `store.Events` mapped to `[]core.DownEventView`; `ActiveAlerts`/`AlertHistory` via `LoadAlertState`/`NewAlertLog` against `stateDir`; `Config` via `getCfg`. Leave write methods + `Doctor` + `Subscribe` returning a clear `errNotImplementedYet` for now (wired in Tasks 7 to 8).

- [ ] **Step 4: Run test + race**

Run: `go test -race ./internal/serverwatch/ -run TestInproc`
Expected: PASS.

- [ ] **Step 5: Parity test** asserting `newInprocAPI(...).Snapshot()` equals the existing `buildDashboardView(snap)` for a fully-populated Snapshot (guards the re-home).

- [ ] **Step 6: Commit**

```bash
git add internal/serverwatch/coreapi_inproc.go internal/serverwatch/coreapi_inproc_test.go
git commit -m "serverwatch: in-process core.API read implementation"
```

---

### Task 5: Route the web UI through `core.API`

**Files:**
- Modify: `internal/web/server.go` (`Deps` gains `API core.API`)
- Modify: `internal/serverwatch/daemon_web.go` (build `newInprocAPI`, set `Deps.API`)
- Modify: web handlers only where they call `d.Snapshot()`/`d.Monitoring()`/`d.Store`/`d.Events` to go through `d.API` (keep behavior identical)
- Test: existing `internal/web/*_test.go` (must stay green); add `internal/web/deps_api_test.go`

**Interfaces:**
- Consumes: `core.API`, `newInprocAPI`.
- Produces: `web.Deps.API core.API`.

- [ ] **Step 1: Write the failing test** that a handler served with a `Deps{API: fake}` renders live data from the fake API (e.g. the dashboard shows the fake CPU value).

```go
//go:build web
func TestDashboardReadsFromAPI(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{snap: core.DashboardView{CPU: 77}}
	// serve GET / as an authed user, assert body contains "77"
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -tags web ./internal/web/ -run TestDashboardReadsFromAPI`
Expected: FAIL (Deps has no API field).

- [ ] **Step 3: Add `API core.API` to `web.Deps`.** Point the dashboard/monitoring/history/alerts handlers at `d.API.Snapshot()`/`.Monitoring()`/`.Series()`/`.Events()`/`.ActiveAlerts()` instead of the individual closures/stores. Keep the old fields temporarily where a handler still needs them; migrate one handler per commit if cleaner. In `daemon_web.go`, construct `newInprocAPI(d.Snapshot, d.Cfg, d.Store, d.StateDir)` and assign `wd.API`.

- [ ] **Step 4: Run all web tests + race**

Run: `go test -tags web ./internal/web/ && go test -race -tags web ./internal/web/`
Expected: PASS (no behavior change).

- [ ] **Step 5: Commit**

```bash
git add internal/web/ internal/serverwatch/daemon_web.go
git commit -m "web: read live state through core.API"
```

---

### Task 6: File-backed `core.API` read implementation + route CLI status/dump/alerts

**Files:**
- Create: `internal/serverwatch/coreapi_file.go` (untagged)
- Modify: `internal/serverwatch/systemd.go` (`cmdStatus`), `dump.go` (`cmdDump`), `alerts_cli.go` (`cmdAlerts`)
- Test: `internal/serverwatch/coreapi_file_test.go`

**Interfaces:**
- Produces: `newFileAPI(stateDir string, cfg *config.Config) core.API`.

- [ ] **Step 1: Write the failing test** that `newFileAPI` reads a written `status.json` back as a DashboardView.

```go
func TestFileAPIReadsStatusJSON(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, systemClock{})
	_ = st.WriteStatus(Snapshot{TS: 9, CPU: 55})
	api := newFileAPI(dir, config.Default())
	v, err := api.Snapshot()
	if err != nil || v.CPU != 55 {
		t.Fatalf("got %+v err %v", v, err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/serverwatch/ -run TestFileAPIReadsStatusJSON`
Expected: FAIL (newFileAPI undefined).

- [ ] **Step 3: Implement `newFileAPI`.** `Snapshot`: read `stateDir/status.json`, unmarshal into `Snapshot`, project via the same builder as the in-process impl. `Series`/`Events`: `openConfiguredStore(cfg)` then `Query`/`Events`. `ActiveAlerts`/`AlertHistory`: `LoadAlertState` + `NewAlertLog` against `stateDir`. `Config`: return the passed cfg. This centralizes the ad-hoc opens the CLI commands do today.

- [ ] **Step 4: Route the CLI commands.** `cmdStatus` calls `newFileAPI(stateDir, cfg).Snapshot()` and renders (keep the exact current output). `cmdDump` calls `.Series(metric, from, to, res)`. `cmdAlerts` list calls `.ActiveAlerts()`/`.AlertHistory(since, limit)`. Preserve every flag and output format; assert with a golden test captured before the change.

- [ ] **Step 5: Run tests + race**

Run: `go test -race ./internal/serverwatch/ -run 'TestFileAPI|TestCmdStatus|TestCmdDump|TestCmdAlerts'`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/serverwatch/coreapi_file.go internal/serverwatch/coreapi_file_test.go internal/serverwatch/systemd.go internal/serverwatch/dump.go internal/serverwatch/alerts_cli.go
git commit -m "serverwatch: file-backed core.API; route CLI status/dump/alerts through it"
```

---

### Task 7: Extract `buildDoctorReport`; add `Doctor()` to both implementations

**Files:**
- Modify: `internal/serverwatch/systemd.go` (extract from `cmdDoctor`)
- Modify: `internal/serverwatch/coreapi_inproc.go`, `coreapi_file.go`
- Test: `internal/serverwatch/doctor_test.go`

**Interfaces:**
- Produces: `buildDoctorReport(x Exec, fs FileSource, c *config.Config, store SampleStore) core.DoctorReport`.

- [ ] **Step 1: Write the failing test** that `buildDoctorReport` reports the discovered-target count and collector toggles for a fake Exec/FileSource.

```go
func TestBuildDoctorReport(t *testing.T) {
	rep := buildDoctorReport(fakeExec{}, fakeFS{}, config.Default(), nil)
	if rep.TargetsDiscovered < 0 {
		t.Fatalf("bad report: %+v", rep)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/serverwatch/ -run TestBuildDoctorReport`
Expected: FAIL (buildDoctorReport undefined).

- [ ] **Step 3: Extract** the probe orchestration currently inline in `cmdDoctor` (systemd.go:304-333: `probeDocker`, `smartctl --scan`, thermal glob, `Discover`, `collectorSummary`) into `buildDoctorReport` returning a `core.DoctorReport`. Rewrite `cmdDoctor` to call it and print the same lines. Implement `Doctor()` on both APIs (inproc uses live x/fs/store; file uses `osExec{}`/`osFS{}` + `openConfiguredStore`).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/serverwatch/ -run 'TestBuildDoctorReport|TestCmdDoctor'`
Expected: PASS (cmdDoctor output unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/serverwatch/systemd.go internal/serverwatch/coreapi_inproc.go internal/serverwatch/coreapi_file.go internal/serverwatch/doctor_test.go
git commit -m "serverwatch: extract buildDoctorReport; implement core.API.Doctor()"
```

---

### Task 8: Wire the mutating methods (ApplyConfig / Ack / TestChannel) through both implementations

**Files:**
- Modify: `internal/serverwatch/coreapi_inproc.go` (in-process: pointer-swap reload, in-memory ack, in-process test)
- Modify: `internal/serverwatch/coreapi_file.go` (file-backed: save + `reloadDaemon()` SIGHUP)
- Modify: `internal/serverwatch/alerts_cli.go` (ack/unack via file API), web config/channels handlers already call `Deps.Reload`/`Deps.TestChannel` (re-point to `Deps.API`)
- Test: `internal/serverwatch/coreapi_write_test.go`

**Interfaces:**
- Consumes: existing `reload` closure semantics (`saveCfg` + `applyConfig`), `sendTestNotification`, `LoadAlertState`/`state.Ack`/`state.Save`, `reloadDaemon`.

- [ ] **Step 1: Write the failing test** that the file-backed `ApplyConfig` persists to disk and the in-process one swaps the live pointer.

```go
func TestFileAPIApplyConfigPersists(t *testing.T) {
	dir := t.TempDir()
	// point cfgPath into dir via the existing test seam
	api := newFileAPI(dir, config.Default())
	c := config.Default()
	c.Thresholds.CPUPct = 42
	if err := api.ApplyConfig(c); err != nil { t.Fatal(err) }
	got, _ := config.Load(/* dir cfg path */)
	if got.Thresholds.CPUPct != 42 { t.Fatalf("not persisted: %v", got.Thresholds.CPUPct) }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/serverwatch/ -run TestFileAPIApplyConfig`
Expected: FAIL (ApplyConfig not implemented / returns errNotImplementedYet).

- [ ] **Step 3: Implement the write methods.** In-process: `ApplyConfig` = the daemon's `reload` closure behavior (saveCfg + applyConfig pointer-swap); `AckAlert`/`UnackAlert` = mutate the live alert state; `TestChannel` = `sendTestNotification(getCfg(), name, "web")`. File-backed: `ApplyConfig` = `saveCfg` then `reloadDaemon()` (SIGHUP); `AckAlert`/`UnackAlert` = `LoadAlertState` + `state.Ack/Unack` + `state.Save` + `reloadDaemon()`; `TestChannel` = `sendTestNotification(cfg, name, "cli")`. Re-point `cmdAlertsAck`/`cmdAlertsUnack` to the file API, and the web `Reload`/`TestChannel` wiring to `Deps.API`.

- [ ] **Step 4: Run full verification**

Run: `go test ./... && go test -tags web ./internal/serverwatch/ ./internal/web/ && go test -race ./internal/serverwatch/ && go vet ./...`
Expected: PASS all.

- [ ] **Step 5: Commit**

```bash
git add internal/serverwatch/ internal/web/
git commit -m "core.API: wire ApplyConfig/Ack/TestChannel through in-process and file backends"
```

---

## Self-Review

- **Spec coverage:** S1 in the spec ("Core contract, in-process; route in-process consumers through it; web.Deps collapses into core.API; CLI status/dump/alerts/doctor go through it") is covered by Tasks 1 to 8. `Subscribe`/event-bus is defined in the interface (Task 3) but its implementation is deferred to spec stage S5 (the alerting inversion), noted in Task 4 as returning not-implemented. The socket transport (S2), CLI split (S3), and web split (S4) are out of scope for this plan and get their own plans.
- **Placeholder scan:** the only `errNotImplementedYet` returns are the intentionally-deferred `Subscribe` (S5) and the write methods before Task 8; every read method is implemented within this plan. No TBD/TODO left in shipped code.
- **Type consistency:** `Resolution`, `SeriesPoint`, `DownEventView`, `DashboardView`, `MonitoringView`, `AlertRecord`, `DoctorReport`, and the `core.API` method set are named identically across Tasks 1, 3, 4, 6, 7, 8. The web aliases (Task 2) keep the exported web names pointing at the core types.
- **Boundary:** every DTO in `core` is stdlib + config only; `internal/web` imports `core` (allowed) and never `internal/serverwatch`; the stdlib guard is re-run in Tasks 1, 2, and 8.

## Deferred to later plans (from the spec)

- S2: control socket server + newline-JSON codec + socket-client `core.API`.
- S3: split the CLI into `serverwatch-ctl` (first supervised plugin).
- S4: split the web into `serverwatch-web` over the socket.
- S5: alerting event-bus inversion (`Subscribe` + embedded notifier consumer).
