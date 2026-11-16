package hivemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/pkg/coordstate"
	"github.com/hivecommons/hive/pkg/hivetool"
)

type reader struct{}

func (reader) Issue(_ context.Context, _ string, n int) (hivetool.Issue, error) {
	if n == 404 {
		return hivetool.Issue{}, errors.New("not found")
	}
	return hivetool.Issue{Number: n, Title: "stuck", Body: "PageCount drops the last page.", State: "open"}, nil
}
func (reader) List(context.Context, string, string, string) ([]hivetool.Entry, error) {
	return nil, nil
}
func (reader) File(_ context.Context, _, _, p string) ([]byte, error) {
	if p == "src/a.go" {
		return []byte("package a\n\nfunc PageCount() {}\n"), nil
	}
	return nil, errors.New("not found")
}
func (reader) Checks(context.Context, string, string) ([]hivetool.CheckRun, error) { return nil, nil }

type tasks struct{ issues []int }

func (t tasks) Next(_ context.Context, _ Principal, skip []int) (Task, bool, error) {
	for _, n := range t.issues {
		ok := true
		for _, s := range skip {
			if s == n {
				ok = false
			}
		}
		if ok {
			return Task{Issue: n, Title: fmt.Sprintf("issue %d", n)}, true, nil
		}
	}
	return Task{}, false, nil
}

type nopWriter struct{}

func (nopWriter) RefSHA(context.Context, string, string) (string, error) {
	return "", hivetool.ErrRefNotFound
}
func (nopWriter) CreateCommit(context.Context, string, string, string, map[string]string) (string, error) {
	return "c", nil
}
func (nopWriter) SetBranch(context.Context, string, string, string, bool) error { return nil }
func (nopWriter) OpenPR(context.Context, string, string, string, string, string) (int, string, error) {
	return 1, "u", nil
}

var principals = map[string]Principal{
	"tok-alice": {Login: "alice", Mode: agentmode.ModeAdvisory},
	"tok-bob":   {Login: "bob", Mode: agentmode.ModeIssuesPRsMerge},
}

