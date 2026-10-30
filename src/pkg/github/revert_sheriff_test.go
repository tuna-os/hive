package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/redmain"
)

// sheriffServer serves acme/widget: main is c3 -> c2 -> c1, "unit" red on c3
// and c2 and green on c1, and c2 is the merge commit of PR #42. culpritRun is
// the latest "unit" run on c2; tests swap it to simulate the re-run.
type sheriffServer struct {
	mu         sync.Mutex
	culpritRun map[string]any
	reruns     []string
	graphql    []map[string]any
	labels     [][]string
	comments   []string
	prFiles    []string
}

func (s *sheriffServer) handler(t *testing.T) http.Handler {
	run := func(id int, conclusion, started string) map[string]any {
		return map[string]any{"id": id, "name": "unit", "status": "completed", "conclusion": conclusion,
			"started_at": started, "completed_at": started, "html_url": "https://github.test/runs/" + conclusion,
			"app": map[string]any{"slug": "github-actions"}}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sha") != "main" {
			t.Errorf("commits listed from %q, want main", r.URL.Query().Get("sha"))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"sha": "c3", "parents": []map[string]any{{"sha": "c2"}}},
			// A commit from a merged branch: off the first-parent chain.
			{"sha": "side", "parents": []map[string]any{{"sha": "c1"}}},
			{"sha": "c2", "parents": []map[string]any{{"sha": "c1"}, {"sha": "side"}}},
			{"sha": "c1", "parents": []map[string]any{{"sha": "c0"}}},
		})
	})
	mux.HandleFunc("/repos/acme/widget/commits/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/acme/widget/commits/"), "/")
		sha, what := parts[0], parts[1]
		if what == "pulls" {
			if sha != "c2" {
				t.Errorf("PR looked up for %s, want the culprit c2", sha)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 42, "title": "feat: break unit", "node_id": "PR_42",
				"merged_at": "2026-10-30T10:00:00Z", "merge_commit_sha": "c2"}})
			return
		}
		if got := r.URL.Query().Get("check_name"); got != "unit" {
			t.Errorf("check runs filtered by %q, want unit", got)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		var runs []map[string]any
		switch sha {
		case "c3":
			runs = []map[string]any{run(33, "failure", "2026-10-30T11:00:00Z")}
		case "c2":
			runs = []map[string]any{s.culpritRun}
		case "c1":
			runs = []map[string]any{run(11, "success", "2026-10-30T09:00:00Z")}
		case "side":
			t.Error("walked a commit off the first-parent chain")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
	})
	mux.HandleFunc("/repos/acme/widget/actions/jobs/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reruns = append(s.reruns, r.URL.Path)
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/repos/acme/widget/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "title": "feat: break unit", "node_id": "PR_42",
			"changed_files": len(s.prFiles), "labels": []map[string]any{}})
	})
	mux.HandleFunc("/repos/acme/widget/pulls/42/files", func(w http.ResponseWriter, r *http.Request) {
		var files []map[string]any
		for _, f := range s.prFiles {
			files = append(files, map[string]any{"filename": f, "status": "modified"})
		}
		_ = json.NewEncoder(w).Encode(files)
	})
	mux.HandleFunc("/repos/acme/widget/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("labels") != "hive/revert" {
			t.Errorf("open-revert lookup labels = %q", r.URL.Query().Get("labels"))
		}
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.graphql = append(s.graphql, req)
		s.mu.Unlock()
		_, _ = w.Write([]byte(`{"data":{"revertPullRequest":{"revertPullRequest":{"number":77,"url":"https://github.test/acme/widget/pull/77"}}}}`))
	})
	mux.HandleFunc("/repos/acme/widget/issues/77/labels", func(w http.ResponseWriter, r *http.Request) {
		var labels []string
		_ = json.NewDecoder(r.Body).Decode(&labels)
		s.mu.Lock()
		s.labels = append(s.labels, labels)
		s.mu.Unlock()
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("/repos/acme/widget/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.comments = append(s.comments, string(body))
		s.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	mux.HandleFunc("/repos/acme/widget/labels/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"hive/revert"}`))
	})
	s.culpritRun = run(22, "failure", "2026-10-30T10:05:00Z")
	return mux
}

func (s *sheriffServer) setCulpritRun(id int, conclusion, started string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.culpritRun = map[string]any{"id": id, "name": "unit", "status": "completed", "conclusion": conclusion,
		"started_at": started, "completed_at": started, "html_url": "https://github.test/runs/rerun",
		"app": map[string]any{"slug": "github-actions"}}
}

func sheriffSetup(t *testing.T, files ...string) (*Client, *sheriffServer, func()) {
	t.Helper()
	s := &sheriffServer{prFiles: files}
	srv := httptest.NewServer(s.handler(t))
	c := newTestClient(t, srv, "acme", []string{"widget"})
	c.ObserveRedMain("acme/widget", "main", map[string]redmain.RedRun{"unit": {}}, time.Date(2026, 10, 30, 11, 0, 0, 0, time.UTC))
	return c, s, srv.Close
}

func sheriffSweep(t *testing.T, c *Client, mode string, now time.Time) []RevertSheriffEvent {
	t.Helper()
	var events []RevertSheriffEvent
	_, err := c.SweepRevertSheriff(context.Background(), RevertSheriffOptions{
		Watches:        func(repo string) bool { return repo == "acme/widget" },
		Mode:           mode,
		Hold:           true,
		MaxPerDay:      2,
		Lookback:       10,
		ConfirmTimeout: 3 * time.Hour,
		Now:            now,
		Audit:          func(ev RevertSheriffEvent) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatalf("SweepRevertSheriff: %v", err)
	}
	return events
}

func onlyAction(t *testing.T, events []RevertSheriffEvent, want string) RevertSheriffEvent {
	t.Helper()
	if len(events) != 1 || events[0].Action != want {
		t.Fatalf("events = %+v, want one %q", events, want)
	}
	return events[0]
}

// The full propose path: find the merge that broke main, re-run its failing
// job, and only after the re-run fails again open a held revert.
func TestRevertSheriffRevertsAConfirmedBreak(t *testing.T) {
	c, s, done := sheriffSetup(t)
	defer done()
	t0 := time.Date(2026, 10, 30, 11, 10, 0, 0, time.UTC)

	ev := onlyAction(t, sheriffSweep(t, c, config.RevertSheriffPropose, t0), "rerun")
	if ev.PR != 42 || ev.Culprit != "c2" {
		t.Fatalf("culprit = PR #%d @%s, want #42 @c2", ev.PR, ev.Culprit)
	}
	if len(s.reruns) != 1 || s.reruns[0] != "/repos/acme/widget/actions/jobs/22/rerun" {
		t.Fatalf("reruns = %v, want job 22", s.reruns)
	}

	// The re-run has not reported yet: the old failing run is not evidence.
	if events := sheriffSweep(t, c, config.RevertSheriffPropose, t0.Add(10*time.Minute)); len(events) != 0 {
		t.Fatalf("acted before the re-run finished: %+v", events)
	}
	if len(s.graphql) != 0 {
		t.Fatal("revert opened before confirmation")
	}

	s.setCulpritRun(23, "failure", t0.Add(time.Minute).Format(time.RFC3339))
	ev = onlyAction(t, sheriffSweep(t, c, config.RevertSheriffPropose, t0.Add(20*time.Minute)), "reverted")
	if ev.RevertPR != 77 {
		t.Fatalf("revert PR = %d, want 77", ev.RevertPR)
	}
	if len(s.graphql) != 1 {
		t.Fatalf("graphql calls = %d, want 1", len(s.graphql))
	}
	vars := s.graphql[0]["variables"].(map[string]any)
	if vars["pr"] != "PR_42" || !strings.Contains(vars["body"].(string), "<!-- hive:revert-sheriff repo=acme/widget pr=42 sha=c2 -->") {
		t.Fatalf("revert mutation vars = %v", vars)
	}
	if len(s.labels) != 1 || strings.Join(s.labels[0], ",") != "hive/revert,fixes-main,hold" {
		t.Fatalf("labels = %v, want hive/revert,fixes-main,hold", s.labels)
	}
	if len(s.comments) != 1 || !strings.Contains(s.comments[0], "#77") {
		t.Fatalf("culprit comment = %v", s.comments)
	}

	// Done: no second revert while main stays red.
	if events := sheriffSweep(t, c, config.RevertSheriffPropose, t0.Add(30*time.Minute)); len(events) != 0 || len(s.graphql) != 1 {
		t.Fatalf("acted twice: events %+v, graphql %d", events, len(s.graphql))
	}
}

func TestRevertSheriffLeavesAFlakeAlone(t *testing.T) {
	c, s, done := sheriffSetup(t)
	defer done()
	t0 := time.Date(2026, 10, 30, 11, 10, 0, 0, time.UTC)
	onlyAction(t, sheriffSweep(t, c, config.RevertSheriffPropose, t0), "rerun")
	s.setCulpritRun(23, "success", t0.Add(time.Minute).Format(time.RFC3339))
	onlyAction(t, sheriffSweep(t, c, config.RevertSheriffPropose, t0.Add(10*time.Minute)), "flaky")
	if len(s.graphql) != 0 {
		t.Fatal("reverted a flake")
	}
}

func TestRevertSheriffShadowAndProtectedPaths(t *testing.T) {
	c, s, done := sheriffSetup(t)
	defer done()
	t0 := time.Date(2026, 10, 30, 11, 10, 0, 0, time.UTC)
	sheriffSweep(t, c, config.RevertSheriffShadow, t0)
	s.setCulpritRun(23, "failure", t0.Add(time.Minute).Format(time.RFC3339))
	onlyAction(t, sheriffSweep(t, c, config.RevertSheriffShadow, t0.Add(10*time.Minute)), "would-revert")
	if len(s.graphql) != 0 || len(s.labels) != 0 || len(s.comments) != 0 {
		t.Fatal("shadow mode wrote to the forge")
	}

	c2, s2, done2 := sheriffSetup(t, "cosign.pub", "README.md")
	defer done2()
	c2.SetHumanMergePaths(map[string][]string{"acme/widget": {"cosign.pub"}})
	sheriffSweep(t, c2, config.RevertSheriffPropose, t0)
	s2.setCulpritRun(23, "failure", t0.Add(time.Minute).Format(time.RFC3339))
	ev := onlyAction(t, sheriffSweep(t, c2, config.RevertSheriffPropose, t0.Add(10*time.Minute)), "refused")
	if !strings.Contains(ev.Reason, "cosign.pub") || len(s2.graphql) != 0 {
		t.Fatalf("protected-path culprit: %+v, graphql %d", ev, len(s2.graphql))
	}
}

func TestRevertSheriffIgnoresUnwatchedAndGreenRepos(t *testing.T) {
	c, s, done := sheriffSetup(t)
	defer done()
	t0 := time.Date(2026, 10, 30, 11, 10, 0, 0, time.UTC)
	res, err := c.SweepRevertSheriff(context.Background(), RevertSheriffOptions{Watches: func(string) bool { return false }, Now: t0})
	if err != nil || res.Repos != 0 || len(s.reruns) != 0 {
		t.Fatalf("unwatched repo touched: %+v %v", res, err)
	}
	c.ObserveRedMain("acme/widget", "main", nil, t0)
	if events := sheriffSweep(t, c, config.RevertSheriffPropose, t0); len(events) != 0 || len(s.reruns) != 0 {
		t.Fatalf("green main acted on: %+v", events)
	}
}
