package redmain

import (
	"reflect"
	"testing"
	"time"
)

func TestTrackerRedIgnoresThresholdAndClearsWhenGreen(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	tr.Observe("Tuna-OS/TunaOS", "main", map[string]RedRun{"Unit Tests": {}, "Lint": {}}, now)

	branch, checks := tr.Red("tuna-os/tunaos")
	if branch != "main" || !reflect.DeepEqual(checks, []string{"Lint", "Unit Tests"}) {
		t.Fatalf("Red = %q %v, want main [Lint Unit Tests]", branch, checks)
	}
	// Seconds old: the breaker has not tripped, but the checks are red.
	if tr.Active("tuna-os/tunaos", time.Hour, now.Add(time.Second)) != nil {
		t.Fatal("breaker must not trip before its threshold")
	}

	tr.Observe("tuna-os/tunaos", "main", map[string]RedRun{"Lint": {}}, now.Add(time.Minute))
	if _, checks := tr.Red("tuna-os/tunaos"); !reflect.DeepEqual(checks, []string{"Lint"}) {
		t.Fatalf("after Unit Tests went green, Red = %v, want [Lint]", checks)
	}
	tr.Observe("tuna-os/tunaos", "main", nil, now.Add(2*time.Minute))
	if _, checks := tr.Red("tuna-os/tunaos"); len(checks) != 0 {
		t.Fatalf("green main: Red = %v, want none", checks)
	}
}
