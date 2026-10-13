package main

import "github.com/InfoDiveLabs/trinetra/internal/config"

// scheduleMode enumerates the mutually exclusive choices ctl's Schedule screen offers: off
// (both keys cleared), a single daily HH:MM firing time.
type scheduleMode int

const (
	scheduleOff scheduleMode = iota
	scheduleDaily
	scheduleWeekly
)

// scheduleAnswers accumulates the Schedule screen's input.
type scheduleAnswers struct {
	Mode   scheduleMode
	Daily  string // "HH:MM", used when Mode == scheduleDaily
	Weekly string // "dow@HH:MM", used when Mode == scheduleWeekly
}

// applySchedule applies ans onto cfg via the SAME validated config.Set setters `trinetra
// schedule daily|weekly` uses (schedule.daily/ schedule.weekly).
func applySchedule(cfg *config.Config, ans scheduleAnswers) error {
	switch ans.Mode {
	case scheduleDaily:
		if err := cfg.Set("schedule.daily", ans.Daily); err != nil {
			return err
		}
		return cfg.Set("schedule.weekly", "")
	case scheduleWeekly:
		if err := cfg.Set("schedule.weekly", ans.Weekly); err != nil {
			return err
		}
		return cfg.Set("schedule.daily", "")
	default: // scheduleOff
		if err := cfg.Set("schedule.daily", ""); err != nil {
			return err
		}
		return cfg.Set("schedule.weekly", "")
	}
}
