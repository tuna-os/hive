package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/redmain"
	"github.com/hivecommons/hive/pkg/revertsheriff"
	"github.com/hivecommons/hive/pkg/writeback"
)

// RevertSheriffOptions configures one SweepRevertSheriff pass.
type RevertSheriffOptions struct {
	// Watches reports whether the sheriff covers a repo (owner/repo).
	Watches        func(repo string) bool
	Mode           string
	Hold           bool
	MaxPerDay      int
	Lookback       int
	ConfirmTimeout time.Duration
	Now            time.Time
	// Audit receives one event per thing the sheriff decided or did.
	Audit func(RevertSheriffEvent)
}

// RevertSheriffEvent is one sheriff decision, for the audit log.
type RevertSheriffEvent struct {
	Repo, Check string
	// Action is one of: unattributable, no-pr, rerun, flaky, gave-up,
	// refused, would-revert, reverted, failed.
	Action    string
	Culprit   string
	PR        int
	Reason    string
	RevertPR  int
	RevertURL string
}

// RevertSheriffResult summarizes one pass.
type RevertSheriffResult struct {
	Repos, Red, Events int
}

func (c *Client) revertSheriffLedger() *revertsheriff.Ledger {
	c.revertSheriffOnce.Do(func() { c.revertSheriff = revertsheriff.NewLedger() })
	return c.revertSheriff
}

