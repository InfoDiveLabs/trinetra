package main

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// TestOnboardCheckEntersOnboardingWhenNoToken asserts Init's fetchOnboardCheckCmd, once its
// onboardCheckMsg lands on a model still sitting on Home.
func TestOnboardCheckEntersOnboardingWhenNoToken(t *testing.T) {
	m := newModel(&fakeAPI{cfg: &config.Config{}})
	var mm tea.Model = m
	mm, cmd := mm.Update(onboardCheckMsg{cfg: &config.Config{}})
	if cmd == nil {
		t.Fatal("expected a Cmd focusing the token input, got nil")
	}
	got := mm.(model)
	if got.step != stepOnboard {
		t.Fatalf("step = %v, want stepOnboard", got.step)
	}
	if got.onboard.screen != onboardTokenStep {
		t.Fatalf("onboard.screen = %v, want onboardTokenStep", got.onboard.screen)
	}
}

// TestOnboardCheckSkippedWhenEnrolled asserts a config that's already
// configured and enrolled never enters onboarding.
func TestOnboardCheckSkippedWhenEnrolled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.Token = "abc"
	cfg.Telegram.ChatID = "123"
	m := newModel(&fakeAPI{cfg: cfg})
	var mm tea.Model = m
	mm, _ = mm.Update(onboardCheckMsg{cfg: cfg})
	if mm.(model).step != stepHome {
		t.Fatalf("step = %v, want stepHome (already enrolled)", mm.(model).step)
	}
}

// TestOnboardCheckSkippedIfAlreadyNavigatedAway asserts the auto-entry never fires once the
// user has already left Home.
func TestOnboardCheckSkippedIfAlreadyNavigatedAway(t *testing.T) {
	m := newModel(&fakeAPI{cfg: &config.Config{}})
	m.step = stepSetupWeb
	var mm tea.Model = m
	mm, _ = mm.Update(onboardCheckMsg{cfg: &config.Config{}})
	if mm.(model).step != stepSetupWeb {
		t.Fatalf("step = %v, want stepSetupWeb (unchanged)", mm.(model).step)
	}
}

// TestOnboardTokenFlowShowsPIN drives the token step end to end: typing a token and
// pressing enter issues applyOnboardTokenCmd; once that succeeds.
func TestOnboardTokenFlowShowsPIN(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}, enrollPIN: "7734", enrollEnrolled: false}
	var mm tea.Model = newModel(api)
	mm, _ = mm.Update(onboardCheckMsg{cfg: &config.Config{}}) // enters onboarding

	mm = typeString(t, mm, "mytoken")
	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("expected applyOnboardTokenCmd, got nil")
	}
	msg := runCmd(t, cmd)
	applied, ok := msg.(onboardTokenAppliedMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want onboardTokenAppliedMsg", msg)
	}
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	if api.applied == nil || api.applied.Telegram.Token != "mytoken" {
		t.Fatalf("applied telegram.token = %+v, want mytoken", api.applied)
	}

	mm, cmd = mm.Update(applied)
	got := mm.(model)
	if got.onboard.screen != onboardPINStep {
		t.Fatalf("onboard.screen = %v, want onboardPINStep", got.onboard.screen)
	}
	if cmd == nil {
		t.Fatal("expected fetchOnboardPINCmd after the token is saved, got nil")
	}
	pinMsg := runCmd(t, cmd).(onboardPINMsg)
	if pinMsg.err != nil || pinMsg.enrolled || pinMsg.pin != "7734" {
		t.Fatalf("onboardPINMsg = %+v, want pin=7734 enrolled=false err=nil", pinMsg)
	}

	mm, cmd = mm.Update(pinMsg)
	got = mm.(model)
	if got.onboard.pin != "7734" || got.onboard.enrolled {
		t.Fatalf("onboard state = %+v, want pin=7734 enrolled=false", got.onboard)
	}
	if cmd == nil {
		t.Fatal("expected a poll tick to be scheduled while not yet enrolled")
	}
}

