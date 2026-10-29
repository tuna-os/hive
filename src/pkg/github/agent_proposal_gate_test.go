package github

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
)

// The agent-proposal gate is the admission-side half of #5117: an
// unacknowledged hive-filed direction proposal must not reach the actionable
// set, while everything the gate is NOT about keeps flowing.

func agentProposalIssues() []wireIssue {
	bot := wireUser{"hive[bot]"}
	return []wireIssue{
		{Number: 1, Title: "[architect] split the build layer", User: bot,
			Labels: []wireLabel{{Name: "agent/architect"}}, CreatedAt: hoursAgo(5)},
		{Number: 2, Title: "[Strategist] adopt a release train", User: bot,
			CreatedAt: hoursAgo(5)}, // proposal by title only, no lane label
		{Number: 3, Title: "[architect] approved split", User: bot,
			Labels: []wireLabel{{Name: "agent/architect"}, {Name: HumanAckLabel}}, CreatedAt: hoursAgo(5)},
		{Number: 4, Title: "[architect] owned by a maintainer", User: bot,
			Labels: []wireLabel{{Name: "agent/architect"}}, Assignees: []wireUser{{"dan"}}, CreatedAt: hoursAgo(5)},
		{Number: 5, Title: "[scanner] nil deref in parser", User: bot,
			Labels: []wireLabel{{Name: "agent/scanner"}, {Name: "bug"}}, CreatedAt: hoursAgo(5)},
		{Number: 6, Title: "[architect] a human wants this", User: wireUser{"alice"},
			Labels: []wireLabel{{Name: "agent/architect"}}, CreatedAt: hoursAgo(5)},
	}
}

func enumerateAgentProposals(t *testing.T, f config.IssueFilterConfig) map[int]bool {
	t.Helper()
	org, repo := "testorg", "testrepo"
	server := httptest.NewServer(buildMux(t, org, repo, agentProposalIssues(), nil))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetIssueFilter(f)
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	return actionableNumbers(result)
}

func TestEnumerateActionable_AgentProposalGate(t *testing.T) {
	nums := enumerateAgentProposals(t, config.IssueFilterConfig{AgentProposal: config.AgentProposalGateConfig{
		Labels:        []string{"agent/architect", "agent/strategist"},
		TitlePrefixes: []string{"[strategist]"},
	}})
	cases := []struct {
		number int
		want   bool
		why    string
	}{
		{1, false, "unacknowledged hive-filed proposal (lane label) must be held"},
		{2, false, "unacknowledged hive-filed proposal (title prefix, case-insensitive) must be held"},
		{3, true, "approved-direction admits the proposal"},
		{4, true, "a human assignee acknowledges the proposal"},
		{5, true, "a hive-filed bug report is not a proposal"},
		{6, true, "a human-filed issue is never held by this gate"},
	}
	for _, tc := range cases {
		if nums[tc.number] != tc.want {
			t.Errorf("issue #%d actionable=%v, want %v: %s", tc.number, nums[tc.number], tc.want, tc.why)
		}
	}
}

func TestEnumerateActionable_AgentProposalGateOffByDefault(t *testing.T) {
	nums := enumerateAgentProposals(t, config.IssueFilterConfig{})
	for n := 1; n <= 6; n++ {
		if !nums[n] {
			t.Errorf("issue #%d held with no agent_proposal configured; the gate must be opt-in", n)
		}
	}
}

// A webhook must not admit a freshly filed proposal ahead of the reconcile
// pass: that single-item path is how a new [architect] issue would otherwise
// reach the queue the moment it is opened.
func TestClassifyWebhookIssue_AgentProposalGate(t *testing.T) {
	org, repo := "testorg", "testrepo"
	server := httptest.NewServer(buildMux(t, org, repo, nil, nil))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetIssueFilter(config.IssueFilterConfig{AgentProposal: config.AgentProposalGateConfig{Labels: []string{"agent/architect"}}})

	issue := func(login string, labels ...string) *gh.Issue {
		is := &gh.Issue{Number: gh.Ptr(1), Title: gh.Ptr("[architect] x"), User: &gh.User{Login: gh.Ptr(login)}}
		for _, l := range labels {
			is.Labels = append(is.Labels, &gh.Label{Name: gh.Ptr(l)})
		}
		return is
	}
	now := time.Now()
	if got := c.classifyWebhookIssue(repo, issue("hive[bot]", "agent/architect"), []string{"agent/architect"}, now).Bucket; got != WebhookBucketUnknown {
		t.Errorf("unacknowledged proposal bucket = %q, want %q (left to the reconcile pass)", got, WebhookBucketUnknown)
	}
	acked := []string{"agent/architect", HumanAckLabel}
	if got := c.classifyWebhookIssue(repo, issue("hive[bot]", acked...), acked, now).Bucket; got != WebhookBucketActionable {
		t.Errorf("approved proposal bucket = %q, want %q", got, WebhookBucketActionable)
	}
	if got := c.classifyWebhookIssue(repo, issue("alice", "agent/architect"), []string{"agent/architect"}, now).Bucket; got != WebhookBucketActionable {
		t.Errorf("human-filed issue bucket = %q, want %q", got, WebhookBucketActionable)
	}
}
