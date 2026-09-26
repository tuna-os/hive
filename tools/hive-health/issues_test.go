package main

import (
	"strings"
	"testing"
)

func failingSchool(t *testing.T) HiveReport {
	return Evaluate(authedRaw(t, "school"), nil, DefaultThresholds(), fixtureNow)
}

func TestPlanCreatesIssueForFailingHiveOnly(t *testing.T) {
	school := failingSchool(t)
	reilly := Evaluate(publicRaw(t, "reilly"), nil, DefaultThresholds(), fixtureNow)
	r := Report{GeneratedAt: fixtureNow, Hives: []HiveReport{school, reilly}}
	acts := PlanIssues(r, nil, "https://github.com/tuna-os/hive/actions/runs/1")
	if len(acts) != 1 || acts[0].Op != "create" || acts[0].Target != "school" {
		t.Fatalf("actions = %+v", acts)
	}
	a := acts[0]
	if !strings.HasPrefix(a.Title, "[operations] ") {
		t.Fatalf("title must carry the [operations] lane prefix: %q", a.Title)
	}
	if strings.Join(a.Labels, ",") != "hive-health,agent/operations" {
		t.Fatalf("labels = %v", a.Labels)
	}
	for _, bad := range []string{"hold", "ai-fix-requested"} {
		for _, l := range a.Labels {
			if l == bad {
				t.Fatalf("label %q must never be applied automatically", bad)
			}
		}
	}
	if !strings.Contains(a.Body, IssueMarker("school")) || !strings.Contains(a.Body, fingerprintMarker(school.Fingerprint)) {
		t.Fatal("body lacks markers")
	}
	for _, want := range []string{"agent/reviewer/needs-login", "For the Hive agent picking this up", "Diagnose, don't operate", "needs a human", "actions/runs/1"} {
		if !strings.Contains(a.Body, want) {
			t.Fatalf("body lacks %q", want)
		}
	}
}

func TestPlanUpdatesWithoutCommentWhenUnchanged(t *testing.T) {
	school := failingSchool(t)
	r := Report{GeneratedAt: fixtureNow, Hives: []HiveReport{school}}
	open := []OpenIssue{{Number: 42, Body: IssueBody(school, fixtureNow, "")}}
	acts := PlanIssues(r, open, "")
	if len(acts) != 1 || acts[0].Op != "update" || acts[0].Number != 42 || acts[0].Comment != "" {
		t.Fatalf("actions = %+v", acts)
	}
}

func TestPlanCommentsWhenFailingSetChanges(t *testing.T) {
	school := failingSchool(t)
	r := Report{GeneratedAt: fixtureNow, Hives: []HiveReport{school}}
	old := IssueMarker("school") + "\n" + fingerprintMarker("0123456789ab") + "\n| `reachability` |"
	acts := PlanIssues(r, []OpenIssue{{Number: 7, Body: old}}, "")
	if len(acts) != 1 || acts[0].Op != "update" || acts[0].Comment == "" {
		t.Fatalf("actions = %+v", acts)
	}
	if !strings.Contains(acts[0].Comment, "`agent/reviewer/needs-login` (new)") {
		t.Fatalf("comment = %s", acts[0].Comment)
	}
}

func TestPlanClosesOnRecoveryAndDedups(t *testing.T) {
	school := failingSchool(t)
	reef := Evaluate(publicRaw(t, "reef"), nil, DefaultThresholds(), fixtureNow) // passes
	r := Report{GeneratedAt: fixtureNow, Hives: []HiveReport{school, reef}}
	open := []OpenIssue{
		{Number: 9, Body: IssueMarker("school")},
		{Number: 5, Body: IssueMarker("school")},
		{Number: 6, Body: IssueMarker("reef")},
		{Number: 8, Body: "unrelated issue that happens to carry the label"},
		{Number: 10, Body: IssueMarker("retired-hive")},
	}
	acts := PlanIssues(r, open, "")
	got := map[int]string{}
	for _, a := range acts {
		got[a.Number] = a.Op
	}
	if got[5] != "update" || got[9] != "close" || got[6] != "close" {
		t.Fatalf("ops = %v", got)
	}
	if _, touched := got[8]; touched {
		t.Fatal("touched an issue without a marker")
	}
	if _, touched := got[10]; touched {
		t.Fatal("touched an issue for a Hive not in this run (e.g. -only)")
	}
	for _, a := range acts {
		if a.Number == 6 && !strings.Contains(a.Comment, "no longer failing") {
			t.Fatalf("recovery comment = %q", a.Comment)
		}
	}
}

func TestWarningsNeverOpenAnIssue(t *testing.T) {
	h := HiveReport{Name: "x", URL: "https://x", Checks: []Check{{ID: "hub-heartbeat", Level: Warn}}}
	h.finalize()
	if acts := PlanIssues(Report{Hives: []HiveReport{h}}, nil, ""); len(acts) != 0 {
		t.Fatalf("actions = %+v", acts)
	}
}

func TestIssueBodyIsInert(t *testing.T) {
	h := HiveReport{Name: "x", URL: "https://x.example", Level: Fail, Checks: []Check{
		{ID: "agent/a/blocked", Kind: "blocked", Level: Fail, Summary: "ping @hanthor see #12 | <script>"},
	}, RecentOutput: []OutputItem{{Agent: "a", Source: "bead/bug", Text: "@someone broke it"}}}
	h.finalize()
	body := IssueBody(h, fixtureNow, "")
	if strings.Contains(body, "@hanthor") || strings.Contains(body, "@someone") {
		t.Fatal("body would ping users")
	}
	if strings.Contains(body, "<script>") {
		t.Fatal("body carries raw HTML")
	}
}

func TestAdviceCoversEveryFailKind(t *testing.T) {
	for _, k := range []string{"reachability", "deep-health", "ready", "github-auth", "governor", "governor-stalled",
		"needs-login", "crash-loop", "blocked", "agent-down", "kick-stale", "not-producing", "system-alert"} {
		a := adviceFor(k, HiveReport{Name: "school", URL: "https://school.tunaos.org", Namespace: "hive"})
		if len(a.Diagnose) == 0 {
			t.Errorf("no diagnosis advice for fail kind %q", k)
		}
	}
}
