// Package serverwatch: coreapi_file.go implements core.API file-backed --
// the counterpart to coreapi_inproc.go's in-process implementation, for the
// separate CLI process (`serverwatch status`/`dump`/`alerts`/...), which has
// no access to the running daemon's live memory and so must read everything
// back off disk: status.json for Snapshot, the configured SampleStore for
// Series/Events, and alerts.json/alertlog.jsonl for ActiveAlerts/
// AlertHistory. It shares buildDashboardView/buildMonitoringView and the
// severityString/AlertRecord-mapping logic with coreapi_inproc.go so both
// implementations stay consistent by construction rather than by two
// separately-maintained copies.
//
// Deliberately UNTAGGED, same reasoning as coreapi_inproc.go's doc: core.API
// and its DTOs live in internal/core, which imports nothing but stdlib +
// internal/config, so this file never pulls internal/web's third-party
// dependencies into the default build.
package serverwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// fileAPI is the file-backed core.API implementation: every method opens
// whatever it needs off disk on each call (there is no long-lived daemon
// state to hold onto here, unlike inprocAPI) -- stateDir is the daemon's
// state directory (status.json, alerts.json, alertlog.jsonl) and cfg is the
// CLI process' own freshly-loaded config, used both as Config()'s return
// value and to open the configured SampleStore for Series/Events.
type fileAPI struct {
	stateDir string
	cfg      *config.Config
}

// newFileAPI builds a core.API backed by stateDir's on-disk files plus cfg.
// This centralizes the ad-hoc opens the CLI commands (cmdStatus, cmdDump,
// cmdAlerts) do today into one place, same read semantics, one boundary.
func newFileAPI(stateDir string, cfg *config.Config) core.API {
	return &fileAPI{stateDir: stateDir, cfg: cfg}
}

// alertStatePath/alertLogPath mirror inprocAPI's identically named helpers
// (coreapi_inproc.go) -- same filenames under the same state directory.
func (a *fileAPI) alertStatePath() string { return filepath.Join(a.stateDir, "alerts.json") }
func (a *fileAPI) alertLogPath() string   { return filepath.Join(a.stateDir, "alertlog.jsonl") }

// Snapshot implements core.API: it reads stateDir/status.json (the last
// Snapshot the daemon's sampler loop wrote via Store.WriteStatus),
// unmarshals it into a Snapshot, and projects it through the same
// buildDashboardView the in-process impl uses, then computes Availability
// fresh on top -- the same two-step sequence inprocAPI.Snapshot follows, so
// the two implementations produce an identical shape from equivalent input.
// A missing/corrupt status.json is a real error (unlike the alert-state/log
// reads elsewhere in this file, which degrade to empty): there is no
// meaningful "empty Snapshot" to project, and the caller (a future consumer
// of this method -- cmdStatus deliberately still reads the raw bytes itself,
// see systemd.go) needs to know status isn't available yet.
func (a *fileAPI) Snapshot() (core.DashboardView, error) {
	b, err := os.ReadFile(filepath.Join(a.stateDir, "status.json"))
	if err != nil {
		return core.DashboardView{}, err
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return core.DashboardView{}, err
	}
	v := buildDashboardView(snap)
	v.Availability = core.ComputeAvailability(a, time.Now().Unix())
	return v, nil
}

// Monitoring implements core.API the same way Snapshot does: read
// status.json, project via buildMonitoringView with this fileAPI's cfg.
func (a *fileAPI) Monitoring() (core.MonitoringView, error) {
	b, err := os.ReadFile(filepath.Join(a.stateDir, "status.json"))
	if err != nil {
		return core.MonitoringView{}, err
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return core.MonitoringView{}, err
	}
	return buildMonitoringView(snap, a.cfg), nil
}