func bearer(r *http.Request) (Principal, bool) {
	p, ok := principals[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	return p, ok
}

type harness struct {
	t     *testing.T
	srv   *httptest.Server
	now   time.Time
	store *coordstate.MemoryStore
	out   []hivetool.Outcome
}

func newHarness(t *testing.T, mut func(*Config)) *harness {
	h := &harness{t: t, now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC), store: coordstate.NewMemoryStore()}
	cfg := Config{Repo: "o/r", Ref: "main", Reader: reader{}, Tasks: tasks{issues: []int{7, 8}}, Auth: bearer, Store: h.store,
		Now:       func() time.Time { return h.now },
		OnOutcome: func(_ context.Context, _ Principal, _ Task, o hivetool.Outcome) { h.out = append(h.out, o) },
	}
	if mut != nil {
		mut(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = httptest.NewServer(s)
	t.Cleanup(h.srv.Close)
	return h
}

type reply struct {
	status int
	sid    string
	body   map[string]any
}

func (h *harness) post(tok, sid, body string) reply {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if sid != "" {
		req.Header.Set(SessionHeader, sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode, sid: resp.Header.Get(SessionHeader), body: map[string]any{}}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

func (h *harness) open(tok string) string {
	h.t.Helper()
	r := h.post(tok, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if r.status != 200 || r.sid == "" {
		h.t.Fatalf("initialize = %d %v", r.status, r.body)
	}
	return r.sid
}

// call returns the tool result text and isError.
func (h *harness) call(tok, sid, name, args string) (string, bool) {
	h.t.Helper()
	r := h.post(tok, sid, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args))
	res, ok := r.body["result"].(map[string]any)
	if !ok {
		h.t.Fatalf("%s: no result: %d %v", name, r.status, r.body)
	}
	content := res["content"].([]any)[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return content, isErr
}

func (h *harness) tools(tok, sid string) []string {
	r := h.post(tok, sid, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	var names []string
	for _, t := range r.body["result"].(map[string]any)["tools"].([]any) {
		names = append(names, t.(map[string]any)["name"].(string))
	}
	return names
}

func TestProtocolEdges(t *testing.T) {
	h := newHarness(t, nil)
	if r := h.post("", "", `{}`); r.status != 401 {
		t.Fatalf("no auth = %d", r.status)
	}
	resp, _ := http.Get(h.srv.URL)
	if resp.StatusCode != 405 {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
	cases := map[string]int{
		`[{"jsonrpc":"2.0"}]`:            400,
		`{nope`:                          400,
		`{"jsonrpc":"1.0","method":"x"}`: 400,
		`{"jsonrpc":"2.0","method":"tools/list"}`: 400, // no session
	}
	for body, want := range cases {
		if r := h.post("tok-alice", "", body); r.status != want {
			t.Errorf("%s = %d, want %d", body, r.status, want)
		}
	}
	if r := h.post("tok-alice", "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); r.status != 202 {
		t.Fatalf("notification = %d", r.status)
	}
	if r := h.post("tok-alice", "", `{"jsonrpc":"2.0","id":9,"method":"ping"}`); r.status != 200 || r.body["result"] == nil {
		t.Fatalf("ping = %v", r.body)
	}
	if r := h.post("tok-alice", "", `{"jsonrpc":"2.0","id":1,"method":"x","params":`+strings.Repeat(" ", maxBodyBytes)+`{}}`); r.status != 413 {
		t.Fatalf("oversize = %d", r.status)
	}
	sid := h.open("tok-alice")
	if r := h.post("tok-alice", sid, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`); r.body["error"] == nil {
		t.Fatal("unknown method accepted")
	}
	if r := h.post("tok-alice", sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`); r.body["error"] == nil {
		t.Fatal("nameless call accepted")
	}
	if r := h.post("tok-alice", "feedface", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); r.status != 404 {
		t.Fatalf("unknown session = %d", r.status)
	}
	if r := h.post("tok-alice", "../x", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hive_next_task"}}`); r.status != 404 {
		t.Fatalf("bad session id = %d", r.status)
	}
	if r := h.post("tok-bob", sid, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); r.status != 403 {
		t.Fatalf("foreign session = %d", r.status)
	}
	if r := h.post("tok-alice", sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"repo_patch_files","arguments":{}}}`); r.body["error"] == nil {
		t.Fatal("write tool callable at ADVISORY")
	}
}

func TestToolListFollowsClampedMode(t *testing.T) {
	ro := newHarness(t, nil)
	if got := strings.Join(ro.tools("tok-bob", ro.open("tok-bob")), ","); strings.Contains(got, "repo_patch_files") {
		t.Fatalf("no Writer, yet write tools offered: %s", got)
	}
	rw := newHarness(t, func(c *Config) { c.Writer = nopWriter{} })
	bob := strings.Join(rw.tools("tok-bob", rw.open("tok-bob")), ",")
	if !strings.Contains(bob, "repo_patch_files") || !strings.Contains(bob, "pr_create") || strings.Contains(bob, "merge") {
		t.Fatalf("bob (merge tier, clamped to ISSUES_AND_PRS) = %s", bob)
	}
	alice := strings.Join(rw.tools("tok-alice", rw.open("tok-alice")), ",")
	if strings.Contains(alice, "repo_patch_files") || !strings.HasPrefix(alice, ToolNextTask) || !strings.Contains(alice, "repo_read_file") {
		t.Fatalf("alice = %s", alice)
	}
}

func TestTaskFlowEvidenceAndOutcome(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.open("tok-alice")
	if out, isErr := h.call("tok-alice", sid, "repo_read_file", `{"path":"src/a.go"}`); !isErr || !strings.Contains(out, "no task in progress") {
		t.Fatalf("read before task = %q", out)
	}
	if out, isErr := h.call("tok-alice", sid, ToolDeclineTask, `{}`); !isErr || !strings.Contains(out, "nothing is on offer") {
		t.Fatal(out)
	}
	out, _ := h.call("tok-alice", sid, ToolNextTask, `{}`)
	if !strings.Contains(out, `"issue": 7`) || strings.Contains(out, "PageCount drops") {
		t.Fatalf("offer leaked the body or wrong issue: %q", out)
	}
	if out, _ := h.call("tok-alice", sid, ToolDeclineTask, `{}`); !strings.Contains(out, "Declined") {
		t.Fatal(out)
	}
	if out, _ := h.call("tok-alice", sid, ToolNextTask, `{}`); !strings.Contains(out, `"issue": 8`) {
		t.Fatalf("declined issue offered again: %q", out)
	}
	if out, isErr := h.call("tok-alice", sid, ToolAcceptTask, `{"issue":7}`); !isErr || !strings.Contains(out, "only the issue") {
		t.Fatal(out)
	}
	brief, isErr := h.call("tok-alice", sid, ToolAcceptTask, `{"issue":8}`)
	if isErr || !strings.Contains(brief, "PageCount drops the last page.") || !strings.Contains(brief, "untrusted data") {
		t.Fatalf("brief = %q", brief)
	}
	if out, isErr := h.call("tok-alice", sid, ToolNextTask, `{}`); !isErr || !strings.Contains(out, "in progress") {
		t.Fatal(out)
	}
	if out, isErr := h.call("tok-alice", sid, "task_complete", `{"summary":"bug at src/a.go:3"}`); !isErr || !strings.Contains(out, "refused") {
		t.Fatalf("ungrounded completion accepted: %q", out)
	}
	if _, isErr := h.call("tok-alice", sid, "repo_read_file", `{"path":"src/a.go"}`); isErr {
		t.Fatal("read failed")
	}
	if out, isErr := h.call("tok-alice", sid, "repo_read_file", `{"path":"src/a.go"}`); !isErr || !strings.Contains(out, "duplicate") {
		t.Fatalf("duplicate not refused across requests: %q", out)
	}
	if out, isErr := h.call("tok-alice", sid, "task_complete", `{"summary":"bug at src/a.go:3"}`); isErr || !strings.Contains(out, "1 of 1 citations verified") || !strings.Contains(out, "finished") {
		t.Fatalf("grounded completion = %q", out)
	}
	if len(h.out) != 1 || h.out[0].Tool != hivetool.ToolTaskComplete {
		t.Fatalf("OnOutcome = %+v", h.out)
	}
	if out, _ := h.call("tok-alice", sid, ToolNextTask, `{}`); !strings.Contains(out, "No task") {
		t.Fatalf("finished/declined issues offered again: %q", out)
	}
}

func TestClaimsAcrossContributors(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Tasks = tasks{issues: []int{7}}; c.ClaimTTL = time.Minute })
	a := h.open("tok-alice")
	b := h.open("tok-bob")
	h.call("tok-alice", a, ToolNextTask, `{}`)
	h.call("tok-bob", b, ToolNextTask, `{}`) // both see #7 on offer
	if _, isErr := h.call("tok-alice", a, ToolAcceptTask, `{"issue":7}`); isErr {
		t.Fatal("alice accept")
	}
	if out, isErr := h.call("tok-bob", b, ToolAcceptTask, `{"issue":7}`); !isErr || !strings.Contains(out, "claimed") {
		t.Fatalf("bob took a claimed issue: %q", out)
	}
	if out, _ := h.call("tok-bob", b, ToolNextTask, `{}`); !strings.Contains(out, "No task") {
		t.Fatalf("claimed issue offered: %q", out)
	}
	// Alice goes quiet; the lease lapses and Bob takes over.
	h.now = h.now.Add(2 * time.Minute)
	h.call("tok-bob", b, ToolNextTask, `{}`)
	if _, isErr := h.call("tok-bob", b, ToolAcceptTask, `{"issue":7}`); isErr {
		t.Fatal("bob could not take a lapsed claim")
	}
	if out, isErr := h.call("tok-alice", a, "repo_read_file", `{"path":"src/a.go"}`); !isErr || !strings.Contains(out, "lapsed") {
		t.Fatalf("alice kept working a lost claim: %q", out)
	}
	h.call("tok-bob", b, "repo_read_file", `{"path":"src/a.go"}`)
	if _, isErr := h.call("tok-bob", b, "task_abort", `{"reason":"needs maintainer input"}`); isErr {
		t.Fatal("abort")
	}
	// Done: never offered again, even after the TTL.
	h.now = h.now.Add(time.Hour)
	c := h.open("tok-alice")
	if out, _ := h.call("tok-alice", c, ToolNextTask, `{}`); !strings.Contains(out, "No task") {
		t.Fatalf("finished issue offered: %q", out)
	}
}

func TestRateLimitAndMissingIssue(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.CallsPerMinute = 2; c.Tasks = tasks{issues: []int{404}} })
	sid := h.open("tok-alice")
	h.call("tok-alice", sid, ToolNextTask, `{}`)
	if out, isErr := h.call("tok-alice", sid, ToolAcceptTask, `{"issue":404}`); !isErr || !strings.Contains(out, "could not read") {
		t.Fatal(out)
	}
	if out, isErr := h.call("tok-alice", sid, ToolNextTask, `{}`); !isErr || !strings.Contains(out, "rate limit") {
		t.Fatalf("third call = %q", out)
	}
	h.now = h.now.Add(61 * time.Second)
	if _, isErr := h.call("tok-alice", sid, ToolNextTask, `{}`); isErr {
		t.Fatal("budget did not refill")
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty config")
	}
}

func TestTokenAuth(t *testing.T) {
	auth, err := TokenAuth([]TokenEntry{{SHA256: HashToken("s3cret"), Login: "carol", Mode: "ISSUES_AND_PRS"}, {SHA256: HashToken("ro"), Login: "dan"}})
	if err != nil {
		t.Fatal(err)
	}
	for tok, want := range map[string]Principal{"s3cret": {"carol", agentmode.ModeIssuesAndPRs}, "ro": {"dan", agentmode.ModeAdvisory}} {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		if p, ok := auth(r); !ok || p != want {
			t.Errorf("%s = %+v %v", tok, p, ok)
		}
	}
	for _, h := range []string{"", "Bearer ", "Bearer nope", "Basic s3cret"} {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("Authorization", h)
		if _, ok := auth(r); ok {
			t.Errorf("%q authenticated", h)
		}
	}
	for _, bad := range [][]TokenEntry{{{SHA256: "zz", Login: "x"}}, {{SHA256: HashToken("a")}}, {{SHA256: HashToken("a"), Login: "x", Mode: "GOD"}}} {
		if _, err := TokenAuth(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestLabelTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("labels") != "hive-lite" || r.Header.Get("Authorization") != "Bearer gh" {
			w.WriteHeader(403)
			return
		}
		io.WriteString(w, `[{"number":3,"title":"pr","pull_request":{}},{"number":4,"title":"done"},{"number":5,"title":"open","labels":[{"name":"hive-lite"}]}]`)
	}))
	defer srv.Close()
	l := &LabelTasks{Repo: "o/r", Label: "hive-lite", BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "gh", nil }}
	got, ok, err := l.Next(context.Background(), Principal{}, []int{4})
	if err != nil || !ok || got.Issue != 5 || len(got.Labels) != 1 {
		t.Fatalf("Next = %+v %v %v", got, ok, err)
	}
	if _, ok, _ := l.Next(context.Background(), Principal{}, []int{4, 5}); ok {
		t.Fatal("everything skipped, yet offered")
	}
	l.Label = "other"
	if _, _, err := l.Next(context.Background(), Principal{}, nil); err == nil {
		t.Fatal("403 swallowed")
	}
	l.Token = func(context.Context) (string, error) { return "", errors.New("no token") }
	if _, _, err := l.Next(context.Background(), Principal{}, nil); err == nil {
		t.Fatal("token error swallowed")
	}
	if _, _, err := (&LabelTasks{Repo: "o/r", BaseURL: "http://127.0.0.1:1"}).Next(context.Background(), Principal{}, nil); err == nil {
		t.Fatal("transport error swallowed")
	}
}

func TestBranchSafeAndNames(t *testing.T) {
	if branchSafe("Some_User[bot]") != "some-user-bot" {
		t.Fatal(branchSafe("Some_User[bot]"))
	}
	if mcpName("repo.read_file") != "repo_read_file" {
		t.Fatal("mcpName")
	}
	if _, ok := hiveToolName("repo_patch_files", agentmode.ModeAdvisory); ok {
		t.Fatal("write tool resolved at ADVISORY")
	}
}
