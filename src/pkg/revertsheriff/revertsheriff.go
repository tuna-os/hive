// Package revertsheriff decides when a merge that broke a required check on a
// default branch should be reverted. It holds the rules only: finding the
// commit that turned a check red, confirming the failure by a re-run, and the
// gates a revert must pass. pkg/github reads the forge and acts.
//
// The sheriff never acts on a single red result. A culprit is the newest
// commit whose required check is red while its parent's is green; its failing
// job is re-run once, and only a red re-run confirms it. A green re-run marks
// the failure flaky and nothing is reverted.
package revertsheriff

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Outcome is a required check's latest result on one commit.
type Outcome int

const (
	// Unknown means the check has no run on the commit.
	Unknown Outcome = iota
	// Pending means the check's latest run has not completed.
	Pending
	// Green means the latest run completed and passed.
	Green
	// Red means the latest run completed and failed.
	Red
	// Skipped means the check's run skipped itself, e.g. a path-filtered job
	// on a commit outside its paths. That commit did not change what the
	// check tests, so it cannot be the culprit and the walk passes over it.
	Skipped
)

func (o Outcome) String() string {
	switch o {
	case Pending:
		return "pending"
	case Green:
		return "green"
	case Red:
		return "red"
	case Skipped:
		return "skipped"
	}
	return "unknown"
}

// Commit is one default-branch commit and the check's outcome on it.
type Commit struct {
	SHA     string
	Outcome Outcome
}

// Finding is the commit that turned the check red.
type Finding struct {
	// Culprit is the oldest red commit on top of the branch.
	Culprit string
	// LastGreen is the nearest older commit where the check passed.
	LastGreen string
	// RedCommits counts the commits from the tip down to the culprit.
	RedCommits int
}

// FindCulprit walks commits newest first. The tip must be red; the culprit
// is the oldest red commit before the first green one, and skipped commits in
// between are passed over. A commit with no completed result inside the walk
// makes the break unattributable, because that commit may be the one that
// broke it.
func FindCulprit(commits []Commit) (Finding, string, bool) {
	if len(commits) == 0 {
		return Finding{}, "no commits", false
	}
	if commits[0].Outcome != Red {
		return Finding{}, "tip is " + commits[0].Outcome.String(), false
	}
	culprit := 0
	for i := 1; i < len(commits); i++ {
		switch commits[i].Outcome {
		case Red:
			culprit = i
		case Skipped:
			continue
		case Green:
			return Finding{Culprit: commits[culprit].SHA, LastGreen: commits[i].SHA, RedCommits: culprit + 1}, "", true
		default:
			return Finding{}, fmt.Sprintf("commit %s has no completed result (%s)", short(commits[i].SHA), commits[i].Outcome), false
		}
	}
	return Finding{}, fmt.Sprintf("no green result in the %d commits looked at", len(commits)), false
}

// Phase is where a case stands.
type Phase int

const (
	// Suspected: a culprit was found; its failing job has not been re-run.
	Suspected Phase = iota
	// Rerunning: the failing job was re-run; waiting for the result.
	Rerunning
	// Confirmed: the re-run failed too; the culprit may be reverted.
	Confirmed
	// Flaky: the re-run passed; nothing is reverted.
	Flaky
	// GaveUp: the failure could not be confirmed (no re-runnable job, or the
	// re-run did not finish in time).
	GaveUp
	// Acted: the revert was proposed (or logged, in shadow mode).
	Acted
	// Refused: confirmed, but a gate refused the revert.
	Refused
)

func (p Phase) String() string {
	return [...]string{"suspected", "rerunning", "confirmed", "flaky", "gave-up", "acted", "refused"}[p]
}

// Terminal reports whether nothing more happens for the case.
func (p Phase) Terminal() bool { return p >= Flaky }

// Case is one red required check on one repo and its culprit.
type Case struct {
	Repo, Check string
	Finding     Finding
	// PR is the merged pull request whose merge commit is the culprit.
	PR int
	// Tip is the branch tip the case was opened at. A case with no culprit
	// is looked at again once the tip moves.
	Tip     string
	Phase   Phase
	Reason  string
	RerunAt time.Time
	Updated time.Time
}

// Step is what the caller should do next for a case.
type Step int

const (
	// Wait: nothing to do this pass.
	Wait Step = iota
	// RequestRerun: re-run the culprit's failing job, then call Rerun.
	RequestRerun
	// Revert: the failure is confirmed; pass the gates and revert.
	Revert
	// Done: the case is terminal.
	Done
)

// Observation is the culprit's check as read this pass.
type Observation struct {
	Outcome Outcome
	// StartedAt is when the check's latest run on the culprit started.
	StartedAt time.Time
}

