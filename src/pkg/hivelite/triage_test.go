package hivelite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/hivetool"
	"github.com/hivecommons/hive/pkg/turn"
)

// scripted returns one canned response per call and records what it saw.
type scripted struct {
	steps []turn.LLMResponse
	errAt map[int]error
	calls int
	seen  [][]turn.Message
	tools [][]toolapprove.ToolRequest
}

func (s *scripted) Generate(_ context.Context, msgs []turn.Message, tools []toolapprove.ToolRequest) (turn.LLMResponse, error) {
	i := s.calls
	s.calls++
	s.seen = append(s.seen, append([]turn.Message(nil), msgs...))
	s.tools = append(s.tools, tools)
	if err := s.errAt[i]; err != nil {
		return turn.LLMResponse{}, err
	}
	if i >= len(s.steps) {
		return turn.LLMResponse{Content: "still thinking"}, nil
	}
	return s.steps[i], nil
}

func tc(name, args string) turn.LLMResponse {
	return turn.LLMResponse{ToolCalls: []turn.ToolCall{{ID: fmt.Sprintf("%s-%d", name, len(args)), Name: name, Arguments: args}}}
}

type reader struct{ body string }

func (r reader) Issue(context.Context, string, int) (hivetool.Issue, error) {
	return hivetool.Issue{Number: 7, Title: "stuck", Body: r.body, State: "open"}, nil
}
func (reader) List(context.Context, string, string, string) ([]hivetool.Entry, error) {
	return nil, nil
}
func (reader) File(_ context.Context, _, _, p string) ([]byte, error) {
	if p == "src/a.go" {
		return []byte("package a\n\nfunc Rebase() {}\n"), nil
	}
	return nil, errors.New("not found")
}
func (reader) Checks(context.Context, string, string) ([]hivetool.CheckRun, error) { return nil, nil }

func cfg(llm turn.LLMClient) Config {
	return Config{Repo: "o/r", Ref: "main", Issue: 7, LLM: llm, Reader: reader{body: "The rebase is refused."}, MaxTurns: 6, FinishAfter: 3}
}

const readA = `{"path":"src/a.go"}`

func TestExplicitGroundedCompletion(t *testing.T) {
	llm := &scripted{steps: []turn.LLMResponse{
		tc(hivetool.ToolRepoRead, readA),
		tc(hivetool.ToolTaskComplete, `{"summary":"Root cause at src/a.go:3"}`),
	}}
	res, err := RunTriage(context.Background(), cfg(llm))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome.Tool != hivetool.ToolTaskComplete || res.Verified != 1 || res.Implicit || res.Withheld != "" || res.Turns != 2 {
		t.Fatalf("res = %+v", res)
	}
	// The screened issue body is pinned in the task message, and a canary
	// preamble is in the system primer.
	first := llm.seen[0]
	if !strings.Contains(first[1].Content, "The rebase is refused.") || !strings.Contains(first[1].Content, "untrusted data") {
		t.Fatalf("task message = %q", first[1].Content)
	}
	if !strings.Contains(first[0].Content, "HIVE-CANARY-") {
		t.Fatal("no canary in the primer")
	}
}

func TestFinishPhaseOffersOnlyTerminalToolsAndAcceptsGroundedProse(t *testing.T) {
	llm := &scripted{steps: []turn.LLMResponse{
		tc(hivetool.ToolRepoRead, readA),
		tc(hivetool.ToolRepoSearch, `{"query":"x"}`), // no searcher: errors, still a turn
		tc(hivetool.ToolRepoRead, `{"path":"src/a.go","start_line":2}`),
		{Content: "The root cause is in src/a.go:3 (Rebase)."}, // prose in finish phase
	}}
	res, err := RunTriage(context.Background(), cfg(llm))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Implicit || res.Verified != 1 {
		t.Fatalf("res = %+v", res)
	}
	last := llm.tools[len(llm.tools)-1]
	if len(last) != 2 || last[0].Tool != hivetool.ToolTaskComplete {
		t.Fatalf("finish phase offered %+v", last)
	}
}

