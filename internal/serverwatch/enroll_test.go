// Package serverwatch: enroll_test.go covers enrollState (enroll.go), the
// shared holder that makes the daemon's poll loop and the control socket's
// EnrollmentPIN read the SAME Telegram enrollment pin (#90).
package serverwatch

import (
	"sync"
	"testing"
)

// TestEnrollStatePINConfiguredNotEnrolled: telegram token set, chat id
// empty -> a stable non-empty pin, the same value across repeated calls
// (so the value the daemon prints and the value it later matches against
// in enrollMatch never drift).
func TestEnrollStatePINConfiguredNotEnrolled(t *testing.T) {
	e := &enrollState{}
	cfg := newTgConfig("tok", "")

	pin1, enrolled1 := e.PIN(cfg)
	if pin1 == "" {
		t.Fatalf("PIN() = %q, want a non-empty pin when configured and not enrolled", pin1)
	}
	if enrolled1 {
		t.Fatalf("enrolled = true, want false when chat id is empty")
	}

	pin2, enrolled2 := e.PIN(cfg)
	if pin2 != pin1 {
		t.Fatalf("PIN() = %q on second call, want the same stable pin %q", pin2, pin1)
	}
	if enrolled2 {
		t.Fatalf("enrolled = true on second call, want false")
	}
}

// TestEnrollStatePINEnrolled: once a chat id is set, PIN must report "" and
// enrolled=true -- there is nothing left to enroll with, so no pin should
// ever be shown or matched again.
func TestEnrollStatePINEnrolled(t *testing.T) {
	e := &enrollState{}
	cfg := newTgConfig("tok", "555")

	pin, enrolled := e.PIN(cfg)
	if pin != "" {
		t.Fatalf("PIN() = %q, want \"\" once enrolled", pin)
	}
	if !enrolled {
		t.Fatalf("enrolled = false, want true when chat id is set")
	}
}

// TestEnrollStatePINNotConfigured: no telegram token at all -> no pin to
// show (there is nothing to enroll into yet), and not reported enrolled.
func TestEnrollStatePINNotConfigured(t *testing.T) {
	e := &enrollState{}
	cfg := newTgConfig("", "")

	pin, enrolled := e.PIN(cfg)
	if pin != "" {
		t.Fatalf("PIN() = %q, want \"\" when telegram is not configured", pin)
	}
	if enrolled {
		t.Fatalf("enrolled = true, want false when telegram is not configured")
	}
}

// TestEnrollStateReset: Reset clears the cached pin (white-box: this test
// lives in package serverwatch, so it can inspect e.pin directly), so the
// next PIN() call generates a fresh one rather than reusing a pin that was
// already consumed by a successful enrollment.
func TestEnrollStateReset(t *testing.T) {
	e := &enrollState{}
	cfg := newTgConfig("tok", "")

	if pin, _ := e.PIN(cfg); pin == "" {
		t.Fatalf("PIN() = %q, want a non-empty pin before Reset", pin)
	}

	e.Reset()

	if e.pin != "" {
		t.Fatalf("after Reset e.pin = %q, want \"\" (cleared)", e.pin)
	}

	if pin, _ := e.PIN(cfg); pin == "" {
		t.Fatalf("PIN() after Reset = %q, want a freshly generated non-empty pin", pin)
	}
}

// TestEnrollStatePINConcurrentCallsAgree (-race): concurrent PIN() calls
// against the same unenrolled config must all observe the SAME pin -- the
// mutex must serialize the generate-once-and-cache path, not race two
// callers into caching different pins.
func TestEnrollStatePINConcurrentCallsAgree(t *testing.T) {
	e := &enrollState{}
	cfg := newTgConfig("tok", "")

	const n = 50
	pins := make([]string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			pin, _ := e.PIN(cfg)
			pins[i] = pin
		}(i)
	}
	wg.Wait()

	want := pins[0]
	if want == "" {
		t.Fatalf("pins[0] = \"\", want a non-empty pin")
	}
	for i, p := range pins {
		if p != want {
			t.Fatalf("pins[%d] = %q, want %q (all concurrent callers must agree)", i, p, want)
		}
	}
}
