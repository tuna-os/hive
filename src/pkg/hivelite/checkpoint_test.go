package hivelite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/coordstate"
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/hivetool"
	"github.com/hivecommons/hive/pkg/turn"
)

func storedCheckpoint(t *testing.T, s coordstate.Store) (Checkpoint, string) {
	t.Helper()
	raw, _, err := s.Get(context.Background(), coordstate.SessionKey("hive-lite:o/r#7"))
	if err != nil {
		t.Fatalf("no checkpoint: %v", err)
	}
	var ck Checkpoint
	if err := json.Unmarshal(raw, &ck); err != nil {
		t.Fatal(err)
	}
	return ck, string(raw)
}

func TestCapacityPauseResumesWithEvidence(t *testing.T) {
	store := coordstate.NewMemoryStore()
	capacity := fmt.Errorf("all providers cooling down: %w", turn.ErrInferenceCapacity)

	// Pass 1 reads the code, then every provider is out of capacity.
	llm1 := &scripted{
		steps: []turn.LLMResponse{tc(hivetool.ToolRepoRead, readA)},
		errAt: map[int]error{1: capacity},
	}
	c1 := cfg(llm1)
	c1.State = store
	res, err := RunTriage(context.Background(), c1)
	if !errors.Is(err, turn.ErrInferenceCapacity) || res.Resumed {
		t.Fatalf("pass 1: %+v %v", res, err)
	}
	ck, raw := storedCheckpoint(t, store)
	if ck.Done || ck.Admission != extwork.ModeReportOnly || ck.Envelope.TurnCount != 1 || len(ck.Executor.Evidence["src/a.go"]) == 0 {
		t.Fatalf("checkpoint = %+v", ck)
	}
	if strings.Contains(raw, "HIVE-CANARY-") {
		t.Fatal("canary reached the store")
	}

	// Pass 2 continues: the citation of what pass 1 read verifies, and a
	// repeated read is refused as a duplicate.
	llm2 := &scripted{steps: []turn.LLMResponse{
		tc(hivetool.ToolRepoRead, readA),
		tc(hivetool.ToolTaskComplete, `{"summary":"Root cause at src/a.go:3"}`),
	}}
	c2 := cfg(llm2)
	c2.State = store
	res, err = RunTriage(context.Background(), c2)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resumed || res.Outcome.Tool != hivetool.ToolTaskComplete || res.Verified != 1 {
		t.Fatalf("pass 2 = %+v", res)
	}
	first := llm2.seen[0]
	if !strings.Contains(first[0].Content, "HIVE-CANARY-") || first[0].Role != turn.RoleSystem {
		t.Fatal("system prompt with a fresh canary was not restored")
	}
	if !strings.Contains(first[1].Content, "The rebase is refused.") {
		t.Fatal("resumed session lost the pinned task message")
	}
	found := false
	for _, m := range llm2.seen[1] {
		if m.Role == turn.RoleTool && strings.Contains(m.Content, "duplicate call") {
			found = true
		}
	}
	if !found {
		t.Fatal("pass 2 re-ran a read pass 1 already served")
	}
	ck, raw = storedCheckpoint(t, store)
	if !ck.Done || strings.Contains(raw, "HIVE-CANARY-") {
		t.Fatalf("final checkpoint done=%v", ck.Done)
	}

	// A finished checkpoint is not resumed: the next run starts fresh.
	llm3 := &scripted{steps: []turn.LLMResponse{
		tc(hivetool.ToolRepoRead, readA),
		tc(hivetool.ToolTaskComplete, `{"summary":"Again src/a.go:3"}`),
	}}
	c3 := cfg(llm3)
	c3.State = store
	if res, err := RunTriage(context.Background(), c3); err != nil || res.Resumed || res.Verified != 1 {
		t.Fatalf("pass 3 = %+v %v", res, err)
	}
}

