package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Pre-dispatch guard against re-dispatching an issue whose fix is already
// open (hivecommons/hive#14799). Every automated claim producer runs
// IssueClaimBlockReason first; that gate used to see only the eval cycle's
// claim ledger, which lags a PR a person opened by hand until the next scan.
// #14766 and #14763 were each handed to a relay contributor six minutes
// after their fix PR opened, and the resulting pair of same-issue PRs ended
// up at stage/085 for the owner to pick between. The index here answers the
// same question live — one open-PR listing per repo per sweep, parsed with
// the claim ledger's own closing-keyword grammar — so the guard sees the PR
// the moment it exists.

const (
	// HasOpenPRLabel marks an issue the guard refused to dispatch because an
	// open pull request already links it with a closing keyword. It is a
	// signal for the tracker, not a park: the label comes off with nothing
	// — the stage engine re-derives the issue's stage from the PR itself.
	HasOpenPRLabel            = "has-open-pr"
	hasOpenPRLabelColor       = "0e8a16"
	hasOpenPRLabelDescription = "An open PR already links this issue with Fixes/Closes/Resolves; the hive will not dispatch it again"

	// ClaimBlockReasonOpenPRPrefix prefixes the IssueClaimBlockReason for an
	// issue an open PR already links: "open PR already linked: #14775".
	ClaimBlockReasonOpenPRPrefix = "open PR already linked: #"

	// ClaimBlockReasonOpenPRRateLimited is the IssueDispatchBlockReason while
	// GitHub rate-limits the guard's open-PR listing: the guard cannot tell
	// whether a PR already links the issue, so the dispatch is deferred until
	// the limit resets rather than admitted blind.
	ClaimBlockReasonOpenPRRateLimited = "open-PR guard deferred: GitHub rate limit"

	// openPRLinkCacheTTL bounds how long one repo's open-PR listing answers
	// the guard. A sweep (contributor selectTask pass, eval-cycle claim
	// burst) finishes well inside it, so the listing is paid once per sweep;
	// a PR opened after the listing is seen on the next one.
	openPRLinkCacheTTL = time.Minute

	// openPRLinkListTimeout bounds one full open-PR listing for the guard.
	openPRLinkListTimeout = 45 * time.Second
	// openPRLinkFailureTTL is how long a failed listing answers the guard
	// for its repo: a surge of claims fails open at once instead of each one
	// re-running a listing that just timed out and re-logging the warning.
	// It is also the backoff for a rate limit whose reset time is unknown.
	openPRLinkFailureTTL = time.Minute
	// openPRLinkRateLimitMaxBackoff caps how long a rate-limited listing
	// answers the guard; GitHub's primary window resets within the hour.
	openPRLinkRateLimitMaxBackoff = time.Hour
)

// errOpenPRListingBackedOff marks an error replayed from the failure cache.
var errOpenPRListingBackedOff = errors.New("open-PR listing failed recently")

type openPRLinkFailure struct {
	err   error
	until time.Time
}

// openPRListingRateLimitedError is a listing GitHub refused for its primary
// or secondary rate limit. until is when the guard lists again: the limit's
// reset (Rate.Reset / Retry-After), or openPRLinkFailureTTL when unknown.
type openPRListingRateLimitedError struct {
	until time.Time
	err   error
}

func (e *openPRListingRateLimitedError) Error() string {
	return fmt.Sprintf("open-PR listing rate-limited until %s: %v", e.until.UTC().Format(time.RFC3339), e.err)
}

func (e *openPRListingRateLimitedError) Unwrap() error { return e.err }

// OpenPRLink is one open pull request that links an issue with a closing
// keyword, as the guard found it — or, when RateLimitedUntil is set, the
// guard's refusal for an issue it could not check (no PR fields are set).
type OpenPRLink struct {
	// Repo is the issue's repository in the configured spelling.
	Repo string
	// Issue is the linked issue number.
	Issue int
	// PRRepo / PRNumber / PRURL / PRAuthor identify the linking PR.
	PRRepo   string
	PRNumber int
	PRURL    string
	PRAuthor string
	// CreatedAt is when the PR was opened; with several open PRs linking
	// one issue the earliest is reported.
	CreatedAt time.Time
	// RateLimitedUntil is non-zero when GitHub rate-limited the open-PR
	// listing: no PR is known to link the issue, but the guard refuses the
	// dispatch until this time rather than admitting it blind.
	RateLimitedUntil time.Time
}

