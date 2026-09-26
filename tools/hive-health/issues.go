package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// HealthLabel marks every issue this workflow manages.
const HealthLabel = "hive-health"

// IssueLabels are applied to a new health issue. `agent/operations` routes it
// to the operations lane (pkg/classify label routing matches the `operations`
// segment); the `[operations]` title prefix does the same. Deliberately NOT
// applied: `hold` (would hide it from every agent) and `ai-fix-requested`
// (ai-fix.yml would hand it to Copilot with write access — that is the
// human-approved step, see README).
var IssueLabels = []string{HealthLabel, "agent/operations"}

// OpenIssue is the subset of `gh issue list --json number,title,body` used.
type OpenIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// Action is one step of the issue sync.
type Action struct {
	Op     string   `json:"op"` // create | update | close
	Target string   `json:"target"`
	Number int      `json:"number,omitempty"`
	Title  string   `json:"title,omitempty"`
	Labels []string `json:"labels,omitempty"`
	// Body is the full issue body (create/update); Comment is posted before
	// the edit or close when non-empty.
	Body    string `json:"body,omitempty"`
	Comment string `json:"comment,omitempty"`
}

var markerRE = regexp.MustCompile(`<!-- hive-health:target=([A-Za-z0-9._-]+) -->`)
var fingerprintRE = regexp.MustCompile(`<!-- hive-health:fingerprint=([0-9a-f]*) -->`)

// PlanIssues decides what to do with the per-Hive issues. It opens one issue
// per failing Hive, keeps it current while it fails, and closes it once the
// Hive is no longer failing (warnings alone never open or hold an issue).
func PlanIssues(r Report, open []OpenIssue, runURL string) []Action {
	byTarget := map[string][]OpenIssue{}
	for _, is := range open {
		if m := markerRE.FindStringSubmatch(is.Body); m != nil {
			byTarget[m[1]] = append(byTarget[m[1]], is)
		}
	}
	for k := range byTarget {
		sort.Slice(byTarget[k], func(i, j int) bool { return byTarget[k][i].Number < byTarget[k][j].Number })
	}

	var actions []Action
	for _, h := range r.Hives {
		existing := byTarget[h.Name]
		failing := h.Level == Fail
		switch {
		case failing && len(existing) == 0:
			actions = append(actions, Action{Op: "create", Target: h.Name, Title: IssueTitle(h),
				Labels: IssueLabels, Body: IssueBody(h, r.GeneratedAt, runURL)})
		case failing:
			keep := existing[0]
			a := Action{Op: "update", Target: h.Name, Number: keep.Number, Title: IssueTitle(h),
				Body: IssueBody(h, r.GeneratedAt, runURL)}
			if prevFP := fingerprintOf(keep.Body); prevFP != h.Fingerprint {
				a.Comment = changedComment(h, keep.Body, r.GeneratedAt, runURL)
			}
			actions = append(actions, a)
			// Duplicates (a race, or a hand-copied marker) collapse into the oldest.
			for _, dup := range existing[1:] {
				actions = append(actions, Action{Op: "close", Target: h.Name, Number: dup.Number,
					Comment: fmt.Sprintf("Duplicate of #%d, which tracks %s's health.", keep.Number, h.Name)})
			}
		default:
			for _, is := range existing {
				actions = append(actions, Action{Op: "close", Target: h.Name, Number: is.Number,
					Comment: recoveredComment(h, r.GeneratedAt, runURL)})
			}
		}
	}
	return actions
}

func fingerprintOf(body string) string {
	if m := fingerprintRE.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

func changedComment(h HiveReport, oldBody string, at time.Time, runURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Failing checks changed at %s", at.UTC().Format("2006-01-02 15:04 UTC"))
	if runURL != "" {
		fmt.Fprintf(&b, " ([run](%s))", runURL)
	}
	b.WriteString(". Now failing:\n\n")
	for _, c := range h.ChecksAt(Fail) {
		status := "already reported"
		if !strings.Contains(oldBody, "`"+c.ID+"`") {
			status = "new"
		}
		fmt.Fprintf(&b, "- `%s` (%s): %s\n", c.ID, status, mdEscape(c.Summary))
	}
	return b.String()
}

func recoveredComment(h HiveReport, at time.Time, runURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is no longer failing (verdict **%s**) as of %s", h.Name, h.Level, at.UTC().Format("2006-01-02 15:04 UTC"))
	if runURL != "" {
		fmt.Fprintf(&b, " ([run](%s))", runURL)
	}
	b.WriteString(". Closing; the probe reopens a new issue if it fails again.")
	if ws := h.ChecksAt(Warn); len(ws) > 0 {
		b.WriteString("\n\nRemaining warnings (these do not hold the issue open):\n\n")
		for _, c := range ws {
			fmt.Fprintf(&b, "- `%s`: %s\n", c.ID, mdEscape(c.Summary))
		}
	}
	return b.String()
}