func TestUngroundedProseIsNotAResult(t *testing.T) {
	llm := &scripted{steps: []turn.LLMResponse{
		{Content: "It is probably the scheduler; see src/x.go:10."}, // early prose -> finish phase
		{Content: "Final: src/x.go:10."},                            // ungrounded prose
	}}
	res, err := RunTriage(context.Background(), cfg(llm))
	if !errors.Is(err, ErrNoResult) {
		t.Fatalf("err = %v res = %+v", err, res)
	}
	if res.Turns != 2 {
		t.Fatalf("turns = %d (early prose must jump straight to the finish phase)", res.Turns)
	}
}

func TestCanaryEchoIsWithheld(t *testing.T) {
	llm := &scripted{}
	llm.steps = []turn.LLMResponse{tc(hivetool.ToolRepoRead, readA)}
	// Second step echoes the canary it was shown in the primer.
	echo := &echoCanary{inner: llm}
	res, err := RunTriage(context.Background(), cfg(echo))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Withheld, "canary") {
		t.Fatalf("withheld = %q", res.Withheld)
	}
}

type echoCanary struct{ inner *scripted }

func (e *echoCanary) Generate(ctx context.Context, msgs []turn.Message, tools []toolapprove.ToolRequest) (turn.LLMResponse, error) {
	if e.inner.calls == 0 {
		return e.inner.Generate(ctx, msgs, tools)
	}
	e.inner.calls++
	tok := ""
	for _, f := range strings.Fields(msgs[0].Content) {
		if strings.HasPrefix(f, "HIVE-CANARY-") {
			tok = f
		}
	}
	return tc(hivetool.ToolTaskComplete, fmt.Sprintf(`{"summary":"src/a.go:3 and the marker %s"}`, tok)), nil
}

func TestCapacityExhaustionPropagates(t *testing.T) {
	llm := &scripted{errAt: map[int]error{0: fmt.Errorf("all 429: %w", turn.ErrInferenceCapacity)}}
	res, err := RunTriage(context.Background(), cfg(llm))
	if !errors.Is(err, turn.ErrInferenceCapacity) || res.Status != turn.StatusPaused {
		t.Fatalf("err = %v status = %s", err, res.Status)
	}
}