// Advance moves a case on from what the sheriff saw this pass.
func (c *Case) Advance(obs Observation, now time.Time, confirmTimeout time.Duration) Step {
	switch c.Phase {
	case Suspected:
		return RequestRerun
	case Rerunning:
		// Only a run that started after the re-run request is its result.
		if !obs.StartedAt.IsZero() && !obs.StartedAt.Before(c.RerunAt) {
			switch obs.Outcome {
			case Red:
				c.set(Confirmed, "re-run failed again", now)
				return Revert
			case Green:
				c.set(Flaky, "re-run passed", now)
				return Done
			}
		}
		if confirmTimeout > 0 && now.Sub(c.RerunAt) > confirmTimeout {
			c.set(GaveUp, "re-run did not finish within "+confirmTimeout.String(), now)
			return Done
		}
		return Wait
	case Confirmed:
		return Revert
	}
	return Done
}

// Rerun records the outcome of the re-run request.
func (c *Case) Rerun(err error, now time.Time) {
	if err != nil {
		c.set(GaveUp, "cannot re-run the failing job: "+err.Error(), now)
		return
	}
	c.Phase = Rerunning
	c.RerunAt = now
	c.Updated = now
}

// Finish marks a confirmed case acted on or refused.
func (c *Case) Finish(acted bool, reason string, now time.Time) {
	if acted {
		c.set(Acted, reason, now)
		return
	}
	c.set(Refused, reason, now)
}

func (c *Case) set(p Phase, reason string, now time.Time) {
	c.Phase = p
	c.Reason = reason
	c.Updated = now
}

// PRFacts are what the gates need to know about the culprit PR.
type PRFacts struct {
	Number int
	Title  string
	Labels []string
	// ProtectedPaths are the changed files that match the repo's
	// human-merge paths; any match refuses the revert.
	ProtectedPaths []string
	// OpenRevert is an open revert PR already proposed for this PR, or 0.
	OpenRevert int
}

// RevertLabel marks revert PRs the sheriff opens.
const RevertLabel = "hive/revert"

// Gate refuses a confirmed revert that should not happen automatically.
func Gate(pr PRFacts, revertsToday, maxPerDay int) (string, bool) {
	if pr.OpenRevert > 0 {
		return fmt.Sprintf("revert #%d is already open", pr.OpenRevert), false
	}
	for _, l := range pr.Labels {
		if strings.EqualFold(l, RevertLabel) {
			return "culprit is itself a sheriff revert", false
		}
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(pr.Title)), "revert ") {
		return "culprit is itself a revert", false
	}
	if len(pr.ProtectedPaths) > 0 {
		return "culprit touches human-merge paths: " + strings.Join(pr.ProtectedPaths, ", "), false
	}
	if maxPerDay > 0 && revertsToday >= maxPerDay {
		return fmt.Sprintf("daily cap of %d reverts reached", maxPerDay), false
	}
	return "", true
}

// Marker is the hidden line that ties a revert PR to its culprit.
func Marker(repo string, pr int, sha string) string {
	return fmt.Sprintf("<!-- hive:revert-sheriff repo=%s pr=%d sha=%s -->", strings.ToLower(repo), pr, sha)
}

// MarkerFor reports whether body carries the marker for repo#pr.
func MarkerFor(body, repo string, pr int) bool {
	return strings.Contains(body, fmt.Sprintf("<!-- hive:revert-sheriff repo=%s pr=%d ", strings.ToLower(repo), pr))
}

// Ledger keeps cases and the reverts proposed per repo and day. It is
// process-local: after a restart a still-red check is found again and its
// job re-run again, which can only delay a revert.
type Ledger struct {
	mu    sync.Mutex
	cases map[string]*Case
	acted map[string][]time.Time
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{cases: map[string]*Case{}, acted: map[string][]time.Time{}}
}

func caseKey(repo, check string) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "\x00" + check
}

// Case returns the case for repo's check, or nil.
func (l *Ledger) Case(repo, check string) *Case {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cases[caseKey(repo, check)]
}

// Open starts a case. An existing case for the same culprit is kept; a case
// with no culprit (unattributable) is always replaced, so it records the
// latest tip.
func (l *Ledger) Open(c *Case) *Case {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := caseKey(c.Repo, c.Check)
	if prev := l.cases[k]; prev != nil && prev.Finding.Culprit != "" && prev.Finding.Culprit == c.Finding.Culprit {
		return prev
	}
	l.cases[k] = c
	return c
}

// Settle drops repo's cases whose check is no longer red. red holds the
// checks red on the tip now.
func (l *Ledger) Settle(repo string, red map[string]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	prefix := strings.ToLower(strings.TrimSpace(repo)) + "\x00"
	for k, c := range l.cases {
		if strings.HasPrefix(k, prefix) && !red[c.Check] {
			delete(l.cases, k)
		}
	}
}

// RecordRevert counts a revert proposed for repo at now.
func (l *Ledger) RecordRevert(repo string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := strings.ToLower(strings.TrimSpace(repo))
	l.acted[k] = append(l.acted[k], now)
}

// RevertsSince counts reverts proposed for repo in the 24 hours before now.
func (l *Ledger) RevertsSince(repo string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := strings.ToLower(strings.TrimSpace(repo))
	kept := l.acted[k][:0]
	for _, t := range l.acted[k] {
		if now.Sub(t) < 24*time.Hour {
			kept = append(kept, t)
		}
	}
	l.acted[k] = kept
	return len(kept)
}

func short(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}
