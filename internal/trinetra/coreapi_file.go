// Package trinetra: coreapi_file.go implements core.API file-backed -- the counterpart to
// coreapi_inproc.go's in-process implementation, for the separate CLI process.
package trinetra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/version"
)

// errEnrollNeedsDaemon is returned by fileAPI.EnrollmentPIN: the enrollment pin lives only
// in the running daemon's in-memory enrollState (enroll.go).
var errEnrollNeedsDaemon = errors.New("trinetra: enrollment pin requires a running daemon; dial the control socket instead")

// fileAPI is the file-backed core.API implementation: every method opens whatever it needs
// off disk on each call.
type fileAPI struct {
	stateDir string
	cfg      *config.Config
}

// newFileAPI builds a core.API backed by stateDir's on-disk files plus cfg.
func newFileAPI(stateDir string, cfg *config.Config) core.API {
	return &fileAPI{stateDir: stateDir, cfg: cfg}
}

// alertStatePath/alertLogPath mirror inprocAPI's identically named helpers
// (coreapi_inproc.go) -- same filenames under the same state directory.
func (a *fileAPI) alertStatePath() string { return filepath.Join(a.stateDir, "alerts.json") }
func (a *fileAPI) alertLogPath() string   { return filepath.Join(a.stateDir, "alertlog.jsonl") }

// Snapshot implements core.API: it reads stateDir/status.json (the last Snapshot the
// daemon's sampler loop wrote via Store.WriteStatus), unmarshals it into a Snapshot.
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

// Series implements core.API: it opens the configured SampleStore for this one call and
// closes it before returning.
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
		// Same "query <metric>: " wrap the pre-routing dumpSeries (dump.go) applied around this
		// exact store.Query call, preserved here now that the call lives inside Series instead.
		return nil, fmt.Errorf("query %s: %w", metric, err)
	}
	out := make([]core.SeriesPoint, len(pts))
	for i, p := range pts {
		out[i] = core.SeriesPoint{TS: p.TS, Min: p.Min, Avg: p.Avg, Max: p.Max}
	}
	return out, nil
}

