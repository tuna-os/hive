package revertsheriff

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func commits(outcomes ...Outcome) []Commit {
	out := make([]Commit, len(outcomes))
	for i, o := range outcomes {
		out[i] = Commit{SHA: string(rune('a' + i)), Outcome: o}
	}
	return out
}

func TestFindCulprit(t *testing.T) {
	tests := []struct {
		name       string
		in         []Commit
		culprit    string
		lastGreen  string
		ok         bool
		reasonPart string
	}{
		{name: "tip broke it", in: commits(Red, Green, Green), culprit: "a", lastGreen: "b", ok: true},
		{name: "older commit broke it, later commits stay red", in: commits(Red, Red, Red, Green), culprit: "c", lastGreen: "d", ok: true},
		{name: "tip green", in: commits(Green, Red), ok: false, reasonPart: "tip is green"},
		{name: "skipped commit between culprit and last green", in: commits(Red, Skipped, Green), culprit: "a", lastGreen: "c", ok: true},
		{name: "skipped commit inside the red run", in: commits(Red, Skipped, Red, Green), culprit: "c", lastGreen: "d", ok: true},
		{name: "only skipped below the tip", in: commits(Red, Skipped, Skipped), ok: false, reasonPart: "no green result in the 3"},
		{name: "tip pending", in: commits(Pending, Red, Green), ok: false, reasonPart: "tip is pending"},
		{name: "gap without a result", in: commits(Red, Unknown, Green), ok: false, reasonPart: "no completed result"},
		{name: "pending parent", in: commits(Red, Pending, Green), ok: false, reasonPart: "no completed result"},
		{name: "red beyond lookback", in: commits(Red, Red), ok: false, reasonPart: "no green result in the 2"},
		{name: "empty", in: nil, ok: false, reasonPart: "no commits"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, reason, ok := FindCulprit(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v (reason %q), want %v", ok, reason, tc.ok)
			}
			if ok && (f.Culprit != tc.culprit || f.LastGreen != tc.lastGreen) {
				t.Fatalf("finding = %+v, want culprit %q last green %q", f, tc.culprit, tc.lastGreen)
			}
			if !ok && !strings.Contains(reason, tc.reasonPart) {
				t.Fatalf("reason = %q, want it to contain %q", reason, tc.reasonPart)
			}
		})
	}
}

func TestCaseConfirmsOnlyOnARedRerun(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	c := &Case{Repo: "o/r", Check: "Lint", Phase: Suspected}
	if got := c.Advance(Observation{}, t0, time.Hour); got != RequestRerun {
		t.Fatalf("suspected step = %v, want RequestRerun", got)
	}
	c.Rerun(nil, t0)
	// The original failing run predates the request: it is not the re-run.
	if got := c.Advance(Observation{Outcome: Red, StartedAt: t0.Add(-time.Minute)}, t0.Add(time.Minute), time.Hour); got != Wait {
		t.Fatalf("stale red step = %v, want Wait", got)
	}
	if got := c.Advance(Observation{Outcome: Pending, StartedAt: t0.Add(time.Second)}, t0.Add(2*time.Minute), time.Hour); got != Wait {
		t.Fatalf("pending re-run step = %v, want Wait", got)
	}
	if got := c.Advance(Observation{Outcome: Red, StartedAt: t0.Add(time.Second)}, t0.Add(10*time.Minute), time.Hour); got != Revert {
		t.Fatalf("red re-run step = %v, want Revert", got)
	}
	if c.Phase != Confirmed {
		t.Fatalf("phase = %v, want confirmed", c.Phase)
	}
}

func TestCaseGreenRerunIsFlaky(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	c := &Case{Phase: Suspected}
	c.Rerun(nil, t0)
	if got := c.Advance(Observation{Outcome: Green, StartedAt: t0.Add(time.Second)}, t0.Add(5*time.Minute), time.Hour); got != Done {
		t.Fatalf("green re-run step = %v, want Done", got)
	}
	if c.Phase != Flaky {
		t.Fatalf("phase = %v, want flaky", c.Phase)
	}
	if got := c.Advance(Observation{Outcome: Red, StartedAt: t0.Add(time.Hour)}, t0.Add(2*time.Hour), time.Hour); got != Done {
		t.Fatalf("a flaky case must stay done, got %v", got)
	}
}