// RateLimited reports whether l is a rate-limit deferral rather than a PR.
func (l OpenPRLink) RateLimited() bool { return !l.RateLimitedUntil.IsZero() }

// openPRLinkIndex is one repo's open PRs indexed by the issue they close.
type openPRLinkIndex struct {
	fetchedAt time.Time
	// byIssue maps claimKey(issueRepo, issue) to every open PR closing it,
	// sorted by creation time (earliest first).
	byIssue map[string][]OpenPRLink
	// prs is the raw listing, kept for the duplicate-fix resolver so it does
	// not list the repo a second time in the same sweep.
	prs []*gh.PullRequest
}

// openPRLinkCache is the per-client, per-repo cache behind the guard.
type openPRLinkCache struct {
	mu    sync.Mutex
	repos map[string]*openPRLinkIndex
	// failures holds each repo's last listing failure until its until:
	// openPRLinkFailureTTL, or a rate limit's reset.
	failures map[string]openPRLinkFailure
	// disabled is scanner.skip_issues_with_open_pr: false.
	disabled bool
	now      func() time.Time
}

func (c *Client) openPRLinks() *openPRLinkCache {
	c.openPRLinkOnce.Do(func() {
		c.openPRLinkState = &openPRLinkCache{repos: map[string]*openPRLinkIndex{}, failures: map[string]openPRLinkFailure{}, now: time.Now}
	})
	return c.openPRLinkState
}

// SetSkipIssuesWithOpenPR installs scanner.skip_issues_with_open_pr. The
// guard is on until told otherwise.
func (c *Client) SetSkipIssuesWithOpenPR(enabled bool) {
	if c == nil {
		return
	}
	cache := c.openPRLinks()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.disabled = !enabled
}

// SkipIssuesWithOpenPR reports whether the open-PR guard is on.
func (c *Client) SkipIssuesWithOpenPR() bool {
	if c == nil {
		return false
	}
	cache := c.openPRLinks()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return !cache.disabled
}

// InvalidateOpenPRLinks drops the cached listing for repo (every repo when
// repo is ""), so the next guard call lists again. The PR-opened watcher
// path calls it when the hive itself opens a PR.
func (c *Client) InvalidateOpenPRLinks(repo string) {
	if c == nil {
		return
	}
	cache := c.openPRLinks()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if repo == "" {
		cache.repos = map[string]*openPRLinkIndex{}
		cache.failures = map[string]openPRLinkFailure{}
		return
	}
	delete(cache.repos, c.canonicalRepo(repo))
	delete(cache.failures, c.canonicalRepo(repo))
}

// canonicalRepo maps repo onto the configured spelling, so a caller naming
// the repo bare or org-qualified shares one cache entry and one claim key.
func (c *Client) canonicalRepo(repo string) string {
	return canonicalConfigRepo(repo, c.getRepos())
}

