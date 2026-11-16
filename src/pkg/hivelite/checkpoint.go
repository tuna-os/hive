package hivelite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/coordstate"
	"github.com/hivecommons/hive/pkg/hivetool"
	"github.com/hivecommons/hive/pkg/turn"
)

// checkpointVersion is bumped when the checkpoint shape changes; an older
// checkpoint is discarded rather than misread.
const checkpointVersion = 1

// canaryPlaceholder replaces the session canary wherever it appears before a
// checkpoint is written: a state branch on a public repository is public.
const canaryPlaceholder = "[hive-canary]"

// Checkpoint is what a pass stores so the next one can continue.
type Checkpoint struct {
	Version     int                  `json:"version"`
	Admission   string               `json:"admission"`
	Fix         bool                 `json:"fix,omitempty"`
	Done        bool                 `json:"done"`
	Finishing   bool                 `json:"finishing,omitempty"`
	FinishAfter int                  `json:"finish_after"`
	Envelope    turn.SessionEnvelope `json:"envelope"`
	Executor    hivetool.State       `json:"executor"`
	UpdatedAt   time.Time            `json:"updated_at"`
}

type checkpointer struct {
	store     coordstate.Store
	key       string
	canary    string
	admission string
	fix       bool
	version   coordstate.Version
}

func newCheckpointer(cfg Config, sessionID, canary string) *checkpointer {
	if cfg.State == nil {
		return nil
	}
	return &checkpointer{store: cfg.State, key: coordstate.SessionKey(sessionID), canary: canary, admission: cfg.Admission, fix: cfg.Fix}
}

// load returns the stored checkpoint and whether it should be resumed: it
// must be unfinished, current-version, and for the same admission and mode
// (a report-only checkpoint never resumes as a fix session or vice versa).
func (c *checkpointer) load(ctx context.Context) (Checkpoint, bool, error) {
	if c == nil {
		return Checkpoint{}, false, nil
	}
	raw, ver, err := c.store.Get(ctx, c.key)
	if errors.Is(err, coordstate.ErrNotFound) {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("hivelite: load checkpoint: %w", err)
	}
	c.version = ver
	var ck Checkpoint
	if err := json.Unmarshal(raw, &ck); err != nil || ck.Version != checkpointVersion {
		return Checkpoint{}, false, nil // unreadable or stale shape: start fresh, overwrite
	}
	ok := !ck.Done && ck.Admission == c.admission && ck.Fix == c.fix && len(ck.Envelope.Messages) > 0
	return ck, ok, nil
}

// save writes the checkpoint under CAS against the version this pass
// loaded. A conflict means another pass ran this session concurrently;
// that is reported, never overwritten.
func (c *checkpointer) save(ctx context.Context, env turn.SessionEnvelope, ex *hivetool.Executor, finishing bool, finishAfter int, done bool) error {
	if c == nil {
		return nil
	}
	ck := Checkpoint{
		Version: checkpointVersion, Admission: c.admission, Fix: c.fix, Done: done,
		Finishing: finishing, FinishAfter: finishAfter,
		Envelope: blankSystem(env), Executor: ex.Snapshot(), UpdatedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(ck)
	if err != nil {
		return err
	}
	// The canary is a prefix plus hex, so it serialises verbatim: one
	// replace over the encoded checkpoint covers every field (messages,
	// operations, approvals, journal, outcome) at once.
	if c.canary != "" {
		raw = bytes.ReplaceAll(raw, []byte(c.canary), []byte(canaryPlaceholder))
	}
	// Persist even when the pass's own context is spent (deadline exits are
	// exactly the ones worth resuming).
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	ver, err := c.store.Put(sctx, c.key, raw, c.version)
	if err != nil {
		return fmt.Errorf("hivelite: save checkpoint: %w", err)
	}
	c.version = ver
	return nil
}

// blankSystem returns a copy of env whose system message (which carries the
// canary preamble) is emptied; restoreSystem rebuilds it on resume.
func blankSystem(env turn.SessionEnvelope) turn.SessionEnvelope {
	out := env.Clone()
	for i := range out.Messages {
		if out.Messages[i].Role == turn.RoleSystem {
			out.Messages[i].Content = ""
			if out.Messages[i].Metadata == nil {
				out.Messages[i].Metadata = map[string]string{}
			}
			out.Messages[i].Metadata["hive_system"] = "rebuilt on resume"
		}
	}
	return out
}

// restoreSystem puts this pass's system prompt (with its fresh canary) back
// into a resumed envelope.
func restoreSystem(env *turn.SessionEnvelope, system string) {
	for i := range env.Messages {
		if env.Messages[i].Role == turn.RoleSystem {
			env.Messages[i].Content = system
			delete(env.Messages[i].Metadata, "hive_system")
			return
		}
	}
	env.Messages = append([]turn.Message{{Role: turn.RoleSystem, Content: system, Timestamp: time.Now().UTC()}}, env.Messages...)
}
