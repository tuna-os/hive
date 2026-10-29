package governor

import (
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// A freeze window pauses exactly the agents it lists while it is active, and
// they resume their mode cadence when it ends, with no config reload.
func TestFreezeWindowPausesListedAgentsOnly(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"architect":     {Enabled: true},
		"architect-2":   {Enabled: true, ReplicaOf: "architect", ReplicaIndex: 2, ReplicaCount: 2},
		"ci-maintainer": {Enabled: true},
	}
	g := New(config.GovernorConfig{
		Modes: map[string]config.ModeConfig{"idle": {Cadences: map[string]config.Cadence{
			"architect": "10m", "ci-maintainer": "10m",
		}}},
		FreezeWindows: []config.FreezeWindow{{
			Name: "iso", Anchor: "2026-11-02", CycleDays: 14, StartDay: 11, EndDay: 14,
			Agents: []string{"architect"},
		}},
	}, agents, slog.Default())

	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC) // day 11: frozen
	g.now = func() time.Time { return now }
	due := g.Evaluate(0, 0, 0, 0)
	if slices.Contains(due, "architect") || slices.Contains(due, "architect-2") {
		t.Fatalf("frozen agents were due: %v", due)
	}
	if !slices.Contains(due, "ci-maintainer") {
		t.Fatalf("unlisted agent not due during the window: %v", due)
	}
	if c := g.state.Cadences[config.CadenceTargetKey("architect", "")]; !c.Paused {
		t.Errorf("architect cadence = %+v, want paused", c)
	}

	now = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC) // day 1 of the next cycle
	due = g.Evaluate(0, 0, 0, 0)
	if !slices.Contains(due, "architect") {
		t.Fatalf("architect not due after the window ended: %v", due)
	}
}
