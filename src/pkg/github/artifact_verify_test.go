package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// artifactServer serves fixed bodies by exact request path; unknown paths 404.
func artifactServer(t *testing.T, routes map[string]struct {
	status int
	body   string
}) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rt, ok := routes[strings.ToLower(r.URL.Path)]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if rt.status != 0 && rt.status != http.StatusOK {
			http.Error(w, `{"message":"x"}`, rt.status)
			return
		}
		_, _ = io.WriteString(w, rt.body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

type route = struct {
	status int
	body   string
}

func artClient(ts *httptest.Server) *Client {
	return NewClientForTest(ts.URL, "tuna-os", []string{"hive"}, verifyTestLogger())
}

const issueBody = `{"number":5,"state":"open","title":"plan","user":{"login":"alice"},
 "repository_url":"https://api.github.com/repos/tuna-os/hive"}`

func TestParseArtifactURL(t *testing.T) {
	if _, err := ParseArtifactURL(ArtifactIssue, "https://github.com/o/r/pull/9"); err == nil {
		t.Fatal("PR URL must not parse as issue")
	}
	if _, err := ParseArtifactURL(ArtifactRelease, "https://github.com/o/r/issues/9"); err == nil {
		t.Fatal("issue URL must not parse as release")
	}
	if _, err := ParseArtifactURL(ArtifactIssue, "see https://github.com/o/r/issues/9 ok"); err == nil {
		t.Fatal("prose containing a URL is not proof")
	}
	r, err := ParseArtifactURL(ArtifactIssue, " https://github.com/o/r/issues/9#c1 ")
	if err != nil || r.Number != 9 || r.Owner != "o" || r.Repo != "r" {
		t.Fatalf("got %+v %v", r, err)
	}
	r, err = ParseArtifactURL(ArtifactRelease, "https://github.com/o/r/releases/tag/v1.0.0")
	if err != nil || r.Tag != "v1.0.0" {
		t.Fatalf("got %+v %v", r, err)
	}
}

func TestVerifyArtifact_IssueVerifiedAndSatisfies(t *testing.T) {
	ts := artifactServer(t, map[string]route{"/repos/tuna-os/hive/issues/5": {body: issueBody}})
	res := artClient(ts).VerifyReportedArtifact(context.Background(), ArtifactIssue, "tuna-os/hive", "https://github.com/tuna-os/hive/issues/5", "Alice")
	if !res.Verified() || res.State != StateOpen || res.Key != "issue:tuna-os/hive#5" {
		t.Fatalf("unexpected: %+v", res)
	}
	if !res.Satisfies(ArtifactAcceptance{Kind: ArtifactIssue, State: StateOpen}) ||
		!res.Satisfies(ArtifactAcceptance{Kind: ArtifactIssue}) {
		t.Fatal("should satisfy open/any issue acceptance")
	}
	if res.Satisfies(ArtifactAcceptance{Kind: ArtifactIssue, State: StateClosed}) ||
		res.Satisfies(ArtifactAcceptance{Kind: ArtifactRelease}) ||
		res.Satisfies(ArtifactAcceptance{}) {
		t.Fatal("must not satisfy a different state/kind/empty acceptance")
	}
}

func TestVerifyArtifact_IssueURLThatIsAPullRequest(t *testing.T) {
	body := `{"number":5,"state":"open","user":{"login":"alice"},"pull_request":{"url":"x"},
	 "repository_url":"https://api.github.com/repos/tuna-os/hive"}`
	ts := artifactServer(t, map[string]route{"/repos/tuna-os/hive/issues/5": {body: body}})
	res := artClient(ts).VerifyReportedArtifact(context.Background(), ArtifactIssue, "tuna-os/hive", "https://github.com/tuna-os/hive/issues/5", "alice")
	if res.Outcome != ArtifactRejected || !strings.Contains(res.Reason, "pull request") {
		t.Fatalf("got %+v", res)
	}
}

func TestVerifyArtifact_ForeignRepoAndAuthorRejected(t *testing.T) {
	foreign := `{"number":5,"state":"open","user":{"login":"alice"},
	 "repository_url":"https://api.github.com/repos/other/hive"}`
	ts := artifactServer(t, map[string]route{"/repos/other/hive/issues/5": {body: foreign}, "/repos/tuna-os/hive/issues/5": {body: issueBody}})
	c := artClient(ts)
	res := c.VerifyReportedArtifact(context.Background(), ArtifactIssue, "tuna-os/hive", "https://github.com/other/hive/issues/5", "alice")
	if res.Outcome != ArtifactRejected {
		t.Fatalf("foreign repo: %+v", res)
	}
	res = c.VerifyReportedArtifact(context.Background(), ArtifactIssue, "tuna-os/hive", "https://github.com/tuna-os/hive/issues/5", "bob")
	if res.Outcome != ArtifactRejected {
		t.Fatalf("wrong author: %+v", res)
	}
}

func TestVerifyArtifact_UnavailableIsUnverifiedNotRejected(t *testing.T) {
	for _, st := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadGateway} {
		ts := artifactServer(t, map[string]route{"/repos/tuna-os/hive/issues/5": {status: st}})
		res := artClient(ts).VerifyReportedArtifact(context.Background(), ArtifactIssue, "tuna-os/hive", "https://github.com/tuna-os/hive/issues/5", "alice")
		if res.Outcome != ArtifactUnverified || res.Err == nil || res.Satisfies(ArtifactAcceptance{Kind: ArtifactIssue}) {
			t.Fatalf("status %d: %+v", st, res)
		}
	}
	var nilClient *Client
	if res := nilClient.VerifyReportedArtifact(context.Background(), ArtifactIssue, "r", "u", "a"); res.Outcome != ArtifactUnverified {
		t.Fatalf("nil client: %+v", res)
	}
	ts := artifactServer(t, nil)
	res := artClient(ts).VerifyReportedArtifact(context.Background(), ArtifactIssue, "tuna-os/hive", "https://github.com/tuna-os/hive/issues/5", "alice")
	if res.Outcome != ArtifactRejected {
		t.Fatalf("404: %+v", res)
	}
}