func TestRenderComment(t *testing.T) {
	r := Result{Turns: 9, Verified: 1, Outcome: hivetool.Outcome{Tool: hivetool.ToolTaskComplete, Summary: "Root cause src/a.go:3",
		Citations: []hivetool.Citation{{Path: "src/a.go", Line: 3, Verified: true}, {Path: "README.md", Line: 9}}}}
	c := RenderComment(r, "workers-ai-glm (@cf/zai-org/glm-4.7-flash)")
	for _, want := range []string{CommentMarker, "1 of 2 citations verified", "Root cause src/a.go:3", "unverified: `README.md:9`", "Advisory only"} {
		if !strings.Contains(c, want) {
			t.Errorf("comment missing %q:\n%s", want, c)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := RunTriage(context.Background(), Config{}); err == nil {
		t.Fatal("empty config must error")
	}
}

func TestAdmissionPostureGatesFixMode(t *testing.T) {
	base := Config{Repo: "o/r", Ref: "main", Issue: 1, LLM: &scripted{}, Reader: reader{}}
	for _, c := range []struct {
		admission string
		fix       bool
		want      string
	}{
		{"", false, extwork.ModeReportOnly},
		{"", true, extwork.ModeToolHosted},
		{extwork.ModeReportOnly, true, "needs a tool-hosted admission"},
		{extwork.ModeGitHubMediated, true, "needs a tool-hosted admission"},
		{extwork.ModeShadow, false, "cannot run a session"},
		{"enforce", false, "cannot run a session"},
	} {
		cfg := base
		cfg.Admission, cfg.Fix = c.admission, c.fix
		if c.fix {
			cfg.Writer = &memWriter{refs: map[string]string{}}
		}
		err := cfg.defaults()
		if strings.HasPrefix(c.want, "needs") || strings.HasPrefix(c.want, "cannot") {
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%q fix=%v: err = %v, want %q", c.admission, c.fix, err, c.want)
			}
			continue
		}
		if err != nil || cfg.Admission != c.want {
			t.Errorf("%q fix=%v: admission = %q, err %v", c.admission, c.fix, cfg.Admission, err)
		}
	}
}

type memWriter struct {
	refs map[string]string
	pr   int
}

func (w *memWriter) RefSHA(_ context.Context, _, b string) (string, error) {
	if s, ok := w.refs[b]; ok {
		return s, nil
	}
	return "", hivetool.ErrRefNotFound
}
func (w *memWriter) CreateCommit(context.Context, string, string, string, map[string]string) (string, error) {
	return "c1", nil
}
func (w *memWriter) SetBranch(_ context.Context, _, b, sha string, _ bool) error {
	w.refs[b] = sha
	return nil
}
func (w *memWriter) OpenPR(context.Context, string, string, string, string, string) (int, string, error) {
	w.pr = 77
	return 77, "u", nil
}

func TestFixModeOpensDraftPRThroughHive(t *testing.T) {
	llm := &scripted{steps: []turn.LLMResponse{
		tc(hivetool.ToolRepoRead, readA),
		tc(hivetool.ToolRepoPatch, `{"commit_message":"fix","patches":[{"path":"src/a.go","search":"func Rebase() {}","replace":"func Rebase() { recreate() }"}]}`),
		tc(hivetool.ToolPRCreate, `{"title":"fix: recreate","body":"Use recreate."}`),
		tc(hivetool.ToolTaskComplete, `{"summary":"Changed src/a.go:3"}`),
	}}
	w := &memWriter{refs: map[string]string{"main": "c0"}}
	c := cfg(llm)
	c.Fix, c.Writer = true, w
	res, err := RunTriage(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome.PRNumber != 77 || w.refs["hive-lite/issue-7"] != "c1" || w.refs["main"] != "c0" {
		t.Fatalf("res=%+v refs=%v", res, w.refs)
	}
	var offered []string
	for _, d := range llm.tools[0] {
		offered = append(offered, d.Tool)
	}
	if !strings.Contains(strings.Join(offered, ","), hivetool.ToolRepoPatch) {
		t.Fatalf("fix mode did not offer write tools: %v", offered)
	}
	if !strings.Contains(RenderComment(res, "m"), "Draft PR:** #77") {
		t.Fatal("comment does not link the draft PR")
	}
}

func TestReportModeOffersNoWriteTools(t *testing.T) {
	llm := &scripted{steps: []turn.LLMResponse{tc(hivetool.ToolRepoPatch, `{"commit_message":"x","patches":[{"path":"src/a.go","content":"x"}]}`)}}
	_, _ = RunTriage(context.Background(), cfg(llm))
	for _, d := range llm.tools[0] {
		if d.Tool == hivetool.ToolRepoPatch || d.Tool == hivetool.ToolPRCreate {
			t.Fatalf("report mode offered %s", d.Tool)
		}
	}
}

type cmpWriter struct{ memWriter }

func (cmpWriter) Compare(context.Context, string, string, string) (string, error) {
	return "-\treturn total / perPage\n+\treturn (total + perPage - 1) / perPage", nil
}

func TestResumedFixSessionSeesPriorDiff(t *testing.T) {
	llm := &scripted{steps: []turn.LLMResponse{tc(hivetool.ToolTaskAbort, `{"reason":"already fixed"}`)}}
	w := &cmpWriter{memWriter{refs: map[string]string{"main": "c0", "hive-lite/issue-7": "c5"}}}
	c := cfg(llm)
	c.Fix, c.Writer = true, w
	_, _ = RunTriage(context.Background(), c)
	task := llm.seen[0][1].Content
	if !strings.Contains(task, "already holds changes") || !strings.Contains(task, "(total + perPage - 1)") {
		t.Fatalf("task = %q", task)
	}
	// A fresh branch gets no note.
	llm2 := &scripted{steps: []turn.LLMResponse{tc(hivetool.ToolTaskAbort, `{"reason":"x"}`)}}
	c2 := cfg(llm2)
	c2.Fix, c2.Writer = true, &cmpWriter{memWriter{refs: map[string]string{"main": "c0"}}}
	_, _ = RunTriage(context.Background(), c2)
	if strings.Contains(llm2.seen[0][1].Content, "already holds") {
		t.Fatal("fresh branch must not get a resume note")
	}
}
