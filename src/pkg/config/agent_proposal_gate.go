package config

import "strings"

// AgentProposalGateConfig (project.issue_filter.agent_proposal) keeps a
// hive-filed DIRECTION proposal out of the actionable set until a human has
// acknowledged it.
//
// #5117 already refuses to merge a PR whose cited issues are all
// agent-authored and unacknowledged, but it fires on the PR side: by then an
// implementer has spent the run, opened the PR, and the queue carries it
// until somebody closes it. On tuna-os/tunaOS that is how ~127 open
// [architect]/[strategist]/[outreach] proposals turned into a steady stream
// of unrequested docs and refactor PRs. Reporter trust (#9665) cannot carry
// this: it judges human reporters only and leaves hive-filed issues to #5117
// by design.
//
// This gate is the admission-side half of #5117, scoped to proposals: an
// issue is held only when ALL of these are true —
//   - its author is not human (the hive App, or any bot);
//   - it is not acknowledged (approved-direction label, a human assignee, or
//     an acknowledged parent — the same signals #5117 lifts on);
//   - it carries one of Labels or its title starts with one of TitlePrefixes.
//
// A scanner's bug report, a CI-maintainer's failure tracker and every
// human-filed issue are unaffected. Opt-in: the zero value holds nothing.
type AgentProposalGateConfig struct {
	// Labels mark an issue as a proposal (exact, case-insensitive), e.g.
	// agent/architect, agent/strategist.
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// TitlePrefixes mark an issue as a proposal by its title (case-
	// insensitive prefix), e.g. "[architect]". Covers proposals whose lane
	// label was never applied.
	TitlePrefixes []string `yaml:"title_prefixes,omitempty" json:"title_prefixes,omitempty"`
}

// IsZero reports whether the gate is off.
func (g AgentProposalGateConfig) IsZero() bool {
	return len(g.Labels) == 0 && len(g.TitlePrefixes) == 0
}

// Equal reports whether two gates are identical (order-sensitive, exact).
func (g AgentProposalGateConfig) Equal(o AgentProposalGateConfig) bool {
	return equalStringSlices(g.Labels, o.Labels) && equalStringSlices(g.TitlePrefixes, o.TitlePrefixes)
}

// IsProposal reports whether an issue with this title and these labels is a
// proposal under the gate. Blank configured entries never match.
func (g AgentProposalGateConfig) IsProposal(title string, labels []string) bool {
	for _, want := range g.Labels {
		if strings.TrimSpace(want) == "" {
			continue
		}
		for _, have := range labels {
			if labelMatches(want, have) {
				return true
			}
		}
	}
	title = strings.ToLower(strings.TrimSpace(title))
	for _, prefix := range g.TitlePrefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix != "" && strings.HasPrefix(title, prefix) {
			return true
		}
	}
	return false
}

// HoldsAgentProposal implements github.AgentProposalGate.
func (f IssueFilterConfig) HoldsAgentProposal(title string, labels []string) bool {
	return f.AgentProposal.IsProposal(title, labels)
}
