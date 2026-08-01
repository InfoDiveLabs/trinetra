// Package serverwatch: coreapi_enroll_test.go covers core.API's
// EnrollmentPIN (#90) on both implementations: inprocAPI reads through the
// shared enrollState it was constructed with (coreapi_inproc.go), while
// fileAPI -- a separate CLI process with no live daemon state -- always
// returns errEnrollNeedsDaemon (coreapi_file.go).
package serverwatch

import (
	"context"
	"testing"

	"serverwatch/internal/config"
)

// TestInprocEnrollmentPINReadsThroughHolder pins that inprocAPI.EnrollmentPIN
// is nothing but a pass-through to the enrollState it was constructed with,
// called against the live getCfg -- the same enroll.PIN(c) call pollLoop
// makes each iteration (daemon.go), so a socket caller sees the exact same
// pin the daemon is matching /start <pin> against.
func TestInprocEnrollmentPINReadsThroughHolder(t *testing.T) {
	cfg := newTgConfig("tok", "")
	enroll := &enrollState{}
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return cfg }, nil, t.TempDir(), nil, nil, enroll)

	wantPin, wantEnrolled := enroll.PIN(cfg)

	pin, enrolled, err := api.EnrollmentPIN(context.Background())
	if err != nil {
		t.Fatalf("EnrollmentPIN() error = %v, want nil", err)
	}
	if pin != wantPin || enrolled != wantEnrolled {
		t.Errorf("EnrollmentPIN() = %q, %v; want %q, %v (the exact enrollState value)", pin, enrolled, wantPin, wantEnrolled)
	}
}

// TestInprocEnrollmentPINReflectsEnrolledState pins the enrolled=true path:
// once the config's chat id is set, EnrollmentPIN must report "" and
// enrolled=true, mirroring enrollState.PIN's own contract.
func TestInprocEnrollmentPINReflectsEnrolledState(t *testing.T) {
	cfg := newTgConfig("tok", "555")
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return cfg }, nil, t.TempDir(), nil, nil, &enrollState{})

	pin, enrolled, err := api.EnrollmentPIN(context.Background())
	if err != nil {
		t.Fatalf("EnrollmentPIN() error = %v, want nil", err)
	}
	if pin != "" || !enrolled {
		t.Errorf("EnrollmentPIN() = %q, %v; want \"\", true once enrolled", pin, enrolled)
	}
}

// TestFileAPIEnrollmentPINReturnsClearError pins fileAPI's contract: a
// separate CLI process has no live enrollState to ask, so it always returns
// errEnrollNeedsDaemon rather than a stale or fabricated pin.
func TestFileAPIEnrollmentPINReturnsClearError(t *testing.T) {
	api := newFileAPI(t.TempDir(), config.Default())

	pin, enrolled, err := api.EnrollmentPIN(context.Background())
	if err != errEnrollNeedsDaemon {
		t.Fatalf("EnrollmentPIN() error = %v, want errEnrollNeedsDaemon", err)
	}
	if pin != "" || enrolled {
		t.Errorf("EnrollmentPIN() = %q, %v on error, want zero values", pin, enrolled)
	}
}
