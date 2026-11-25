package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// ArtifactKind names a non-code handoff a task may declare as its accepted
// output. Pull requests keep their own contract (VerifyReportedPR); this is an
// opt-in sibling for tasks whose deliverable is an issue or a release. An
// artifact result never carries a "merged" or "head green" fact, so it cannot
// stand in for merged-fix proof.
type ArtifactKind string

const (
	ArtifactIssue   ArtifactKind = "issue"
	ArtifactRelease ArtifactKind = "release"
)

// ArtifactState is the state a task requires of the reported artifact.
type ArtifactState string

const (
	// StateAny accepts either state of the kind.
	StateAny ArtifactState = ""
	// Issue states.
	StateOpen   ArtifactState = "open"
	StateClosed ArtifactState = "closed"
	// Release states. A draft is not public; a published release is.
	StateDraft     ArtifactState = "draft"
	StatePublished ArtifactState = "published"
)

// ArtifactOutcome separates "checked and good", "checked and bad" and "could
// not check". Only ArtifactVerified may earn delivery credit.
type ArtifactOutcome string

const (
	ArtifactVerified   ArtifactOutcome = "verified"
	ArtifactRejected   ArtifactOutcome = "rejected"
	ArtifactUnverified ArtifactOutcome = "unverified"
)

// ArtifactRef is a reported artifact URL after parsing.
type ArtifactRef struct {
	Kind   ArtifactKind
	Owner  string
	Repo   string
	Number int    // issue number
	Tag    string // release tag (also set for "untagged-…" drafts)
	ID     int64  // release id, when the URL is the numeric form
}

var (
	issueURLPattern      = regexp.MustCompile(`^https?://[^/\s]+/([^/\s]+)/([^/\s]+)/issues/(\d+)/?(?:[?#].*)?$`)
	releaseTagURLPattern = regexp.MustCompile(`^https?://[^/\s]+/([^/\s]+)/([^/\s]+)/releases/tag/([^/\s?#]+)/?(?:[?#].*)?$`)
)

// ParseArtifactURL parses a reported URL as the given kind. It is anchored on
// the whole URL (not a prose search) and strict about the path segment, so a
// /pull/N URL never parses as an issue and an issue URL never as a release.
func ParseArtifactURL(kind ArtifactKind, raw string) (ArtifactRef, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ArtifactRef{}, errors.New("empty artifact URL")
	}
	switch kind {
	case ArtifactIssue:
		m := issueURLPattern.FindStringSubmatch(s)
		if m == nil {
			return ArtifactRef{}, fmt.Errorf("not a GitHub issue URL: %q", raw)
		}
		n, err := strconv.Atoi(m[3])
		if err != nil || n <= 0 {
			return ArtifactRef{}, fmt.Errorf("invalid issue number in %q", raw)
		}
		return ArtifactRef{Kind: kind, Owner: m[1], Repo: m[2], Number: n}, nil
	case ArtifactRelease:
		m := releaseTagURLPattern.FindStringSubmatch(s)
		if m == nil {
			return ArtifactRef{}, fmt.Errorf("not a GitHub release URL: %q", raw)
		}
		tag, err := url.PathUnescape(m[3])
		if err != nil || tag == "" {
			return ArtifactRef{}, fmt.Errorf("invalid release tag in %q", raw)
		}
		return ArtifactRef{Kind: kind, Owner: m[1], Repo: m[2], Tag: tag}, nil
	}
	return ArtifactRef{}, fmt.Errorf("unsupported artifact kind %q", kind)
}

// ArtifactAcceptance is a task's declaration of which artifact is sufficient.
// A zero value accepts nothing.
type ArtifactAcceptance struct {
	Kind  ArtifactKind
	State ArtifactState
}