func TestCheckpointNotResumedAcrossModes(t *testing.T) {
	store := coordstate.NewMemoryStore()
	llm := &scripted{steps: []turn.LLMResponse{tc(hivetool.ToolRepoRead, readA)}, errAt: map[int]error{1: turn.ErrInferenceCapacity}}
	c := cfg(llm)
	c.State = store
	if _, err := RunTriage(context.Background(), c); !errors.Is(err, turn.ErrInferenceCapacity) {
		t.Fatal(err)
	}
	fix := cfg(&scripted{steps: []turn.LLMResponse{tc(hivetool.ToolTaskAbort, `{"reason":"nothing"}`)}})
	fix.State, fix.Fix, fix.Writer = store, true, &memWriter{refs: map[string]string{}}
	res, err := RunTriage(context.Background(), fix)
	if err != nil || res.Resumed {
		t.Fatalf("fix session resumed a report-only checkpoint: %+v %v", res, err)
	}
}

// conflictStore simulates another pass writing between our load and save.
type conflictStore struct{ *coordstate.MemoryStore }

func (c conflictStore) Put(context.Context, string, []byte, coordstate.Version) (coordstate.Version, error) {
	return "", coordstate.ErrConflict
}

func TestCheckpointConflictIsReported(t *testing.T) {
	c := cfg(&scripted{steps: []turn.LLMResponse{
		tc(hivetool.ToolRepoRead, readA),
		tc(hivetool.ToolTaskComplete, `{"summary":"Root cause at src/a.go:3"}`),
	}})
	c.State = conflictStore{coordstate.NewMemoryStore()}
	if _, err := RunTriage(context.Background(), c); !errors.Is(err, coordstate.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestCorruptCheckpointStartsFresh(t *testing.T) {
	store := coordstate.NewMemoryStore()
	if _, err := store.Put(context.Background(), coordstate.SessionKey("hive-lite:o/r#7"), []byte("{not json"), ""); err != nil {
		t.Fatal(err)
	}
	c := cfg(&scripted{steps: []turn.LLMResponse{
		tc(hivetool.ToolRepoRead, readA),
		tc(hivetool.ToolTaskComplete, `{"summary":"Root cause at src/a.go:3"}`),
	}})
	c.State = store
	res, err := RunTriage(context.Background(), c)
	if err != nil || res.Resumed {
		t.Fatalf("%+v %v", res, err)
	}
	if ck, _ := storedCheckpoint(t, store); !ck.Done {
		t.Fatal("corrupt checkpoint was not overwritten")
	}
}

func TestRestoreSystemPrependsWhenMissing(t *testing.T) {
	env := turn.SessionEnvelope{Messages: []turn.Message{{Role: turn.RoleUser, Content: "task"}}}
	restoreSystem(&env, "sys")
	if len(env.Messages) != 2 || env.Messages[0].Role != turn.RoleSystem || env.Messages[0].Content != "sys" {
		t.Fatalf("%+v", env.Messages)
	}
}

func TestResumeResetsFailedStatus(t *testing.T) {
	store := coordstate.NewMemoryStore()
	c1 := cfg(&scripted{
		steps: []turn.LLMResponse{tc(hivetool.ToolRepoRead, readA)},
		errAt: map[int]error{1: errors.New("provider rejected request (HTTP 400)")},
	})
	c1.State = store
	if _, err := RunTriage(context.Background(), c1); err == nil {
		t.Fatal("pass 1 should fail")
	}
	if ck, _ := storedCheckpoint(t, store); ck.Done {
		t.Fatal("a provider error must leave the session resumable")
	}
	c2 := cfg(&scripted{steps: []turn.LLMResponse{tc(hivetool.ToolTaskComplete, `{"summary":"Root cause at src/a.go:3"}`)}})
	c2.State = store
	res, err := RunTriage(context.Background(), c2)
	if err != nil || !res.Resumed || res.Status == turn.StatusFailed || res.Verified != 1 {
		t.Fatalf("pass 2 = %+v %v", res, err)
	}
}