func TestCaseGivesUp(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	c := &Case{Phase: Suspected}
	c.Rerun(errors.New("check run is not a GitHub Actions job"), t0)
	if c.Phase != GaveUp || !c.Phase.Terminal() {
		t.Fatalf("phase = %v, want terminal gave-up", c.Phase)
	}

	c = &Case{Phase: Suspected}
	c.Rerun(nil, t0)
	if got := c.Advance(Observation{Outcome: Pending, StartedAt: t0.Add(time.Second)}, t0.Add(3*time.Hour+time.Second), 3*time.Hour); got != Done || c.Phase != GaveUp {
		t.Fatalf("timed-out re-run: step %v phase %v, want Done gave-up", got, c.Phase)
	}
}

func TestGate(t *testing.T) {
	base := PRFacts{Number: 7, Title: "feat: thing"}
	if reason, ok := Gate(base, 0, 2); !ok {
		t.Fatalf("plain PR refused: %s", reason)
	}
	cases := map[string]PRFacts{
		"open revert":    {Number: 7, Title: "feat", OpenRevert: 9},
		"sheriff revert": {Number: 7, Title: "x", Labels: []string{"Hive/Revert"}},
		"manual revert":  {Number: 7, Title: "Revert \"feat: thing\""},
		"protected path": {Number: 7, Title: "feat", ProtectedPaths: []string{"cosign.pub"}},
	}
	for name, pr := range cases {
		if _, ok := Gate(pr, 0, 2); ok {
			t.Errorf("%s: gate allowed the revert", name)
		}
	}
	if _, ok := Gate(base, 2, 2); ok {
		t.Error("daily cap not enforced")
	}
	if _, ok := Gate(base, 50, 0); !ok {
		t.Error("max_per_day 0 must mean no cap")
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	body := "Reverts #12.\n\n" + Marker("Tuna-OS/TunaOS", 12, "abc")
	if !MarkerFor(body, "tuna-os/tunaos", 12) {
		t.Fatal("marker not found for its own PR")
	}
	if MarkerFor(body, "tuna-os/tunaos", 1) {
		t.Fatal("#1 must not match the marker for #12")
	}
}

func TestLedger(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	l := NewLedger()
	first := l.Open(&Case{Repo: "O/R", Check: "Lint", Finding: Finding{Culprit: "a"}})
	if got := l.Open(&Case{Repo: "o/r", Check: "Lint", Finding: Finding{Culprit: "a"}}); got != first {
		t.Fatal("re-opening the same culprit must keep the existing case")
	}
	if got := l.Open(&Case{Repo: "o/r", Check: "Lint", Finding: Finding{Culprit: "b"}}); got == first {
		t.Fatal("a new culprit must replace the case")
	}
	l.Open(&Case{Repo: "o/r", Check: "Docs", Tip: "t1", Phase: GaveUp})
	if got := l.Open(&Case{Repo: "o/r", Check: "Docs", Tip: "t2", Phase: GaveUp}); got.Tip != "t2" {
		t.Fatalf("an unattributable case must record the new tip, got %q", got.Tip)
	}
	l.Open(&Case{Repo: "o/r", Check: "Unit Tests", Finding: Finding{Culprit: "c"}})
	l.Settle("o/r", map[string]bool{"Unit Tests": true})
	if l.Case("o/r", "Lint") != nil {
		t.Fatal("a check no longer red must be settled")
	}
	if l.Case("o/r", "Unit Tests") == nil {
		t.Fatal("a still-red check must be kept")
	}

	l.RecordRevert("o/r", now.Add(-25*time.Hour))
	l.RecordRevert("o/r", now.Add(-time.Hour))
	if got := l.RevertsSince("O/R", now); got != 1 {
		t.Fatalf("reverts in the last day = %d, want 1", got)
	}
}