// ArtifactVerification is the result of VerifyReportedArtifact.
type ArtifactVerification struct {
	Outcome ArtifactOutcome
	Reason  string
	// Err is set when the outcome is Unverified because GitHub could not be
	// consulted (rate limit, permission, 5xx, network, no client).
	Err error

	Kind ArtifactKind
	// State is the observed state (open/closed, draft/published).
	State ArtifactState
	// Key is a canonical identity (kind, canonical repo, number or release id),
	// stable across URL casing and repository renames, for duplicate detection.
	Key string
	// Repo is the canonical "owner/repo" GitHub resolved the artifact to.
	Repo       string
	Author     string
	Title      string
	Prerelease bool
}

// Verified reports whether the artifact exists, is bound to the assigned
// repository and contributor, and is of the requested kind.
func (v ArtifactVerification) Verified() bool { return v.Outcome == ArtifactVerified }

// Satisfies reports whether this verification meets a task's acceptance:
// verified, same kind, and the required state (StateAny accepts any). It says
// nothing about code delivery.
func (v ArtifactVerification) Satisfies(a ArtifactAcceptance) bool {
	if !v.Verified() || a.Kind == "" || v.Kind != a.Kind {
		return false
	}
	return a.State == StateAny || a.State == v.State
}

func unavailable(kind ArtifactKind, reason string, err error) ArtifactVerification {
	return ArtifactVerification{Outcome: ArtifactUnverified, Kind: kind, Reason: reason, Err: err}
}

// classifyLookupErr maps a GitHub error to Rejected (the object is definitively
// absent: 404/410) or Unverified (we could not tell: everything else).
func classifyLookupErr(kind ArtifactKind, err error) ArtifactVerification {
	var er *gh.ErrorResponse
	if errors.As(err, &er) && er.Response != nil {
		switch er.Response.StatusCode {
		case http.StatusNotFound, http.StatusGone:
			return ArtifactVerification{Outcome: ArtifactRejected, Kind: kind,
				Reason: "artifact not found (or not visible to hive)", Err: err}
		}
	}
	return unavailable(kind, "github lookup failed", err)
}

// VerifyReportedArtifact checks a client-reported issue or release URL against
// GitHub. It confirms the object exists, is of the requested kind (an issue
// URL that is really a pull request is rejected), lives in expectedRepo
// (compared against the canonical repo GitHub returns, so a renamed repository
// still matches the assignment while a foreign repo never does), and was
// created by expectedAuthor. The observed State is returned for the caller to
// compare with the task's ArtifactAcceptance; verification itself does not
// require a particular state. API failures other than a definitive 404/410 are
// Unverified and must not earn delivery credit.
func (c *Client) VerifyReportedArtifact(ctx context.Context, kind ArtifactKind, expectedRepo, reportedURL, expectedAuthor string) ArtifactVerification {
	if c == nil || c.client == nil {
		return unavailable(kind, "no github client configured", ErrNoGitHubClient)
	}
	ref, err := ParseArtifactURL(kind, reportedURL)
	if err != nil {
		return ArtifactVerification{Outcome: ArtifactRejected, Kind: kind, Reason: "unparseable artifact URL: " + err.Error()}
	}
	switch kind {
	case ArtifactIssue:
		return c.verifyIssue(ctx, ref, expectedRepo, expectedAuthor)
	default:
		return c.verifyRelease(ctx, ref, expectedRepo, expectedAuthor)
	}
}

func bindArtifact(v ArtifactVerification, canonicalRepo, expectedRepo, author, expectedAuthor string) ArtifactVerification {
	if !prBaseRepoMatches(canonicalRepo, expectedRepo) {
		v.Outcome = ArtifactRejected
		v.Reason = fmt.Sprintf("artifact repo %q does not match assigned repo %q", canonicalRepo, expectedRepo)
		return v
	}
	if expectedAuthor == "" || author == "" || !strings.EqualFold(author, expectedAuthor) {
		v.Outcome = ArtifactRejected
		v.Reason = fmt.Sprintf("artifact author %q does not match contributor %q", author, expectedAuthor)
		return v
	}
	v.Outcome = ArtifactVerified
	return v
}

