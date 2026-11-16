package hivetool

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	x, _ := newExec()
	x.RequireEvidence = true
	if _, err := run(t, x, call("1", ToolRepoRead, `{"path":"src/pkg/a.go"}`)); err != nil {
		t.Fatal(err)
	}
	x.headSHA, x.prNumber = "abc", 12
	x.verified = []verifyRecord{{sha: "abc", report: "ok"}}

	raw, err := json.Marshal(x.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}

	y, _ := newExec()
	y.RequireEvidence = true
	y.Restore(st)
	// Duplicate memory survives.
	if _, err := run(t, y, call("2", ToolRepoRead, `{"path":"src/pkg/a.go"}`)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("restored executor re-ran a served read: %v", err)
	}
	// Evidence survives: a citation of the earlier read verifies.
	if _, err := run(t, y, call("3", ToolTaskComplete, `{"summary":"cause at src/pkg/a.go:3"}`)); err != nil {
		t.Fatalf("restored evidence rejected: %v", err)
	}
	s2 := y.Snapshot()
	if s2.HeadSHA != "abc" || s2.PRNumber != 12 || len(s2.Verified) != 1 || s2.Verified[0].Report != "ok" || s2.Outcome == nil {
		t.Fatalf("snapshot = %+v", s2)
	}
	// Restoring an outcome-bearing state, then an empty one, clears it.
	z, _ := newExec()
	z.Restore(s2)
	if _, ok := z.Outcome(); !ok {
		t.Fatal("outcome not restored")
	}
	z.Restore(State{})
	if _, ok := z.Outcome(); ok || len(z.Snapshot().Evidence) != 0 {
		t.Fatal("empty restore kept state")
	}
}
