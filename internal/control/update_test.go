package control

import (
	"context"
	"errors"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// TestClientUpdateStatusAndApplyRoundTrip pins the four new control-socket
// self-update methods (task 8): a Client dialed against a real Serve loop
// must round-trip UpdateStatus unmodified and must have UpdateApply reach
// the daemon-side api with the exact version requested.
func TestClientUpdateStatusAndApplyRoundTrip(t *testing.T) {
	fake := &fakeAPI{updateStatus: core.UpdateStatusView{Running: "0.5.0", Available: "0.5.1"}}
	path := startTestServer(t, fake, "")

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	got, err := client.UpdateStatus()
	if err != nil {
		t.Fatalf("UpdateStatus() error: %v", err)
	}
	if got.Running != "0.5.0" || got.Available != "0.5.1" {
		t.Errorf("UpdateStatus() = %+v, want Running=0.5.0 Available=0.5.1", got)
	}

	if err := client.UpdateApply(context.Background(), "0.5.1"); err != nil {
		t.Fatalf("UpdateApply() error: %v", err)
	}
	if fake.appliedUpdateVersion != "0.5.1" {
		t.Errorf("fake.appliedUpdateVersion = %q, want %q", fake.appliedUpdateVersion, "0.5.1")
	}
}

// TestClientUpdateCheckAndRollbackRoundTrip covers the remaining two methods
// (UpdateCheck, UpdateRollback) plus a status-view field the apply/status
// test above doesn't exercise (Pending), and pins that UpdateRollback
// reaches the fake with no arguments to check.
func TestClientUpdateCheckAndRollbackRoundTrip(t *testing.T) {
	fake := &fakeAPI{updateStatus: core.UpdateStatusView{
		Running: "0.5.0",
		Pending: &core.UpdatePendingView{Version: "0.5.1", From: "0.5.0", Deadline: 1234, Rollback: false},
	}}
	path := startTestServer(t, fake, "")

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	got, err := client.UpdateCheck(context.Background())
	if err != nil {
		t.Fatalf("UpdateCheck() error: %v", err)
	}
	if got.Pending == nil || got.Pending.Version != "0.5.1" {
		t.Errorf("UpdateCheck() = %+v, want Pending.Version=0.5.1", got)
	}

	if err := client.UpdateRollback(); err != nil {
		t.Fatalf("UpdateRollback() error: %v", err)
	}
	if !fake.updateRollbackCalled {
		t.Error("fake.updateRollbackCalled = false, want true")
	}
}

// TestClientUpdateMethodsSurfaceErrors proves an error the daemon-side api
// returns from any of the four Update* methods comes back over the wire with
// its exact message -- the same error-propagation proof
// TestClientValidateChannelSurfacesError gives ValidateChannel, here for the
// self-update surface (#122, task 8).
func TestClientUpdateMethodsSurfaceErrors(t *testing.T) {
	wantErr := "update: signature does not verify against any trusted key"
	fake := &fakeAPI{
		updateStatusErr:   errors.New(wantErr),
		updateCheckErr:    errors.New(wantErr),
		updateApplyErr:    errors.New(wantErr),
		updateRollbackErr: errors.New(wantErr),
	}
	path := startTestServer(t, fake, "")

	client, err := Dial(path, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	if _, err := client.UpdateStatus(); err == nil || err.Error() != wantErr {
		t.Errorf("UpdateStatus() error = %v, want %q", err, wantErr)
	}
	if _, err := client.UpdateCheck(context.Background()); err == nil || err.Error() != wantErr {
		t.Errorf("UpdateCheck() error = %v, want %q", err, wantErr)
	}
	if err := client.UpdateApply(context.Background(), "0.5.1"); err == nil || err.Error() != wantErr {
		t.Errorf("UpdateApply() error = %v, want %q", err, wantErr)
	}
	if err := client.UpdateRollback(); err == nil || err.Error() != wantErr {
		t.Errorf("UpdateRollback() error = %v, want %q", err, wantErr)
	}
}