func (c *Client) verifyIssue(ctx context.Context, ref ArtifactRef, expectedRepo, expectedAuthor string) ArtifactVerification {
	issue, _, err := c.client.Issues.Get(ctx, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return classifyLookupErr(ArtifactIssue, err)
	}
	if issue == nil {
		return unavailable(ArtifactIssue, "github returned no issue", nil)
	}
	if issue.IsPullRequest() {
		return ArtifactVerification{Outcome: ArtifactRejected, Kind: ArtifactIssue,
			Reason: "reported issue URL is a pull request"}
	}
	canonical := canonicalRepoFromIssue(issue, ref)
	author := safeGetLogin(issue.GetUser())
	v := ArtifactVerification{
		Kind: ArtifactIssue, Repo: canonical, Author: author, Title: issue.GetTitle(),
		State: ArtifactState(strings.ToLower(issue.GetState())),
		Key:   fmt.Sprintf("issue:%s#%d", strings.ToLower(canonical), issue.GetNumber()),
	}
	return bindArtifact(v, canonical, expectedRepo, author, expectedAuthor)
}

// canonicalRepoFromIssue prefers the repository GitHub says owns the issue
// (follows renames/transfers) over the URL the contributor typed.
func canonicalRepoFromIssue(issue *gh.Issue, ref ArtifactRef) string {
	if full := issue.GetRepository().GetFullName(); full != "" {
		return full
	}
	if ru := issue.GetRepositoryURL(); ru != "" {
		if u, err := url.Parse(ru); err == nil {
			if o, r := splitRepo(strings.TrimPrefix(u.Path, "/repos/")); o != "" && r != "" {
				return o + "/" + r
			}
		}
	}
	return ref.Owner + "/" + ref.Repo
}

const releaseListPages = 3

func (c *Client) verifyRelease(ctx context.Context, ref ArtifactRef, expectedRepo, expectedAuthor string) ArtifactVerification {
	rel, _, err := c.client.Repositories.GetReleaseByTag(ctx, ref.Owner, ref.Repo, ref.Tag)
	if err != nil {
		var er *gh.ErrorResponse
		if !(errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusNotFound) {
			return classifyLookupErr(ArtifactRelease, err)
		}
		// GetReleaseByTag never returns drafts; look through recent releases.
		rel, err = c.findRelease(ctx, ref)
		if err != nil {
			return classifyLookupErr(ArtifactRelease, err)
		}
		if rel == nil {
			return ArtifactVerification{Outcome: ArtifactRejected, Kind: ArtifactRelease,
				Reason: "release not found (or not visible to hive)"}
		}
	}
	canonical := ref.Owner + "/" + ref.Repo
	if u, perr := url.Parse(rel.GetHTMLURL()); perr == nil {
		if m := releaseTagURLPattern.FindStringSubmatch("https://x" + u.Path); m != nil {
			canonical = m[1] + "/" + m[2]
		}
	}
	state := StatePublished
	if rel.GetDraft() {
		state = StateDraft
	}
	author := safeGetLogin(rel.GetAuthor())
	v := ArtifactVerification{
		Kind: ArtifactRelease, Repo: canonical, Author: author, Title: rel.GetName(),
		State: state, Prerelease: rel.GetPrerelease(),
		Key: fmt.Sprintf("release:%s#%d", strings.ToLower(canonical), rel.GetID()),
	}
	return bindArtifact(v, canonical, expectedRepo, author, expectedAuthor)
}

func (c *Client) findRelease(ctx context.Context, ref ArtifactRef) (*gh.RepositoryRelease, error) {
	opts := &gh.ListOptions{PerPage: 100}
	for page := 0; page < releaseListPages; page++ {
		rels, resp, err := c.client.Repositories.ListReleases(ctx, ref.Owner, ref.Repo, opts)
		if err != nil {
			return nil, err
		}
		for _, r := range rels {
			if r.GetTagName() == ref.Tag || strings.HasSuffix(r.GetHTMLURL(), "/releases/tag/"+ref.Tag) {
				return r, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return nil, nil
}