// SweepRevertSheriff looks at every watched repo whose default branch the
// red-main sweep last saw red, finds the merge that broke each red required
// check, confirms it with one re-run of the failing job, and in propose mode
// opens a revert PR. It reads the red-main tracker, so it must run after
// SweepRedMain.
func (c *Client) SweepRevertSheriff(ctx context.Context, opts RevertSheriffOptions) (RevertSheriffResult, error) {
	var res RevertSheriffResult
	if c == nil || c.client == nil {
		return res, ErrNoGitHubClient
	}
	if opts.Watches == nil {
		return res, nil
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	ctx = WithRESTCaller(ctx, "hive:revert_sheriff")
	ledger := c.revertSheriffLedger()
	tracker := c.redMainTracker()
	audit := func(ev RevertSheriffEvent) {
		res.Events++
		if opts.Audit != nil {
			opts.Audit(ev)
		}
	}
	for _, repo := range c.activeRepos() {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		key := c.redMainRepoKey(repo)
		if !opts.Watches(key) {
			continue
		}
		res.Repos++
		branch, checks := tracker.Red(key)
		red := make(map[string]bool, len(checks))
		for _, check := range checks {
			red[check] = true
		}
		ledger.Settle(key, red)
		if branch == "" || len(checks) == 0 {
			continue
		}
		res.Red++
		for _, check := range checks {
			if err := c.revertSheriffCheck(ctx, ledger, key, branch, check, opts, audit); err != nil {
				c.logger.Debug("revert sheriff: check skipped this pass", "repo", key, "check", check, "error", err)
			}
		}
	}
	return res, nil
}

func (c *Client) revertSheriffCheck(ctx context.Context, ledger *revertsheriff.Ledger, key, branch, check string, opts RevertSheriffOptions, audit func(RevertSheriffEvent)) error {
	owner, name := c.splitRepo(key)
	now := opts.Now
	cs := ledger.Case(key, check)
	if cs == nil || (cs.Phase.Terminal() && cs.Finding.Culprit == "") {
		commits, err := c.revertSheriffWalk(ctx, owner, name, branch, check, opts.Lookback)
		if err != nil {
			return err
		}
		tip := ""
		if len(commits) > 0 {
			tip = commits[0].SHA
		}
		if cs != nil && cs.Tip == tip {
			return nil // unattributable at this tip; wait for a new one
		}
		finding, reason, ok := revertsheriff.FindCulprit(commits)
		if !ok {
			ledger.Open(&revertsheriff.Case{Repo: key, Check: check, Tip: tip, Phase: revertsheriff.GaveUp, Reason: reason, Updated: now})
			audit(RevertSheriffEvent{Repo: key, Check: check, Action: "unattributable", Reason: reason})
			return nil
		}
		pr, err := c.revertSheriffMergedPR(ctx, owner, name, finding.Culprit)
		if err != nil {
			return err
		}
		if pr == nil {
			ledger.Open(&revertsheriff.Case{Repo: key, Check: check, Finding: finding, Tip: tip, Phase: revertsheriff.GaveUp, Reason: "culprit is not a PR merge", Updated: now})
			audit(RevertSheriffEvent{Repo: key, Check: check, Action: "no-pr", Culprit: finding.Culprit, Reason: "culprit is not a PR merge"})
			return nil
		}
		cs = ledger.Open(&revertsheriff.Case{Repo: key, Check: check, Finding: finding, PR: pr.GetNumber(), Tip: tip, Phase: revertsheriff.Suspected, Updated: now})
	}
	if cs.Phase.Terminal() {
		return nil
	}
	outcome, run, err := c.revertSheriffOutcome(ctx, owner, name, cs.Finding.Culprit, check)
	if err != nil {
		return err
	}
	obs := revertsheriff.Observation{Outcome: outcome}
	if run != nil {
		obs.StartedAt = run.GetStartedAt().Time
	}
	ev := RevertSheriffEvent{Repo: key, Check: check, Culprit: cs.Finding.Culprit, PR: cs.PR}
	switch cs.Advance(obs, now, opts.ConfirmTimeout) {
	case revertsheriff.RequestRerun:
		pr, _, err := c.client.PullRequests.Get(ctx, owner, name, cs.PR)
		if err != nil {
			return fmt.Errorf("reading culprit PR before CI recovery: %w", err)
		}
		if d := c.WritebackAllowed(key, c.sentinelWritebackItem(pr), writeback.KindCIRerun, "revert sheriff: rerun failing job"); !d.Allowed {
			cs.Finish(false, d.Detail, now)
			ev.Action, ev.Reason = "refused", d.Detail
			audit(ev)
			return nil
		}
		rerr := c.revertSheriffRerun(ctx, owner, name, run)
		cs.Rerun(rerr, now)
		ev.Action, ev.Reason = "rerun", "re-ran the failing job to rule out a flake"
		if rerr != nil {
			ev.Action, ev.Reason = "gave-up", cs.Reason
		}
		audit(ev)
	case revertsheriff.Done:
		ev.Action, ev.Reason = cs.Phase.String(), cs.Reason
		audit(ev)
	case revertsheriff.Revert:
		c.revertSheriffAct(ctx, ledger, cs, owner, name, branch, run, opts, ev, audit)
	}
	return nil
}

// revertSheriffWalk returns the check's outcome on up to lookback commits of
// branch, newest first, following first parents so a merged branch's own
// commits (which never ran the default-branch checks) are not walked. It
// stops at the first green commit.
func (c *Client) revertSheriffWalk(ctx context.Context, owner, name, branch, check string, lookback int) ([]revertsheriff.Commit, error) {
	if lookback <= 0 {
		lookback = 10
	}
	perPage := lookback * 3
	if perPage > 100 {
		perPage = 100
	}
	list, _, err := c.client.Repositories.ListCommits(ctx, owner, name, &gh.CommitsListOptions{SHA: branch, ListOptions: gh.ListOptions{PerPage: perPage}})
	if err != nil {
		return nil, fmt.Errorf("listing %s/%s@%s commits: %w", owner, name, branch, err)
	}
	if len(list) == 0 {
		return nil, nil
	}
	parent := make(map[string]string, len(list))
	for _, rc := range list {
		if ps := rc.Parents; len(ps) > 0 {
			parent[rc.GetSHA()] = ps[0].GetSHA()
		}
	}
	var out []revertsheriff.Commit
	for sha := list[0].GetSHA(); sha != "" && len(out) < lookback; sha = parent[sha] {
		outcome, _, err := c.revertSheriffOutcome(ctx, owner, name, sha, check)
		if err != nil {
			return nil, err
		}
		out = append(out, revertsheriff.Commit{SHA: sha, Outcome: outcome})
		if outcome == revertsheriff.Green {
			break
		}
	}
	return out, nil
}

// revertSheriffOutcome reads check's latest run on ref. A cancelled or
// otherwise inconclusive run is Unknown: it says nothing about the code.
func (c *Client) revertSheriffOutcome(ctx context.Context, owner, name, ref, check string) (revertsheriff.Outcome, *gh.CheckRun, error) {
	runs, _, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, name, ref, &gh.ListCheckRunsOptions{CheckName: gh.Ptr(check), ListOptions: gh.ListOptions{PerPage: 100}})
	if err != nil {
		return revertsheriff.Unknown, nil, fmt.Errorf("reading %q on %s/%s@%s: %w", check, owner, name, ref, err)
	}
	var latest *gh.CheckRun
	if runs != nil {
		for _, cr := range runs.CheckRuns {
			if cr.GetName() != check {
				continue
			}
			if latest == nil || checkRunIsNewer(cr, latest) {
				latest = cr
			}
		}
	}
	return sheriffOutcome(latest), latest, nil
}

