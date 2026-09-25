package main

import "github.com/InfoDiveLabs/trinetra/internal/config"

// scheduleMode enumerates the mutually exclusive choices ctl's Schedule
// screen offers: off (both keys cleared), a single daily HH:MM firing
// time, or a single weekly dow@HH:MM firing time. The underlying config
// keys (schedule.daily/schedule.weekly) are independent knobs that
// `trinetra schedule daily|weekly ...` (systemd.go's cmdSchedule) can
// each set without touching the other, but the ctl screen presents them as
// one mutually exclusive choice for a simpler guided flow: picking daily
// clears any existing weekly schedule and vice versa, and off clears both.
type scheduleMode int

const (
	scheduleOff scheduleMode = iota
	scheduleDaily
	scheduleWeekly
)

// scheduleAnswers accumulates the Schedule screen's input. Daily/Weekly are
// the same raw strings config.Set("schedule.daily"/"schedule.weekly", ...)
// validates (HH:MM / dow@HH:MM), so the screen never has to duplicate
// parseHM/validateWeekly's rules.
type scheduleAnswers struct {
	Mode   scheduleMode
	Daily  string // "HH:MM", used when Mode == scheduleDaily
	Weekly string // "dow@HH:MM", used when Mode == scheduleWeekly
}

// applySchedule applies ans onto cfg via the SAME validated config.Set
// setters `trinetra schedule daily|weekly` uses (schedule.daily/
// schedule.weekly), so ctl gets identical HH:MM/dow@HH:MM validation for
// free. Exactly one of schedule.daily/schedule.weekly ends up non-empty (or
// both empty for Off): the other key is explicitly cleared so a stale value
// from a previous CLI-driven schedule (or a previous visit to this screen)
// never leaves both active at once. Returns the first validation error
// encountered; nothing is applied to the daemon until the caller posts cfg
// via api.ApplyConfig.
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
