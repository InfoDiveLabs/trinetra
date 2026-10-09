// Package trinetra: enroll_test.go covers enrollState (enroll.go).
package trinetra

import (
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// newEnrollCfg builds an unclaimed-bot config (token set, no chat id) with the given
// brute-force bound so the Attempt tests run at a small.
func newEnrollCfg(maxAttempts, cooldownSec int) *config.Config {
	c := newTgConfig("tok", "")
	c.Telegram.MaxEnrollAttempts = maxAttempts
	c.Telegram.EnrollCooldown = cooldownSec
	return c
}

// TestEnrollStatePINConfiguredNotEnrolled: telegram token set, chat id empty -> a stable
// non-empty pin, the same value across repeated calls.
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

// TestEnrollStatePINEnrolled: once a chat id is set, PIN must report "" and enrolled=true
// -- there is nothing left to enroll with, so no pin should ever be shown or matched again.
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

// TestEnrollStateReset: Reset clears the cached pin (white-box: this test lives in package
// trinetra, so it can inspect e.pin directly).
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

// TestEnrollStatePINConcurrentCallsAgree (-race): concurrent PIN() calls against the same
// unenrolled config must all observe the SAME pin.
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

var enrollBase = time.Unix(1_000_000, 0)

// TestEnrollAttemptCorrectPINSucceeds: a correct "/start <pin>" enrolls (true)
// and leaves no failure state behind.
func TestEnrollAttemptCorrectPINSucceeds(t *testing.T) {
	e := &enrollState{pin: "424242"}
	cfg := newEnrollCfg(3, 60)

	if !e.Attempt(cfg, "/start 424242", enrollBase) {
		t.Fatal("Attempt(correct pin) = false, want true")
	}
	if e.failures != 0 {
		t.Errorf("failures = %d after success, want 0", e.failures)
	}
	if !e.cooldownUntil.IsZero() {
		t.Errorf("cooldownUntil set after a success, want zero")
	}
}

// TestEnrollAttemptNonStartNeverCounts: messages that are not a well-formed "/start <arg>"
// are ignored WITHOUT advancing the failure counter.
func TestEnrollAttemptNonStartNeverCounts(t *testing.T) {
	e := &enrollState{pin: "424242"}
	cfg := newEnrollCfg(3, 60)

	for _, text := range []string{"/stats", "hello", "/start", "/start 1 2", "start 424242"} {
		if e.Attempt(cfg, text, enrollBase) {
			t.Errorf("Attempt(%q) = true, want false", text)
		}
	}
	if e.failures != 0 {
		t.Errorf("failures = %d, want 0 (non-/start messages must not count)", e.failures)
	}
	if e.pin != "424242" {
		t.Errorf("pin rotated to %q by non-attempt messages, want unchanged 424242", e.pin)
	}
}

// TestEnrollAttemptWrongGuessThenCorrectResets: wrong guesses below the threshold
// accumulate but don't rotate.
func TestEnrollAttemptWrongGuessThenCorrectResets(t *testing.T) {
	e := &enrollState{pin: "424242"}
	cfg := newEnrollCfg(5, 60) // threshold 5, so 2 wrong guesses don't rotate

	for i := 0; i < 2; i++ {
		if e.Attempt(cfg, "/start 000000", enrollBase) {
			t.Fatalf("wrong guess %d = true, want false", i)
		}
	}
	if e.failures != 2 {
		t.Fatalf("failures = %d after 2 wrong guesses, want 2", e.failures)
	}
	if e.pin != "424242" {
		t.Fatalf("pin rotated to %q below threshold, want unchanged 424242", e.pin)
	}
	if !e.Attempt(cfg, "/start 424242", enrollBase) {
		t.Fatal("Attempt(correct pin) after wrong guesses = false, want true")
	}
	if e.failures != 0 {
		t.Errorf("failures = %d after a correct guess, want 0 (counter must reset)", e.failures)
	}
}

// TestEnrollAttemptRotatesAndCoolsDownAtThreshold is the core #93 property.
func TestEnrollAttemptRotatesAndCoolsDownAtThreshold(t *testing.T) {
	e := &enrollState{pin: "424242"}
	cfg := newEnrollCfg(3, 60)

	// Three wrong guesses: the third crosses the threshold and rotates.
	for i := 0; i < 3; i++ {
		if e.Attempt(cfg, "/start 000000", enrollBase) {
			t.Fatalf("wrong guess %d = true, want false", i)
		}
	}
	if e.pin == "424242" {
		t.Fatal("pin was not rotated after reaching the attempt threshold")
	}
	if e.failures != 0 {
		t.Errorf("failures = %d after rotation, want 0 (reset)", e.failures)
	}
	wantCooldown := enrollBase.Add(60 * time.Second)
	if !e.cooldownUntil.Equal(wantCooldown) {
		t.Errorf("cooldownUntil = %v, want %v", e.cooldownUntil, wantCooldown)
	}
	rotated := e.pin

	// During the cooldown even the correct (rotated) pin is ignored.
	if e.Attempt(cfg, "/start "+rotated, enrollBase.Add(30*time.Second)) {
		t.Error("Attempt during cooldown = true, want false (rate-limited)")
	}

	// After the cooldown, the rotated pin enrolls.
	if !e.Attempt(cfg, "/start "+rotated, enrollBase.Add(61*time.Second)) {
		t.Error("Attempt(rotated pin) after cooldown = false, want true")
	}
}

// TestEnrollAttemptGatedWhenNotEnrollable: with no token (nothing to enroll) or once a chat
// id is set (already enrolled), Attempt never enrolls, matching PIN()'s gate.
func TestEnrollAttemptGatedWhenNotEnrollable(t *testing.T) {
	notConfigured := &config.Config{} // no token
	if (&enrollState{}).Attempt(notConfigured, "/start 424242", enrollBase) {
		t.Error("Attempt with no telegram token = true, want false")
	}

	claimed := newEnrollCfg(3, 60)
	claimed.Telegram.ChatID = "111" // already enrolled
	if (&enrollState{pin: "424242"}).Attempt(claimed, "/start 424242", enrollBase) {
		t.Error("Attempt on an already-enrolled bot = true, want false")
	}
}