func TestVerifyArtifact_RenamedRepoAndDuplicateKey(t *testing.T) {
	// The reported URL uses the old name; GitHub reports the canonical new one.
	body := `{"number":5,"state":"closed","user":{"login":"alice"},
	 "repository_url":"https://api.github.com/repos/tuna-os/hive"}`
	ts := artifactServer(t, map[string]route{
		"/repos/tuna-os/old-name/issues/5": {body: body},
		"/repos/tuna-os/hive/issues/5":     {body: body},
	})
	c := artClient(ts)
	a := c.VerifyReportedArtifact(context.Background(), ArtifactIssue, "tuna-os/hive", "https://github.com/tuna-os/old-name/issues/5", "alice")
	b := c.VerifyReportedArtifact(context.Background(), ArtifactIssue, "tuna-os/hive", "https://github.com/TUNA-OS/hive/issues/5", "alice")
	if !a.Verified() || !b.Verified() || a.Key != b.Key || a.Key == "" {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
}

func TestVerifyArtifact_ReleaseDraftVsPublished(t *testing.T) {
	pub := `{"id":11,"tag_name":"v1","draft":false,"name":"v1","author":{"login":"alice"},
	 "html_url":"https://github.com/tuna-os/hive/releases/tag/v1"}`
	draft := `[{"id":12,"tag_name":"v2","draft":true,"name":"v2","author":{"login":"alice"},
	 "html_url":"https://github.com/tuna-os/hive/releases/tag/untagged-abc"}]`
	ts := artifactServer(t, map[string]route{
		"/repos/tuna-os/hive/releases/tags/v1": {body: pub},
		"/repos/tuna-os/hive/releases":         {body: draft},
	})
	c := artClient(ts)
	p := c.VerifyReportedArtifact(context.Background(), ArtifactRelease, "tuna-os/hive", "https://github.com/tuna-os/hive/releases/tag/v1", "alice")
	if !p.Verified() || p.State != StatePublished {
		t.Fatalf("published: %+v", p)
	}
	d := c.VerifyReportedArtifact(context.Background(), ArtifactRelease, "tuna-os/hive", "https://github.com/tuna-os/hive/releases/tag/untagged-abc", "alice")
	if !d.Verified() || d.State != StateDraft {
		t.Fatalf("draft: %+v", d)
	}
	if d.Satisfies(ArtifactAcceptance{Kind: ArtifactRelease, State: StatePublished}) ||
		!p.Satisfies(ArtifactAcceptance{Kind: ArtifactRelease, State: StatePublished}) {
		t.Fatal("draft must not satisfy published acceptance")
	}
	if p.Satisfies(ArtifactAcceptance{Kind: ArtifactIssue}) {
		t.Fatal("release must not satisfy issue acceptance")
	}
}