// Events implements core.API the same way Series does: open the configured store fresh,
// close it before returning.
func (a *fileAPI) Events(from, to int64) ([]core.DownEventView, error) {
	store, err := openConfiguredStore(a.cfg)
	if err != nil {
		return nil, fmt.Errorf("open sample store: %w", err)
	}
	defer store.Close()

	evs, err := store.Events(from, to)
	if err != nil {
		// Unlike Series' store.Query call above, this is deliberately left unwrapped:
		// store.Events had no CLI caller before this task (dump.go only ever called Query).
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
func (a *fileAPI) ActiveAlerts() ([]core.AlertRecord, error) {
	state := LoadAlertState(a.alertStatePath(), osFS{})
	return activeAlertRecords(state), nil
}

// AlertHistory implements core.API: it delegates to the shared alertHistoryRecords helper
// (coreapi_alerts.go), the same one inprocAPI.AlertHistory (coreapi_inproc.go) calls.
func (a *fileAPI) AlertHistory(since int64, limit int) ([]core.AlertRecord, error) {
	return alertHistoryRecords(NewAlertLog(a.alertLogPath()), since, limit)
}

// Config implements core.API: it just returns the cfg this fileAPI was constructed.
func (a *fileAPI) Config() (*config.Config, error) {
	return a.cfg, nil
}

// Doctor implements core.API via the shared buildDoctorReport (systemd.go), the same probe
// orchestration `trinetra doctor` (cmdDoctor) runs: x/fs are the real osExec{}/osFS{}.
func (a *fileAPI) Doctor() (core.DoctorReport, error) {
	var store SampleStore
	if s, err := openConfiguredStore(a.cfg); err == nil {
		store = s
		defer store.Close()
	}
	return buildDoctorReport(osExec{}, osFS{}, a.cfg, store), nil
}

// HostInfo implements core.API (#100).
func (a *fileAPI) HostInfo() (core.HostInfoView, error) {
	return buildHostInfoView(collectHostInfoFor(a.cfg), time.Now().Unix()), nil
}

// ContainerLogs implements core.API: like HostInfo it shells out to the real
// host on demand (osExec{}/osFS{}).
func (a *fileAPI) ContainerLogs(name string, lines int) (string, error) {
	return collectContainerLogs(osExec{}, osFS{}, name, lines)
}

// Version implements core.API: this process's own build-stamped version (#107).
func (a *fileAPI) Version() (string, error) { return version.String(), nil }

// UpdateStatus implements core.API: this host's persisted self-update posture, via the
// shared coreUpdateStatus helper (update_cmd.go) built from this fileAPI's own cfg.
func (a *fileAPI) UpdateStatus() (core.UpdateStatusView, error) {
	return coreUpdateStatus(a.cfg)
}

// UpdateCheck implements core.API via the shared coreUpdateCheck helper.
func (a *fileAPI) UpdateCheck(ctx context.Context) (core.UpdateStatusView, error) {
	return coreUpdateCheck(ctx, a.cfg)
}

// UpdateApply implements core.API: delegates to newUpdater(a.cfg).apply, exactly like
// `trinetra update apply` itself -- synchronously, blocking until the swap.
func (a *fileAPI) UpdateApply(ctx context.Context, version string) error {
	_, err := newUpdater(a.cfg).apply(ctx, a.cfg, applyOptions{Version: version})
	return err
}

// UpdateRollback implements core.API: delegates to newUpdater(a.cfg).rollback, exactly like
// `trinetra update rollback` -- synchronously.
func (a *fileAPI) UpdateRollback() error {
	return newUpdater(a.cfg).rollback()
}

// EnrollmentPIN implements core.API: this CLI process has no live daemon state (unlike
// inprocAPI, which reads through its own enrollState).
func (a *fileAPI) EnrollmentPIN(ctx context.Context) (string, bool, error) {
	return "", false, errEnrollNeedsDaemon
}

// MonitorTargets implements core.API: unlike EnrollmentPIN above, target discovery needs no
// live daemon state.
func (a *fileAPI) MonitorTargets(ctx context.Context) ([]core.TargetView, error) {
	return targetViewsFromTargets(DiscoverLocal()), nil
}

// ApplyConfig implements core.API: it persists c to cfgPath.
func (a *fileAPI) ApplyConfig(c *config.Config) error {
	if err := saveDaemonCfg(c); err != nil {
		return err
	}
	reloadDaemon()
	return nil
}

// AckAlert implements core.API: LoadAlertState + AlertState.Ack + Save, then a best-effort
// SIGHUP (reloadDaemon) so a running daemon re-reads the ack promptly.
func (a *fileAPI) AckAlert(key string) error {
	statePath := a.alertStatePath()
	state := LoadAlertState(statePath, osFS{})
	if err := state.Ack(key, time.Now().Unix()); err != nil {
		return err
	}
	if err := state.Save(statePath); err != nil {
		return err
	}
	reloadDaemon()
	return nil
}

// UnackAlert implements core.API: AckAlert's mirror image, via
// AlertState.Unack -- identical to cmdAlertsAck's unack path.
func (a *fileAPI) UnackAlert(key string) error {
	statePath := a.alertStatePath()
	state := LoadAlertState(statePath, osFS{})
	if err := state.Unack(key); err != nil {
		return err
	}
	if err := state.Save(statePath); err != nil {
		return err
	}
	reloadDaemon()
	return nil
}

// TestChannel implements core.API: it calls sendTestNotification (channel.go) against this
// fileAPI's cfg with source "cli", identical to cmdChannelTest (channel.go).
func (a *fileAPI) TestChannel(name string) error {
	return sendTestNotification(a.cfg, name, "cli")
}

// ValidateChannel implements core.API: it calls buildNotifier (channels.go) against cc and
// this fileAPI's own cfg.
func (a *fileAPI) ValidateChannel(cc config.ChannelConfig) error {
	_, err := buildNotifier(cc, a.cfg)
	return err
}

// Subscribe implements core.API: fileAPI has no live daemon behind it (this is the separate
// CLI process' file-backed reader, coreapi_file.go's own doc).
func (a *fileAPI) Subscribe(ctx context.Context) (<-chan core.Event, error) {
	return nil, errStreamRequiresDaemon
}