// Series implements core.API: it opens the configured SampleStore fresh
// (openConfiguredStore(a.cfg), rooted at the package-level stateDir the CLI
// process already has set -- see openConfiguredStore's doc) for this one
// call and closes it before returning, mirroring what cmdDump does today.
// Resolution mapping and the nil/failed-store degrade-to-empty behavior are
// the same as inprocAPI.Series (see that method's doc); a store open
// failure, unlike a nil store, IS surfaced as an error here since it
// reflects a real misconfiguration the CLI caller should see, not a daemon
// running in a deliberately degraded mode. The error is wrapped with the
// same "open sample store: " prefix cmdDump/cmdMigrate have always used
// (migrate.go) so a caller that just Fprintln's the returned error -- as
// cmdDump does -- keeps producing that exact wording, whether the store
// open happens here or, previously, directly at the call site.
func (a *fileAPI) Series(metric string, from, to int64, res core.Resolution) ([]core.SeriesPoint, error) {
	store, err := openConfiguredStore(a.cfg)
	if err != nil {
		return nil, fmt.Errorf("open sample store: %w", err)
	}
	defer store.Close()

	var swRes Resolution
	switch res {
	case core.ResRaw:
		swRes = ResRaw
	case core.Res1m:
		swRes = Res1m
	default: // core.ResAuto (or any future/unrecognized value): let the picker decide
		rawRetention := defaultRawRetention
		if a.cfg != nil {
			rawRetention = configuredRawRetention(a.cfg)
		}
		swRes = PickResolution(from, to, time.Now().Unix(), rawRetention)
	}

	pts, err := store.Query(metric, from, to, swRes)
	if err != nil {
		// Same "query <metric>: " wrap the pre-routing dumpSeries (dump.go)
		// applied around this exact store.Query call, preserved here now
		// that the call lives inside Series instead -- see the store-open
		// wrap above for the identical reasoning.
		return nil, fmt.Errorf("query %s: %w", metric, err)
	}
	out := make([]core.SeriesPoint, len(pts))
	for i, p := range pts {
		out[i] = core.SeriesPoint{TS: p.TS, Min: p.Min, Avg: p.Avg, Max: p.Max}
	}
	return out, nil
}

// Events implements core.API the same way Series does: open the configured
// store fresh, close it before returning. A store open failure is surfaced
// as an error wrapped with the same "open sample store: " prefix, same
// reasoning as Series above.
func (a *fileAPI) Events(from, to int64) ([]core.DownEventView, error) {
	store, err := openConfiguredStore(a.cfg)
	if err != nil {
		return nil, fmt.Errorf("open sample store: %w", err)
	}
	defer store.Close()

	evs, err := store.Events(from, to)
	if err != nil {
		// Unlike Series' store.Query call above, this is deliberately left
		// unwrapped: store.Events had no CLI caller before this task (dump.go
		// only ever called Query), so there is no pre-existing "original
		// path" wording to preserve here -- and inprocAPI.Events (the parity
		// reference for this method) doesn't wrap it either.
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
// the same one inprocAPI.ActiveAlerts (coreapi_inproc.go) calls -- see that
// helper's doc for the field-mapping rationale.
func (a *fileAPI) ActiveAlerts() ([]core.AlertRecord, error) {
	state := LoadAlertState(a.alertStatePath(), osFS{})
	return activeAlertRecords(state), nil
}

// AlertHistory implements core.API: it delegates to the shared
// alertHistoryRecords helper (coreapi_alerts.go), the same one
// inprocAPI.AlertHistory (coreapi_inproc.go) calls -- see that helper's doc
// for the field-mapping rationale (newest-first, limit cap, Acked always
// false).
func (a *fileAPI) AlertHistory(since int64, limit int) ([]core.AlertRecord, error) {
	return alertHistoryRecords(NewAlertLog(a.alertLogPath()), since, limit)
}

// Config implements core.API: it just returns the cfg this fileAPI was
// constructed with (the CLI process' own freshly-loaded config -- there is
// no SIGHUP-safe live accessor to call through to here, unlike inprocAPI).
func (a *fileAPI) Config() (*config.Config, error) {
	return a.cfg, nil
}

// Doctor, ApplyConfig, AckAlert, UnackAlert, TestChannel, and Subscribe are
// deferred to later tasks (7-8): each returns errCoreNotImplemented for now
// so *fileAPI satisfies core.API today, mirroring inprocAPI's identical
// deferral (coreapi_inproc.go).

func (a *fileAPI) Doctor() (core.DoctorReport, error) {
	return core.DoctorReport{}, errCoreNotImplemented
}

func (a *fileAPI) ApplyConfig(*config.Config) error { return errCoreNotImplemented }

func (a *fileAPI) AckAlert(key string) error { return errCoreNotImplemented }

func (a *fileAPI) UnackAlert(key string) error { return errCoreNotImplemented }

func (a *fileAPI) TestChannel(name string) error { return errCoreNotImplemented }

func (a *fileAPI) Subscribe(ctx context.Context) (<-chan core.Event, error) {
	return nil, errCoreNotImplemented
}
