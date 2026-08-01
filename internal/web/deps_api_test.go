//go:build web

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// fakeAPI is a minimal core.API test double for this package's handler
// tests: each field backs exactly one read method's return value (the zero
// value/nil error when unset), and every write/Doctor/Subscribe method is a
// harmless no-op stub, since this task only routes the web UI's READ paths
// through core.API (see the task brief), so no test here needs a working
// write half.
type fakeAPI struct {
	snap       core.DashboardView
	snapErr    error
	monitoring core.MonitoringView
	monErr     error
	// series maps a metric name straight to the points Series should return
	// for it (ignoring from/to/res unless seriesErr is set), mirroring the
	// now-removed fakeSeriesStore's shape.
	series    map[string][]core.SeriesPoint
	seriesErr error
	events    []core.DownEventView
	eventsErr error
	active    []core.AlertRecord
	history   []core.AlertRecord
}

func (f fakeAPI) Snapshot() (core.DashboardView, error)    { return f.snap, f.snapErr }
func (f fakeAPI) Monitoring() (core.MonitoringView, error) { return f.monitoring, f.monErr }

func (f fakeAPI) Series(metric string, from, to int64, res core.Resolution) ([]core.SeriesPoint, error) {
	if f.seriesErr != nil {
		return nil, f.seriesErr
	}
	return f.series[metric], nil
}

func (f fakeAPI) Events(from, to int64) ([]core.DownEventView, error) {
	return f.events, f.eventsErr
}

func (f fakeAPI) ActiveAlerts() ([]core.AlertRecord, error) { return f.active, nil }

func (f fakeAPI) AlertHistory(since int64, limit int) ([]core.AlertRecord, error) {
	return f.history, nil
}

func (f fakeAPI) Config() (*config.Config, error)    { return nil, nil }
func (f fakeAPI) Doctor() (core.DoctorReport, error) { return core.DoctorReport{}, nil }

func (f fakeAPI) ApplyConfig(*config.Config) error                         { return nil }
func (f fakeAPI) AckAlert(key string) error                                { return nil }
func (f fakeAPI) UnackAlert(key string) error                              { return nil }
func (f fakeAPI) TestChannel(name string) error                            { return nil }
func (f fakeAPI) Subscribe(ctx context.Context) (<-chan core.Event, error) { return nil, nil }

// TestDashboardReadsFromAPI pins the core TDD obligation for this task: once
// Deps.API is set, GET / renders the fake API's Snapshot() data rather than
// Deps.Snapshot(); the dashboard handler must be reading state through
// core.API, not the pre-Task-5 closures.
func TestDashboardReadsFromAPI(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{snap: core.DashboardView{CPU: 77}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "77") {
		t.Errorf("dashboard body missing fake API CPU value 77:\n%s", rr.Body.String())
	}
}
