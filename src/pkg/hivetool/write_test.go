package hivetool

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agentmode"
)

type fakeWriter struct {
	refs    map[string]string
	commits map[string]map[string]string // sha -> full file set at that commit
	prs     []string
	n       int
}

func newFakeWriter(base map[string]string) *fakeWriter {
	return &fakeWriter{refs: map[string]string{"main": "c0"}, commits: map[string]map[string]string{"c0": base}}
}

func (w *fakeWriter) RefSHA(_ context.Context, _, branch string) (string, error) {
	if s, ok := w.refs[branch]; ok {
		return s, nil
	}
	return "", ErrRefNotFound
}

func (w *fakeWriter) CreateCommit(_ context.Context, _, parent, _ string, files map[string]string) (string, error) {
	w.n++
	sha := fmt.Sprintf("c%d", w.n)
	next := map[string]string{}
	for k, v := range w.commits[parent] {
		next[k] = v
	}
	for k, v := range files {
		next[k] = v
	}
	w.commits[sha] = next
	return sha, nil
}

func (w *fakeWriter) SetBranch(_ context.Context, _, branch, sha string, create bool) error {
	if _, exists := w.refs[branch]; exists == create {
		return fmt.Errorf("create=%v but exists=%v", create, exists)
	}
	w.refs[branch] = sha
	return nil
}

func (w *fakeWriter) OpenPR(_ context.Context, _, head, base, title, body string) (int, string, error) {
	w.prs = append(w.prs, head+"->"+base+": "+title+"\n"+body)
	return 41 + len(w.prs), "https://example/pr", nil
}

// commitReader serves files from the fake writer's commits, so patches stack.
type commitReader struct {
	*fakeReader
	w *fakeWriter
}

func (r commitReader) File(ctx context.Context, repo, ref, file string) ([]byte, error) {
	if set, ok := r.w.commits[ref]; ok {
		if s, ok := set[file]; ok {
			return []byte(s), nil
		}
		return nil, fmt.Errorf("not found: %s", file)
	}
	return r.fakeReader.File(ctx, repo, ref, file)
}

func writeExec() (*Executor, *fakeWriter) {
	x, r := newExec()
	w := newFakeWriter(map[string]string{"src/pkg/a.go": "package a\n\nfunc Rebase() {}\n", ".github/workflows/ci.yml": "on: push\n"})
	x.Ref, x.Reader, x.Writer = "main", commitReader{fakeReader: r, w: w}, w
	x.WorkBranch, x.Mode, x.Canary, x.IssueRef = "hive-lite/issue-7", agentmode.ModeIssuesAndPRs, "HIVE-CANARY-deadbeef", 7
	return x, w
}

func TestPatchStacksOnWorkBranchAndOpensDraftPR(t *testing.T) {
	x, w := writeExec()
	out, err := run(t, x, call("1", ToolRepoPatch, `{"commit_message":"fix rebase","patches":[{"path":"src/pkg/a.go","search":"func Rebase() {}","replace":"func Rebase() { recreate() }"}]}`))
	if err != nil || !strings.Contains(out, "committed c1 on hive-lite/issue-7") {
		t.Fatalf("patch 1: %q %v", out, err)
	}
	// Second patch must apply on top of the first, not on the base.
	if _, err := run(t, x, call("2", ToolRepoPatch, `{"commit_message":"add helper","patches":[{"path":"src/pkg/a.go","search":"recreate() }","replace":"recreate() }\n\nfunc recreate() {}"},{"path":"src/pkg/new.go","content":"package a\n"}]}`)); err != nil {
		t.Fatal(err)
	}
	if got := w.commits[w.refs["hive-lite/issue-7"]]["src/pkg/a.go"]; !strings.Contains(got, "func recreate() {}") || !strings.Contains(got, "recreate() }") {
		t.Fatalf("stacked content = %q", got)
	}
	if w.refs["main"] != "c0" {
		t.Fatal("base branch must never move")
	}
	out, err = run(t, x, call("3", ToolPRCreate, `{"title":"fix: recreate","body":"Adds recreate."}`))
	if err != nil || !strings.Contains(out, "draft PR #42") {
		t.Fatalf("pr: %q %v", out, err)
	}
	if !strings.Contains(w.prs[0], "hive-lite/issue-7->main") || !strings.Contains(w.prs[0], "Refs #7") || strings.Contains(w.prs[0], "Fixes") {
		t.Fatalf("pr = %q", w.prs[0])
	}
	if _, err := run(t, x, call("4", ToolPRCreate, `{"title":"again","body":"x"}`)); err == nil {
		t.Fatal("second PR in one session must be refused")
	}
	if _, err := run(t, x, call("5", ToolTaskComplete, `{"summary":"done, see src/pkg/a.go:3"}`)); err != nil {
		t.Fatal(err)
	}
	if o, _ := x.Outcome(); o.PRNumber != 42 || !o.Citations[0].Verified {
		t.Fatalf("outcome = %+v (patched files count as evidence)", o)
	}
}

