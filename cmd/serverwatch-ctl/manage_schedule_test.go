package main

import (
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestApplyScheduleDaily(t *testing.T) {
	cfg := &config.Config{}
	cfg.Schedule.Weekly = "mon@09:00" // pre-existing weekly, should be cleared
	err := applySchedule(cfg, scheduleAnswers{Mode: scheduleDaily, Daily: "03:30"})
	if err != nil {
		t.Fatalf("applySchedule() error = %v, want nil", err)
	}
	if cfg.Schedule.Daily != "03:30" {
		t.Errorf("Schedule.Daily = %q, want 03:30", cfg.Schedule.Daily)
	}
	if cfg.Schedule.Weekly != "" {
		t.Errorf("Schedule.Weekly = %q, want cleared", cfg.Schedule.Weekly)
	}
}

func TestApplyScheduleWeekly(t *testing.T) {
	cfg := &config.Config{}
	cfg.Schedule.Daily = "03:30" // pre-existing daily, should be cleared
	err := applySchedule(cfg, scheduleAnswers{Mode: scheduleWeekly, Weekly: "mon@09:00"})
	if err != nil {
		t.Fatalf("applySchedule() error = %v, want nil", err)
	}
	if cfg.Schedule.Weekly != "mon@09:00" {
		t.Errorf("Schedule.Weekly = %q, want mon@09:00", cfg.Schedule.Weekly)
	}
	if cfg.Schedule.Daily != "" {
		t.Errorf("Schedule.Daily = %q, want cleared", cfg.Schedule.Daily)
	}
}

func TestApplyScheduleOffClearsBoth(t *testing.T) {
	cfg := &config.Config{}
	cfg.Schedule.Daily = "03:30"
	cfg.Schedule.Weekly = "mon@09:00"
	err := applySchedule(cfg, scheduleAnswers{Mode: scheduleOff})
	if err != nil {
		t.Fatalf("applySchedule() error = %v, want nil", err)
	}
	if cfg.Schedule.Daily != "" || cfg.Schedule.Weekly != "" {
		t.Errorf("Schedule = %+v, want both cleared", cfg.Schedule)
	}
}

func TestApplyScheduleDailyInvalidRejected(t *testing.T) {
	cfg := &config.Config{}
	err := applySchedule(cfg, scheduleAnswers{Mode: scheduleDaily, Daily: "25:99"})
	if err == nil {
		t.Fatal("applySchedule() error = nil, want a validation error for 25:99")
	}
}

func TestApplyScheduleWeeklyInvalidRejected(t *testing.T) {
	cfg := &config.Config{}
	err := applySchedule(cfg, scheduleAnswers{Mode: scheduleWeekly, Weekly: "someday@09:00"})
	if err == nil {
		t.Fatal("applySchedule() error = nil, want a validation error for an invalid day of week")
	}
}