// openPRIndexFor returns the (possibly cached) open-PR link index for repo.
// A failed listing is remembered per canonical repo and replayed without a
// GitHub call until it expires: openPRLinkFailureTTL for an ordinary
// failure, the limit's reset for a rate limit (wrapped in
// openPRListingRateLimitedError). go-github answers every call made under
// an exhausted limit with a local RateLimitError, so without the replay each
// claim candidate re-ran the listing and re-logged the failure.
func (c *Client) openPRIndexFor(ctx context.Context, repo string) (*openPRLinkIndex, error) {
	repo = c.canonicalRepo(repo)
	cache := c.openPRLinks()
	cache.mu.Lock()
	now := cache.now()
	if idx, ok := cache.repos[repo]; ok && now.Sub(idx.fetchedAt) < openPRLinkCacheTTL {
		cache.mu.Unlock()
		return idx, nil
	}
	if f, ok := cache.failures[repo]; ok && now.Before(f.until) {
		cache.mu.Unlock()
		return nil, fmt.Errorf("%w: %w", errOpenPRListingBackedOff, f.err)
	}
	cache.mu.Unlock()

	owner, repoName := c.splitRepo(repo)
	listCtx, cancel := context.WithTimeout(WithRESTCaller(ctx, "hive:open_pr_links"), openPRLinkListTimeout)
	defer cancel()
	prs, err := c.listOpenPRsForSupersessionSweep(listCtx, owner, repoName)
	if err != nil {
		cache.mu.Lock()
		failedAt := cache.now()
		until := failedAt.Add(openPRLinkFailureTTL)
		if isRateLimitErr(err) {
			until = failedAt.Add(retryableGitHubDelay(err, failedAt, openPRLinkFailureTTL, openPRLinkFailureTTL, openPRLinkRateLimitMaxBackoff))
			err = &openPRListingRateLimitedError{until: until, err: err}
		}
		cache.failures[repo] = openPRLinkFailure{err: err, until: until}
		cache.mu.Unlock()
		return nil, err
	}
	identity := c.getHiveIdentity()
	if identity.AppLogin == "" {
		identity.AppLogin = c.appBotLogin
	}
	idx := buildOpenPRLinkIndex(prs, repo, identity, now)
	cache.mu.Lock()
	cache.repos[repo] = idx
	delete(cache.failures, repo)
	cache.mu.Unlock()
	return idx, nil
}

// buildOpenPRLinkIndex indexes prs by the issues their title/body close.
// Only closing-keyword claims count: a `Refs #N` reference or a branch-name
// heuristic is work-in-the-area, not "this PR is the fix".
func buildOpenPRLinkIndex(prs []*gh.PullRequest, repo string, identity HiveIdentity, now time.Time) *openPRLinkIndex {
	idx := &openPRLinkIndex{fetchedAt: now, byIssue: map[string][]OpenPRLink{}, prs: prs}
	for _, pr := range prs {
		if pr == nil || pr.GetNumber() <= 0 || (pr.GetState() != "" && !strings.EqualFold(pr.GetState(), PRStateOpen)) {
			continue
		}
		for _, claim := range claimsFromPR(pr, repo, identity, now) {
			if !claim.Closing {
				continue
			}
			key := claimKey(claim.Repo, claim.Issue)
			idx.byIssue[key] = append(idx.byIssue[key], OpenPRLink{
				Repo:      claim.Repo,
				Issue:     claim.Issue,
				PRRepo:    repo,
				PRNumber:  pr.GetNumber(),
				PRURL:     pr.GetHTMLURL(),
				PRAuthor:  claim.PRAuthor,
				CreatedAt: pr.GetCreatedAt().Time,
			})
		}
	}
	for key := range idx.byIssue {
		links := idx.byIssue[key]
		sort.SliceStable(links, func(i, j int) bool {
			if !links[i].CreatedAt.Equal(links[j].CreatedAt) {
				return links[i].CreatedAt.Before(links[j].CreatedAt)
			}
			return links[i].PRNumber < links[j].PRNumber
		})
	}
	return idx
}

// OpenPRLinkingIssue reports the earliest open pull request in repo that
// links issue number with a closing keyword. ok is false when none does.
// A listing failure is returned so the caller decides how to fail.
func (c *Client) OpenPRLinkingIssue(ctx context.Context, repo string, number int) (OpenPRLink, bool, error) {
	if c == nil || c.client == nil {
		return OpenPRLink{}, false, ErrNoGitHubClient
	}
	idx, err := c.openPRIndexFor(ctx, repo)
	if err != nil {
		return OpenPRLink{}, false, fmt.Errorf("listing open PRs linking %s#%d: %w", repo, number, err)
	}
	links := idx.byIssue[claimKey(c.canonicalRepo(repo), number)]
	if len(links) == 0 {
		return OpenPRLink{}, false, nil
	}
	return links[0], true, nil
}