func sheriffOutcome(cr *gh.CheckRun) revertsheriff.Outcome {
	if cr == nil {
		return revertsheriff.Unknown
	}
	if cr.GetStatus() != "completed" {
		return revertsheriff.Pending
	}
	switch cr.GetConclusion() {
	case "success", "neutral":
		return revertsheriff.Green
	case "skipped":
		return revertsheriff.Skipped
	case "failure", "timed_out", "startup_failure":
		return revertsheriff.Red
	}
	return revertsheriff.Unknown
}

// revertSheriffMergedPR returns the merged PR whose merge commit is sha, or
// nil when sha was not produced by merging a PR.
func (c *Client) revertSheriffMergedPR(ctx context.Context, owner, name, sha string) (*gh.PullRequest, error) {
	prs, _, err := c.client.PullRequests.ListPullRequestsWithCommit(ctx, owner, name, sha, nil)
	if err != nil {
		return nil, fmt.Errorf("finding the PR for %s/%s@%s: %w", owner, name, sha, err)
	}
	for _, pr := range prs {
		if pr.MergedAt != nil && pr.GetMergeCommitSHA() == sha {
			return pr, nil
		}
	}
	return nil, nil
}

var errSheriffNoJob = errors.New("the failing check is not a GitHub Actions job, so it cannot be re-run")

// revertSheriffRerun re-runs the job behind a failing check run. For GitHub
// Actions the check run id is the job id.
func (c *Client) revertSheriffRerun(ctx context.Context, owner, name string, run *gh.CheckRun) error {
	if run == nil || run.GetApp().GetSlug() != "github-actions" {
		return errSheriffNoJob
	}
	if _, err := c.client.Actions.RerunJobByID(ctx, owner, name, run.GetID()); err != nil {
		return fmt.Errorf("re-running job %d: %w", run.GetID(), err)
	}
	return nil
}

func (c *Client) revertSheriffAct(ctx context.Context, ledger *revertsheriff.Ledger, cs *revertsheriff.Case, owner, name, branch string, run *gh.CheckRun, opts RevertSheriffOptions, ev RevertSheriffEvent, audit func(RevertSheriffEvent)) {
	now := opts.Now
	refuse := func(reason string) {
		cs.Finish(false, reason, now)
		ev.Action, ev.Reason = "refused", reason
		audit(ev)
	}
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, cs.PR)
	if err != nil {
		refuse("cannot read the culprit PR: " + err.Error())
		return
	}
	facts := revertsheriff.PRFacts{Number: pr.GetNumber(), Title: pr.GetTitle()}
	for _, l := range pr.Labels {
		facts.Labels = append(facts.Labels, l.GetName())
	}
	if patterns := c.humanMergePathsFor(cs.Repo); len(patterns) > 0 {
		files, err := ListPRChangedFiles(ctx, c.client, owner, name, pr)
		if err != nil {
			refuse("cannot list the culprit's files to check human-merge paths: " + err.Error())
			return
		}
		facts.ProtectedPaths = intent.HumanMergePathMatches(files, patterns)
	}
	open, err := c.openSheriffRevert(ctx, owner, name, cs.Repo, cs.PR)
	if err != nil {
		refuse("cannot check for an open revert: " + err.Error())
		return
	}
	facts.OpenRevert = open
	if reason, ok := revertsheriff.Gate(facts, ledger.RevertsSince(cs.Repo, now), opts.MaxPerDay); !ok {
		refuse(reason)
		return
	}
	runURL := ""
	if run != nil {
		runURL = run.GetHTMLURL()
	}
	if opts.Mode != config.RevertSheriffPropose {
		cs.Finish(true, "shadow mode: would revert", now)
		ev.Action, ev.Reason = "would-revert", fmt.Sprintf("%q failed on %s at this merge and again on re-run; %s", cs.Check, branch, runURL)
		audit(ev)
		return
	}
	if d := c.WritebackAllowed(cs.Repo, c.sentinelWritebackItem(pr), writeback.KindCIRerun, "revert sheriff: propose a held revert after confirmed CI failure"); !d.Allowed {
		refuse(d.Detail)
		return
	}
	number, url, err := c.openRevertPR(ctx, owner, name, pr, cs, branch, runURL, opts.Hold)
	if err != nil {
		cs.Finish(false, "revert failed: "+err.Error(), now)
		ev.Action, ev.Reason = "failed", err.Error()
		audit(ev)
		return
	}
	ledger.RecordRevert(cs.Repo, now)
	cs.Finish(true, fmt.Sprintf("opened revert #%d", number), now)
	ev.Action, ev.RevertPR, ev.RevertURL = "reverted", number, url
	ev.Reason = fmt.Sprintf("%q failed on %s at this merge and again on re-run", cs.Check, branch)
	audit(ev)
}

