package config

import (
	"testing"
	"time"
)

func isoWindow() FreezeWindow {
	return FreezeWindow{
		Name: "iso", Anchor: "2026-11-02", CycleDays: 14, StartDay: 11, EndDay: 14,
		TZ: "America/New_York", Agents: []string{"architect", "Strategist"},
	}
}

func TestFreezeWindowCycleDay(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	w := isoWindow()
	cases := []struct {
		at   time.Time
		day  int
		hold bool
	}{
		{time.Date(2026, 11, 2, 0, 0, 0, 0, ny), 1, false},          // anchor is day 1
		{time.Date(2026, 11, 11, 23, 59, 0, 0, ny), 10, false},      // last open day
		{time.Date(2026, 11, 12, 0, 0, 0, 0, ny), 11, true},         // freeze starts at local midnight
		{time.Date(2026, 11, 15, 23, 59, 0, 0, ny), 14, true},       // last frozen day
		{time.Date(2026, 11, 16, 0, 0, 0, 0, ny), 1, false},         // next cycle
		{time.Date(2026, 11, 29, 12, 0, 0, 0, ny), 14, true},        // second cycle's window
		{time.Date(2026, 10, 31, 12, 0, 0, 0, ny), 13, true},        // before the anchor: previous cycle
		{time.Date(2026, 11, 12, 4, 30, 0, 0, time.UTC), 10, false}, // still Nov 11 in New York
		{time.Date(2027, 3, 14, 12, 0, 0, 0, ny), 7, false},         // DST change day stays a whole day
		{time.Date(2027, 3, 20, 1, 0, 0, 0, ny), 13, true},          // after DST, no drift
	}
	for _, c := range cases {
		if got := w.CycleDay(c.at); got != c.day {
			t.Errorf("CycleDay(%s) = %d, want %d", c.at, got, c.day)
		}
		if got := w.Active(c.at); got != c.hold {
			t.Errorf("Active(%s) = %v, want %v", c.at, got, c.hold)
		}
	}
}

func TestFreezeWindowValidate(t *testing.T) {
	bad := map[string]func(*FreezeWindow){
		"tz":        func(w *FreezeWindow) { w.TZ = "Mars/Olympus" },
		"anchor":    func(w *FreezeWindow) { w.Anchor = "11/02/2026" },
		"cycle":     func(w *FreezeWindow) { w.CycleDays = 0 },
		"start":     func(w *FreezeWindow) { w.StartDay = 0 },
		"end<start": func(w *FreezeWindow) { w.EndDay = 10 },
		"end>cycle": func(w *FreezeWindow) { w.EndDay = 15 },
		"agents":    func(w *FreezeWindow) { w.Agents = nil },
	}
	if err := isoWindow().Validate(); err != nil {
		t.Fatalf("valid window rejected: %v", err)
	}
	for name, mutate := range bad {
		w := isoWindow()
		mutate(&w)
		if w.Validate() == nil {
			t.Errorf("%s: invalid window accepted", name)
		}
		if w.Active(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)) {
			t.Errorf("%s: an invalid window must never hold an agent", name)
		}
	}
}

func TestActiveFreezeWindow(t *testing.T) {
	g := GovernorConfig{FreezeWindows: []FreezeWindow{isoWindow()}}
	frozen := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	open := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if got := g.ActiveFreezeWindow(frozen, "strategist"); got != "iso" {
		t.Errorf("strategist in window = %q, want iso (agent match is case-insensitive)", got)
	}
	if got := g.ActiveFreezeWindow(frozen, "ci-maintainer"); got != "" {
		t.Errorf("unlisted agent held by %q", got)
	}
	if got := g.ActiveFreezeWindow(open, "architect"); got != "" {
		t.Errorf("held outside the window by %q", got)
	}
	if got := (GovernorConfig{}).ActiveFreezeWindow(frozen, "architect"); got != "" {
		t.Errorf("no windows configured, still held by %q", got)
	}
}