func TestWriteGuards(t *testing.T) {
	cases := []struct {
		name, tool, args, want string
		mutate                 func(*Executor)
	}{
		{"advisory mode", ToolRepoPatch, `{"commit_message":"m","patches":[{"path":"src/pkg/a.go","content":"x"}]}`, "may not push", func(x *Executor) { x.Mode = agentmode.ModeIssuesOnly }},
		{"no branch", ToolRepoPatch, `{"commit_message":"m","patches":[{"path":"src/pkg/a.go","content":"x"}]}`, "not enabled", func(x *Executor) { x.WorkBranch = "" }},
		{"protected", ToolRepoPatch, `{"commit_message":"m","patches":[{"path":".github/workflows/ci.yml","content":"x"}]}`, "protected paths", nil},
		{"escape", ToolRepoPatch, `{"commit_message":"m","patches":[{"path":"../x","content":"x"}]}`, "escapes", nil},
		{"ambiguous search", ToolRepoPatch, `{"commit_message":"m","patches":[{"path":"src/pkg/a.go","search":"a","replace":"b"}]}`, "matches", nil},
		{"missing search", ToolRepoPatch, `{"commit_message":"m","patches":[{"path":"src/pkg/a.go","search":"nope","replace":"b"}]}`, "matches 0 times", nil},
		{"no message", ToolRepoPatch, `{"patches":[{"path":"src/pkg/a.go","content":"x"}]}`, "commit_message", nil},
		{"canary", ToolRepoPatch, `{"commit_message":"m","patches":[{"path":"src/pkg/a.go","content":"HIVE-CANARY-deadbeef"}]}`, "canary", nil},
		{"too big", ToolRepoPatch, fmt.Sprintf(`{"commit_message":"m","patches":[{"path":"src/pkg/a.go","content":%q}]}`, strings.Repeat("x", MaxPatchBytes+1)), "limit", nil},
		{"pr before patch", ToolPRCreate, `{"title":"t","body":"b"}`, "no commits yet", nil},
		{"pr canary", ToolPRCreate, `{"title":"t","body":"HIVE-CANARY-deadbeef"}`, "canary", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, w := writeExec()
			if tc.mutate != nil {
				tc.mutate(x)
			}
			_, err := run(t, x, call(tc.name, tc.tool, tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(w.refs) != 1 || len(w.prs) != 0 {
				t.Fatalf("a refused write must not touch the repo: refs=%v prs=%v", w.refs, w.prs)
			}
		})
	}
}

func TestDefinitionsByMode(t *testing.T) {
	has := func(mode agentmode.AgentMode, tool string) bool {
		for _, d := range Definitions(mode) {
			if d.Tool == tool {
				return true
			}
		}
		return false
	}
	if has(agentmode.ModeIssuesOnly, ToolRepoPatch) || has(agentmode.ModeAdvisory, ToolPRCreate) {
		t.Fatal("write tools offered to a mode that cannot push")
	}
	if !has(agentmode.ModeIssuesAndPRs, ToolRepoPatch) || !has(agentmode.ModeIssuesAndPRs, ToolPRCreate) {
		t.Fatal("write tools missing for ISSUES_AND_PRS")
	}
	defs := Definitions(agentmode.ModeIssuesAndPRs)
	if defs[len(defs)-1].Tool != ToolTaskAbort || defs[len(defs)-2].Tool != ToolTaskComplete {
		t.Fatal("terminal tools must stay last")
	}
}

func TestRESTWriter(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(b))
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/git/ref/heads/main":
			io.WriteString(w, `{"object":{"sha":"base"}}`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/ref/heads/"):
			w.WriteHeader(404)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/git/commits/base":
			io.WriteString(w, `{"tree":{"sha":"tree0"}}`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/git/trees":
			io.WriteString(w, `{"sha":"tree1"}`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/git/commits":
			io.WriteString(w, `{"sha":"c1"}`)
		case r.URL.Path == "/repos/o/r/git/refs" || strings.HasPrefix(r.URL.Path, "/repos/o/r/git/refs/heads/"):
			io.WriteString(w, `{}`)
		case r.URL.Path == "/repos/o/r/pulls":
			io.WriteString(w, `{"number":9,"html_url":"u"}`)
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	wr := &RESTWriter{BaseURL: srv.URL}
	ctx := context.Background()
	if _, err := wr.RefSHA(ctx, "o/r", "hive-lite/x"); err != ErrRefNotFound {
		t.Fatalf("missing ref err = %v", err)
	}
	if s, _ := wr.RefSHA(ctx, "o/r", "main"); s != "base" {
		t.Fatal(s)
	}
	sha, err := wr.CreateCommit(ctx, "o/r", "base", "m", map[string]string{"b.go": "B", "a.go": "A"})
	if err != nil || sha != "c1" {
		t.Fatal(sha, err)
	}
	if err := wr.SetBranch(ctx, "o/r", "hive-lite/x", "c1", true); err != nil {
		t.Fatal(err)
	}
	if err := wr.SetBranch(ctx, "o/r", "hive-lite/x", "c2", false); err != nil {
		t.Fatal(err)
	}
	if n, _, err := wr.OpenPR(ctx, "o/r", "hive-lite/x", "main", "t", "b"); err != nil || n != 9 {
		t.Fatal(n, err)
	}
	joined := strings.Join(seen, "\n")
	for _, want := range []string{`"base_tree":"tree0"`, `"path":"a.go"`, `"parents":["c1"]`[:10], `"ref":"refs/heads/hive-lite/x"`, `"force":false`, `"draft":true`} {
		if !strings.Contains(joined, want) {
			t.Errorf("requests missing %s:\n%s", want, joined)
		}
	}
}

func TestWholeFileRewriteThatDropsCodeIsRefused(t *testing.T) {
	x, w := writeExec()
	w.commits["c0"]["src/pkg/big.go"] = "package a\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n\nfunc D() {}\n\nfunc E() {}\n\nfunc F() {}\n\nfunc G() {}\n"
	_, err := run(t, x, call("1", ToolRepoPatch, `{"commit_message":"m","patches":[{"path":"src/pkg/big.go","content":"package a\n\nfunc A() { fixed() }\n"}]}`))
	if err == nil || !strings.Contains(err.Error(), "use search/replace") {
		t.Fatalf("err = %v", err)
	}
	// A whole-file write that keeps the file is fine, as is a new file.
	if _, err := run(t, x, call("2", ToolRepoPatch, `{"commit_message":"m","patches":[{"path":"src/pkg/new.go","content":"package a\n"}]}`)); err != nil {
		t.Fatal(err)
	}
}

type stubVerifier struct {
	ok    bool
	calls int
}

func (v *stubVerifier) Verify(context.Context, map[string]string) (string, bool, error) {
	v.calls++
	return "--- FAIL: TestPageCount", v.ok, nil
}

func TestVerifierGatesTheCommit(t *testing.T) {
	x, w := writeExec()
	v := &stubVerifier{}
	x.Verifier = v
	args := `{"commit_message":"m","patches":[{"path":"src/pkg/a.go","search":"func Rebase() {}","replace":"func Rebase() { bad }"}]}`
	_, err := run(t, x, call("1", ToolRepoPatch, args))
	if err == nil || !strings.Contains(err.Error(), "FAIL: TestPageCount") || !strings.Contains(err.Error(), "nothing was committed") {
		t.Fatalf("err = %v", err)
	}
	if len(w.refs) != 1 {
		t.Fatal("a failed verification must not commit")
	}
	// The same call may be retried after a failure (not a duplicate).
	v.ok = true
	if _, err := run(t, x, call("2", ToolRepoPatch, args)); err != nil {
		t.Fatal(err)
	}
	if v.calls != 2 || w.refs["hive-lite/issue-7"] == "" {
		t.Fatalf("calls=%d refs=%v", v.calls, w.refs)
	}
}

// A session resuming on an existing work branch must read the branch head,
// not the base (live: it re-patched text the branch had already changed).
func TestReadsFollowExistingWorkBranch(t *testing.T) {
	x, w := writeExec()
	w.commits["c9"] = map[string]string{"src/pkg/a.go": "package a\n\nfunc Rebase() { recreate() }\n"}
	w.refs["hive-lite/issue-7"] = "c9"
	out, err := run(t, x, call("1", ToolRepoRead, `{"path":"src/pkg/a.go"}`))
	if err != nil || !strings.Contains(out, "recreate()") || !strings.Contains(out, "hive-lite/issue-7@c9") {
		t.Fatalf("read = %q, %v", out, err)
	}
	if _, err := run(t, x, call("2", ToolRepoPatch, `{"commit_message":"m","patches":[{"path":"src/pkg/a.go","search":"recreate() }","replace":"recreate(); log() }"}]}`)); err != nil {
		t.Fatalf("patch on branch head: %v", err)
	}
	// A read-only session never looks at the work branch.
	x2, w2 := writeExec()
	x2.Mode = 0
	w2.commits["c9"] = w.commits["c9"]
	w2.refs["hive-lite/issue-7"] = "c9"
	if out, _ := run(t, x2, call("1", ToolRepoRead, `{"path":"src/pkg/a.go"}`)); strings.Contains(out, "recreate") {
		t.Fatal("advisory session read the work branch")
	}
}

func TestPRBodyNeutralisesClosingKeywordsAndRecordsVerification(t *testing.T) {
	x, w := writeExec()
	x.Verifier = &stubVerifier{ok: true}
	if _, err := run(t, x, call("1", ToolRepoPatch, `{"commit_message":"m","patches":[{"path":"src/pkg/a.go","search":"func Rebase() {}","replace":"func Rebase() { r() }"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, x, call("2", ToolPRCreate, `{"title":"t","body":"Fixes #1. Also closes: o/r#2 and resolved https://github.com/o/r/issues/3"}`)); err != nil {
		t.Fatal(err)
	}
	body := w.prs[0]
	for _, bad := range []string{"Fixes #1", "closes: o/r#2", "resolved https"} {
		if strings.Contains(body, bad) {
			t.Errorf("closing keyword survived: %q in\n%s", bad, body)
		}
	}
	if !strings.Contains(body, "Refs #1") || !strings.Contains(body, "Hive verification") || !strings.Contains(body, "`c1`: passed") {
		t.Fatalf("body = %s", body)
	}
	// Without a verifier the PR says so.
	x2, w2 := writeExec()
	_, _ = run(t, x2, call("1", ToolRepoPatch, `{"commit_message":"m","patches":[{"path":"src/pkg/a.go","search":"func Rebase() {}","replace":"func Rebase() { r() }"}]}`))
	_, _ = run(t, x2, call("2", ToolPRCreate, `{"title":"t","body":"tests pass"}`))
	if !strings.Contains(w2.prs[0], "No build or test verification ran") {
		t.Fatalf("unverified body = %s", w2.prs[0])
	}
}