// openSheriffRevert returns an open sheriff revert PR for repo#pr, or 0.
func (c *Client) openSheriffRevert(ctx context.Context, owner, name, repo string, pr int) (int, error) {
	issues, _, err := c.client.Issues.ListByRepo(ctx, owner, name, &gh.IssueListByRepoOptions{
		State: "open", Labels: []string{revertsheriff.RevertLabel}, ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return 0, err
	}
	for _, is := range issues {
		if is.IsPullRequest() && revertsheriff.MarkerFor(is.GetBody(), repo, pr) {
			return is.GetNumber(), nil
		}
	}
	return 0, nil
}

const revertPullRequestMutation = `mutation($pr:ID!, $title:String!, $body:String!) {
  revertPullRequest(input:{pullRequestId:$pr, title:$title, body:$body}) {
    revertPullRequest { number url }
  }
}`

// openRevertPR asks GitHub to revert pr (the same operation as the Revert
// button), labels the revert and points the culprit at it.
func (c *Client) openRevertPR(ctx context.Context, owner, name string, pr *gh.PullRequest, cs *revertsheriff.Case, branch, runURL string, hold bool) (int, string, error) {
	if pr.GetNodeID() == "" {
		return 0, "", fmt.Errorf("culprit #%d has no node id", pr.GetNumber())
	}
	title := fmt.Sprintf("Revert %q", pr.GetTitle())
	var b strings.Builder
	fmt.Fprintf(&b, "Reverts #%d (merge commit %s).\n\n", pr.GetNumber(), cs.Finding.Culprit)
	fmt.Fprintf(&b, "The required check `%s` went red on `%s` at this merge: it passed on %s and failed here. ", cs.Check, branch, cs.Finding.LastGreen)
	b.WriteString("A re-run of the failing job failed again, so it is not a flake.")
	if runURL != "" {
		fmt.Fprintf(&b, " Failing run: %s", runURL)
	}
	b.WriteString("\n\nOpened by the Hive revert sheriff. Merge it to restore the branch, or close it if a fix is landing first.")
	if hold {
		b.WriteString(" It carries `hold`, so it waits for a person to release it.")
	}
	b.WriteString("\n\n" + revertsheriff.Marker(cs.Repo, pr.GetNumber(), cs.Finding.Culprit) + "\n")

	var out struct {
		RevertPullRequest struct {
			RevertPullRequest struct {
				Number int    `json:"number"`
				URL    string `json:"url"`
			} `json:"revertPullRequest"`
		} `json:"revertPullRequest"`
	}
	vars := map[string]any{"pr": pr.GetNodeID(), "title": title, "body": b.String()}
	if err := c.graphQL(ctx, revertPullRequestMutation, vars, &out); err != nil {
		return 0, "", fmt.Errorf("revertPullRequest #%d: %w", pr.GetNumber(), err)
	}
	number, url := out.RevertPullRequest.RevertPullRequest.Number, out.RevertPullRequest.RevertPullRequest.URL
	if number == 0 {
		return 0, "", fmt.Errorf("revertPullRequest #%d returned no PR", pr.GetNumber())
	}
	repo := owner + "/" + name
	if err := c.EnsureIssueLabel(ctx, repo, revertsheriff.RevertLabel, "b60205", "Revert opened by the Hive revert sheriff"); err != nil {
		c.logger.Warn("revert sheriff: label not ensured", "repo", repo, "error", err)
	}
	labels := []string{revertsheriff.RevertLabel, redmain.FixesMainLabel}
	if hold {
		labels = append(labels, "hold")
	}
	if err := c.AddLabels(ctx, repo, number, labels); err != nil { // writeback:gated by revertSheriffAct (ci_rerun)
		c.logger.Warn("revert sheriff: labels not added", "repo", repo, "pr", number, "error", err)
	}
	note := fmt.Sprintf("The Hive revert sheriff opened #%d to revert this PR: the required check `%s` failed on `%s` at its merge commit and again on re-run.", number, cs.Check, branch)
	if runURL != "" {
		note += " Failing run: " + runURL
	}
	if err := c.CreateIssueComment(ctx, repo, pr.GetNumber(), note); err != nil { // writeback:gated by revertSheriffAct (ci_rerun)
		c.logger.Warn("revert sheriff: culprit not notified", "repo", repo, "pr", pr.GetNumber(), "error", err)
	}
	return number, url, nil
}
