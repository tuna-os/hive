package config

import (
	"fmt"
	"strings"
	"time"
)

// FreezeWindow (governor.freeze_windows[]) pauses chosen agents on fixed days
// of a repeating cycle, such as the last four days of a 14-day release cycle
// when only stabilisation work should run.
//
// A cycle starts on Anchor (day 1) and repeats every CycleDays days, counted
// in TZ. The window covers days StartDay through EndDay of every cycle,
// inclusive. While it is active, each agent in Agents resolves to a paused
// cadence in every governor mode, exactly as `pause` does; every other agent
// is untouched. Mode cadences cannot express this: cron day-of-month resets
// each month, so "days 11-14 of a 14-day cycle" drifts.
type FreezeWindow struct {
	// Name labels the window in logs.
	Name string `yaml:"name" json:"name"`
	// Anchor is day 1 of some cycle, as YYYY-MM-DD in TZ. Dates before and
	// after it both resolve, so it can be any past or future cycle start.
	Anchor string `yaml:"anchor" json:"anchor"`
	// CycleDays is the cycle length in days.
	CycleDays int `yaml:"cycle_days" json:"cycle_days"`
	// StartDay and EndDay bound the window, 1-based and inclusive.
	StartDay int `yaml:"start_day" json:"start_day"`
	EndDay   int `yaml:"end_day" json:"end_day"`
	// TZ is the IANA zone the days are counted in. Empty means UTC.
	TZ string `yaml:"tz,omitempty" json:"tz,omitempty"`
	// Agents are paused while the window is active. A replica is held when
	// its base agent is listed.
	Agents []string `yaml:"agents" json:"agents"`
}

func (w FreezeWindow) location() (*time.Location, error) {
	if strings.TrimSpace(w.TZ) == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(strings.TrimSpace(w.TZ))
}

// Validate reports the first problem with the window.
func (w FreezeWindow) Validate() error {
	label := w.Name
	if strings.TrimSpace(label) == "" {
		label = "(unnamed)"
	}
	loc, err := w.location()
	if err != nil {
		return fmt.Errorf("governor.freeze_windows %s: tz: %w", label, err)
	}
	if _, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(w.Anchor), loc); err != nil {
		return fmt.Errorf("governor.freeze_windows %s: anchor must be YYYY-MM-DD: %w", label, err)
	}
	if w.CycleDays < 1 {
		return fmt.Errorf("governor.freeze_windows %s: cycle_days must be at least 1", label)
	}
	if w.StartDay < 1 || w.EndDay < w.StartDay || w.EndDay > w.CycleDays {
		return fmt.Errorf("governor.freeze_windows %s: need 1 <= start_day <= end_day <= cycle_days", label)
	}
	if len(w.Agents) == 0 {
		return fmt.Errorf("governor.freeze_windows %s: agents must name at least one agent", label)
	}
	return nil
}

// CycleDay returns the 1-based day of the cycle that now falls on, or 0 when
// the window does not validate.
func (w FreezeWindow) CycleDay(now time.Time) int {
	if w.Validate() != nil {
		return 0
	}
	loc, _ := w.location()
	anchor, _ := time.ParseInLocation("2006-01-02", strings.TrimSpace(w.Anchor), loc)
	local := now.In(loc)
	// Compare calendar dates as UTC midnights: UTC has no DST, so a clock
	// change in TZ cannot move the window by an hour or a day.
	a := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, time.UTC)
	t := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	days := int(t.Sub(a).Hours()) / 24
	day := days % w.CycleDays
	if day < 0 {
		day += w.CycleDays
	}
	return day + 1
}

// Active reports whether now falls inside the window.
func (w FreezeWindow) Active(now time.Time) bool {
	day := w.CycleDay(now)
	return day != 0 && day >= w.StartDay && day <= w.EndDay
}

// Holds reports whether the window names agent (case-insensitive).
func (w FreezeWindow) Holds(agent string) bool {
	for _, a := range w.Agents {
		if strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(agent)) {
			return true
		}
	}
	return false
}

// ActiveFreezeWindow returns the name of the first window that is active at
// now and holds one of names, or "" when none does.
func (g GovernorConfig) ActiveFreezeWindow(now time.Time, names ...string) string {
	for _, w := range g.FreezeWindows {
		if !w.Active(now) {
			continue
		}
		for _, n := range names {
			if n != "" && w.Holds(n) {
				if w.Name != "" {
					return w.Name
				}
				return "freeze window"
			}
		}
	}
	return ""
}