// OpenPRLinkGuard is the pre-dispatch guard every automated claim producer
// runs: it reports the earliest open PR already linking the issue, labels
// the issue HasOpenPRLabel when it does not carry it yet (label created on
// first use) and logs `claim skipped: open PR already linked`. ok is false
// when the guard is off (scanner.skip_issues_with_open_pr: false), no open
// PR links the issue, or the listing failed for any reason but a rate
// limit — such a failure fails OPEN with one warning per repo per backoff
// window: the ledger-backed guards downstream still stand, and a flaky
// listing must not stall every claim in the hive.
//
// A rate-limited listing fails CLOSED instead: ok is true and the link
// carries only RateLimitedUntil (no PR fields, no label written). While the
// installation's limit is exhausted the guard cannot see a PR that already
// links the issue, and the work a dispatch starts needs the same exhausted
// limit anyway, so deferring to the reset costs nothing; admitting handed
// already-linked issues out again. The limit is warned about once per repo
// per window. labels are the issue's current labels as the caller read them
// (nil when unknown).
func (c *Client) OpenPRLinkGuard(ctx context.Context, repo string, number int, labels []string) (OpenPRLink, bool) {
	if c == nil || !c.SkipIssuesWithOpenPR() {
		return OpenPRLink{}, false
	}
	link, ok, err := c.OpenPRLinkingIssue(ctx, repo, number)
	if err != nil {
		// A replayed failure was already warned about when it happened.
		warn := c.logger != nil && !errors.Is(err, errOpenPRListingBackedOff)
		var limited *openPRListingRateLimitedError
		if errors.As(err, &limited) {
			if warn {
				c.logger.Warn("open-PR guard: open-PR listing rate-limited, deferring claims until the limit resets",
					slog.String("repo", repo), slog.Int("issue", number),
					slog.String("reason", "open-pr-listing-rate-limited"),
					slog.Time("retry_at", limited.until), slog.String("error", err.Error()))
			}
			return OpenPRLink{Repo: c.canonicalRepo(repo), Issue: number, RateLimitedUntil: limited.until}, true
		}
		if warn {
			reason := "open-pr-listing-failed"
			if errors.Is(err, context.DeadlineExceeded) {
				reason = "open-pr-listing-timeout"
			}
			c.logger.Warn("open-PR guard: could not list open PRs, admitting the claim on ledger evidence alone",
				slog.String("repo", repo), slog.Int("issue", number),
				slog.String("reason", reason), slog.String("error", err.Error()))
		}
		return OpenPRLink{}, false
	}
	if !ok {
		return OpenPRLink{}, false
	}
	if c.logger != nil {
		c.logger.Info("claim skipped: open PR already linked",
			slog.String("repo", repo), slog.Int("issue", number),
			slog.Int("pr", link.PRNumber), slog.String("pr_url", link.PRURL), slog.String("pr_author", link.PRAuthor))
	}
	c.markIssueHasOpenPR(ctx, repo, number, labels, link)
	return link, true
}

// openPRLinkBlockReason is the IssueClaimBlockReason step for the guard:
// the refusal reason for an issue an open PR already links, or
// ClaimBlockReasonOpenPRRateLimited while the listing is rate-limited; ""
// otherwise.
func (c *Client) openPRLinkBlockReason(ctx context.Context, repo string, number int, labels []string) string {
	link, ok := c.OpenPRLinkGuard(ctx, repo, number, labels)
	if !ok {
		return ""
	}
	if link.RateLimited() {
		return ClaimBlockReasonOpenPRRateLimited
	}
	return fmt.Sprintf("%s%d", ClaimBlockReasonOpenPRPrefix, link.PRNumber)
}

// markIssueHasOpenPR applies HasOpenPRLabel once. Idempotent on the labels
// the caller just read; a label failure is logged, never fatal — the claim
// is refused either way.
func (c *Client) markIssueHasOpenPR(ctx context.Context, repo string, number int, labels []string, link OpenPRLink) {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), HasOpenPRLabel) {
			return
		}
	}
	if err := c.EnsureIssueLabel(ctx, repo, HasOpenPRLabel, hasOpenPRLabelColor, hasOpenPRLabelDescription); err != nil && c.logger != nil {
		c.logger.Warn("open-PR guard: could not ensure label", slog.String("label", HasOpenPRLabel), slog.String("repo", repo), slog.String("error", err.Error()))
	}
	if err := c.AddLabels(ctx, repo, number, []string{HasOpenPRLabel}); err != nil && c.logger != nil {
		c.logger.Warn("open-PR guard: could not label issue", slog.String("label", HasOpenPRLabel),
			slog.String("repo", repo), slog.Int("issue", number), slog.Int("pr", link.PRNumber), slog.String("error", err.Error()))
	}
}
