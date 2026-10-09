// Package trinetra: coreapi_enroll_test.go covers core.API's EnrollmentPIN (#90) on both
// implementations.
package trinetra

import (
	"context"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// TestInprocEnrollmentPINReadsThroughHolder pins that inprocAPI.EnrollmentPIN is nothing.
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

// TestInprocEnrollmentPINReflectsEnrolledState pins the enrolled=true path: once the
// config's chat id is set, EnrollmentPIN must report "" and enrolled=true.
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

// TestFileAPIEnrollmentPINReturnsClearError pins fileAPI's contract: a separate CLI process
// has no live enrollState to ask.
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
