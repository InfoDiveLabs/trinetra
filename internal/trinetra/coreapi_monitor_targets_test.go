// Package serverwatch: coreapi_monitor_targets_test.go covers core.API's
// MonitorTargets on both implementations (coreapi_inproc.go/coreapi_file.go)
// plus the shared targetViewsFromTargets mapping they both use, so ctl's
// monitor-thresholds screen can list targets over the control socket
// instead of importing this package directly to call DiscoverLocal itself.
package trinetra

import (
	"context"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// TestTargetViewsFromTargetsMaps pins the field-for-field mapping from
// Target (this package's discovery result) to core.TargetView (the DTO
// exposed over core.API/the control socket): every exported field carries
// straight across.
func TestTargetViewsFromTargetsMaps(t *testing.T) {
	targets := []Target{
		{ID: "disk:/", Kind: "disk", Display: "/", Available: true},
		{ID: "docker", Kind: "docker", Display: "docker (unavailable)", Available: false},
	}
	got := targetViewsFromTargets(targets)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].ID != "disk:/" || got[0].Kind != "disk" || got[0].Display != "/" || !got[0].Available {
		t.Errorf("got[0] = %+v, want fields copied from targets[0]", got[0])
	}
	if got[1].ID != "docker" || got[1].Available {
		t.Errorf("got[1] = %+v, want fields copied from targets[1] (Available=false)", got[1])
	}
}

// TestInprocMonitorTargetsReturnsSlice is a smoke test: inprocAPI.MonitorTargets
// runs real (osExec{}/osFS{}-backed) discovery, so this only asserts it
// completes without error and without panicking -- the mapping itself is
// pinned by TestTargetViewsFromTargetsMaps above.
func TestInprocMonitorTargetsReturnsSlice(t *testing.T) {
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, nil, t.TempDir(), nil, nil, &enrollState{})
	got, err := api.MonitorTargets(context.Background())
	if err != nil {
		t.Fatalf("MonitorTargets() error = %v, want nil", err)
	}
	_ = got // possibly empty on a sandboxed host with no docker/disks/smartctl
}

// TestFileAPIMonitorTargetsReturnsSlice mirrors
// TestInprocMonitorTargetsReturnsSlice for fileAPI: same real discovery, run
// from the separate CLI process' code path.
func TestFileAPIMonitorTargetsReturnsSlice(t *testing.T) {
	api := newFileAPI(t.TempDir(), config.Default())
	got, err := api.MonitorTargets(context.Background())
	if err != nil {
		t.Fatalf("MonitorTargets() error = %v, want nil", err)
	}
	_ = got
}