// TestOnboardPollDetectsEnrollment asserts a poll tick while on the pin screen re-fetches
// EnrollmentPIN, and once the fake reports enrolled=true, the model reflects it.
func TestOnboardPollDetectsEnrollment(t *testing.T) {
	api := &fakeAPI{enrollPIN: "1111", enrollEnrolled: false}
	m := newModel(api)
	m.step = stepOnboard
	m.onboard = onboardModel{screen: onboardPINStep, pin: "1111"}
	var mm tea.Model = m

	mm, cmd := mm.Update(onboardPollTickMsg{})
	if cmd == nil {
		t.Fatal("expected fetchOnboardPINCmd on a poll tick, got nil")
	}
	pinMsg := runCmd(t, cmd).(onboardPINMsg)
	if pinMsg.enrolled {
		t.Fatal("first poll should not yet report enrolled (fake still set to false)")
	}

	api.enrollEnrolled = true
	api.enrollPIN = ""
	mm, cmd = mm.Update(pinMsg) // still not enrolled -> schedules another tick
	if cmd == nil {
		t.Fatal("expected another poll tick scheduled")
	}
	mm, cmd = mm.Update(onboardPollTickMsg{})
	pinMsg2 := runCmd(t, cmd).(onboardPINMsg)
	if !pinMsg2.enrolled {
		t.Fatal("second poll should report enrolled (fake flipped)")
	}

	mm, cmd = mm.Update(pinMsg2)
	if cmd != nil {
		t.Error("expected no further poll tick once enrolled")
	}
	if !mm.(model).onboard.enrolled {
		t.Error("onboard.enrolled = false, want true")
	}

	if api.enrollCalls != 2 {
		t.Errorf("enrollCalls = %d, want 2 (one per poll)", api.enrollCalls)
	}
}

// TestOnboardPINStepAnyKeyReturnsHomeOnceEnrolled asserts pressing a key on
// the pin screen once enrolled returns to Home.
func TestOnboardPINStepAnyKeyReturnsHomeOnceEnrolled(t *testing.T) {
	m := newModel(&fakeAPI{})
	m.step = stepOnboard
	m.onboard = onboardModel{screen: onboardPINStep, enrolled: true}
	var mm tea.Model = m
	mm, _ = mm.Update(keyRunes('x'))
	if mm.(model).step != stepHome {
		t.Fatalf("step = %v, want stepHome", mm.(model).step)
	}
}

// TestOnboardTokenStepEscSkipsToHome asserts esc on the token step leaves
// onboarding without applying anything.
func TestOnboardTokenStepEscSkipsToHome(t *testing.T) {
	api := &fakeAPI{}
	m := newModel(api)
	m.step = stepOnboard
	m.onboard = onboardModel{screen: onboardTokenStep, tokenIn: newManageValueInput("")}
	var mm tea.Model = m
	mm, _ = mm.Update(keyType(tea.KeyEsc))
	if mm.(model).step != stepHome {
		t.Fatalf("step = %v, want stepHome", mm.(model).step)
	}
	if api.applyN != 0 {
		t.Errorf("ApplyConfig calls = %d, want 0", api.applyN)
	}
}

// TestOnboardTokenAppliedErrorSurfaces asserts a failing ApplyConfig
// surfaces on the token step rather than silently advancing to the pin step.
func TestOnboardTokenAppliedErrorSurfaces(t *testing.T) {
	wantErr := errors.New("apply failed")
	api := &fakeAPI{cfg: &config.Config{}, applyErr: wantErr}
	m := newModel(api)
	m.step = stepOnboard
	m.onboard = onboardModel{screen: onboardTokenStep, tokenIn: newManageValueInput("")}
	m.onboard.tokenIn.Focus()
	var mm tea.Model = m
	mm = typeString(t, mm, "tok")
	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	msg := runCmd(t, cmd).(onboardTokenAppliedMsg)
	mm, _ = mm.Update(msg)
	got := mm.(model)
	if got.onboard.screen != onboardTokenStep {
		t.Fatalf("onboard.screen = %v, want onboardTokenStep (unchanged on error)", got.onboard.screen)
	}
	if got.onboard.applyErr == nil || got.onboard.applyErr.Error() != wantErr.Error() {
		t.Fatalf("onboard.applyErr = %v, want %v", got.onboard.applyErr, wantErr)
	}
}
