package hivetool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/pkg/ioscan"
)

// Write tools (RFC #18464 ModeToolHosted): the model holds no credential;
// Hive applies its patches to a Hive-owned branch through the Git Data API
// and opens a DRAFT pull request. Every guard runs here, independently of
// toolapprove: agent mode must be push-capable, the branch is pinned by the
// executor, pushbroker's protected paths are refused, sizes are bounded,
// and any argument carrying the session canary is refused.
const (
	ToolRepoPatch = "repo.patch_files"
	ToolPRCreate  = "pr.create"

	MaxPatchFiles = 20
	MaxPatchBytes = 256 << 10
)

// Comparer is optionally implemented by a Writer: it renders the diff
// between two refs, so a resumed session can be shown what its work branch
// already holds.
type Comparer interface {
	Compare(ctx context.Context, repo, base, head string) (string, error)
}

// ErrRefNotFound is returned by Writer.RefSHA for a missing branch.
var ErrRefNotFound = errors.New("hivetool: ref not found")

// Verifier checks a candidate change before it is committed: it receives the
// full new contents of every touched file and reports whether the project
// still passes (e.g. builds and tests in a scratch copy of a checkout). The
// report is returned to the model, so a failing change can be corrected.
type Verifier interface {
	Verify(ctx context.Context, files map[string]string) (report string, ok bool, err error)
}

// MinRetainedRatio is the share of an existing file's non-blank lines a
// whole-file content write must keep. Live: a free model "fixed" a one-line
// bug by rewriting the file and deleted an unrelated function.
const MinRetainedRatio = 0.7

// Writer is the repository write surface (GitHub Git Data + Pulls API).
type Writer interface {
	RefSHA(ctx context.Context, repo, branch string) (string, error)
	CreateCommit(ctx context.Context, repo, parentSHA, message string, files map[string]string) (string, error)
	SetBranch(ctx context.Context, repo, branch, sha string, create bool) error
	OpenPR(ctx context.Context, repo, head, base, title, body string) (int, string, error)
}

type patchSpec struct {
	Path    string  `json:"path"`
	Search  *string `json:"search,omitempty"`
	Replace *string `json:"replace,omitempty"`
	Content *string `json:"content,omitempty"`
}

func (x *Executor) writeGuard() error {
	if x.Writer == nil || x.WorkBranch == "" {
		return errors.New("writes are not enabled for this session")
	}
	if !x.Mode.CanPush() {
		return fmt.Errorf("agent mode %s may not push or open PRs", x.Mode)
	}
	return nil
}

func (x *Executor) carriesCanary(raw string) bool {
	return x.Canary != "" && strings.Contains(raw, x.Canary)
}

func (x *Executor) repoPatch(ctx context.Context, raw string) (string, error) {
	if err := x.writeGuard(); err != nil {
		return "", err
	}
	if x.carriesCanary(raw) {
		return "", errors.New("refused: the patch carries the session security canary")
	}
	var args struct {
		CommitMessage string      `json:"commit_message"`
		Patches       []patchSpec `json:"patches"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", fmt.Errorf("arguments: %v", err)
	}
	if len(args.Patches) == 0 || len(args.Patches) > MaxPatchFiles {
		return "", fmt.Errorf("patches must hold 1..%d entries", MaxPatchFiles)
	}
	msg := strings.TrimSpace(args.CommitMessage)
	if msg == "" {
		return "", errors.New("commit_message is required")
	}
	// Normalise paths and refuse protected ones before touching anything.
	paths := make([]string, 0, len(args.Patches))
	for i := range args.Patches {
		p, err := cleanPath(args.Patches[i].Path)
		if err != nil {
			return "", err
		}
		if p == "" {
			return "", fmt.Errorf("patch %d: path is required", i)
		}
		args.Patches[i].Path = p
		paths = append(paths, p)
	}
	protected := x.ProtectedPaths
	if len(protected) == 0 {
		protected = pushbroker.DefaultProtectedPaths
	}
	if bad := pushbroker.ProtectedPathViolations(paths, protected); len(bad) > 0 {
		return "", fmt.Errorf("refused: protected paths need a human: %s", strings.Join(bad, ", "))
	}

	head, create, err := x.workHead(ctx)
	if err != nil {
		return "", err
	}
	files := map[string]string{}
	total := 0
	for i, p := range args.Patches {
		cur, have := files[p.Path]
		if !have {
			b, err := x.Reader.File(ctx, x.Repo, head, p.Path)
			switch {
			case err == nil:
				cur = string(b)
			case p.Content != nil && strings.Contains(err.Error(), "not found"):
				cur = "" // new file
			default:
				return "", fmt.Errorf("patch %d (%s): %v", i, p.Path, err)
			}
		}
		switch {
		case p.Content != nil:
			if have || cur != "" {
				if kept, total := retained(cur, *p.Content); total >= 8 && float64(kept) < MinRetainedRatio*float64(total) {
					return "", fmt.Errorf("patch %d (%s): a whole-file write would drop %d of %d existing lines; use search/replace for the lines you mean to change", i, p.Path, total-kept, total)
				}
			}
			cur = *p.Content
		case p.Search != nil && p.Replace != nil:
			if *p.Search == "" {
				return "", fmt.Errorf("patch %d (%s): search must not be empty", i, p.Path)
			}
			n := strings.Count(cur, *p.Search)
			if n != 1 {
				return "", fmt.Errorf("patch %d (%s): search text matches %d times; it must match exactly once (read the file and copy the exact lines)", i, p.Path, n)
			}
			cur = strings.Replace(cur, *p.Search, *p.Replace, 1)
		default:
			return "", fmt.Errorf("patch %d (%s): give either search+replace or content", i, p.Path)
		}
		files[p.Path] = cur
	}
	for _, c := range files {
		total += len(c)
	}
	if total > MaxPatchBytes {
		return "", fmt.Errorf("patched files total %d bytes, over the %d limit", total, MaxPatchBytes)
	}
	if x.Verifier != nil {
		report, ok, err := x.Verifier.Verify(ctx, files)
		if err != nil {
			return "", fmt.Errorf("verification could not run: %w", err)
		}
		if !ok {
			return "", fmt.Errorf("verification failed; nothing was committed. Fix the change and patch again:\n%s", report)
		}
		x.mu.Lock()
		x.verified = append(x.verified, verifyRecord{report: report})
		x.mu.Unlock()
	}
	sha, err := x.Writer.CreateCommit(ctx, x.Repo, head, msg, files)
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	if err := x.Writer.SetBranch(ctx, x.Repo, x.WorkBranch, sha, create); err != nil {
		return "", fmt.Errorf("update %s: %w", x.WorkBranch, err)
	}
	x.mu.Lock()
	if n := len(x.verified); n > 0 && x.verified[n-1].sha == "" {
		x.verified[n-1].sha = sha
	}
	x.headSHA = sha
	for p, c := range files {
		x.recordEvidence(p, 1, strings.Count(c, "\n")+1)
	}
	x.mu.Unlock()
	changed := make([]string, 0, len(files))
	for p := range files {
		changed = append(changed, p)
	}
	sort.Strings(changed)
	return fmt.Sprintf("committed %s on %s: %s. Next: pr.create (draft) or more repo.patch_files.", short(sha), x.WorkBranch, strings.Join(changed, ", ")), nil
}

// workHead returns the commit to patch on top of: the work branch head when
// it exists (so successive patches stack), else the base ref.
func (x *Executor) workHead(ctx context.Context) (string, bool, error) {
	x.mu.Lock()
	cached := x.headSHA
	x.mu.Unlock()
	if cached != "" {
		return cached, false, nil
	}
	sha, err := x.Writer.RefSHA(ctx, x.Repo, x.WorkBranch)
	if err == nil {
		x.mu.Lock()
		x.headSHA = sha
		x.mu.Unlock()
		return sha, false, nil
	}
	if !errors.Is(err, ErrRefNotFound) {
		return "", false, err
	}
	base, err := x.Writer.RefSHA(ctx, x.Repo, x.Ref)
	if err != nil {
		return "", false, fmt.Errorf("base %s: %w", x.Ref, err)
	}
	return base, true, nil
}

func (x *Executor) prCreate(ctx context.Context, raw string) (string, error) {
	if err := x.writeGuard(); err != nil {
		return "", err
	}
	if x.carriesCanary(raw) {
		return "", errors.New("refused: the pull request carries the session security canary")
	}
	var args struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", fmt.Errorf("arguments: %v", err)
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		return "", errors.New("title is required")
	}
	x.mu.Lock()
	head, opened := x.headSHA, x.prNumber
	x.mu.Unlock()
	if head == "" {
		return "", errors.New("no commits yet: call repo.patch_files first")
	}
	if opened != 0 {
		return "", fmt.Errorf("pull request #%d is already open for this session", opened)
	}
	body, v := ioscan.EnforceOutput(strings.TrimSpace(args.Body))
	if v.Blocked {
		return "", errors.New("refused: the PR body tripped the ioscan output policy")
	}
	// A free-tier draft never auto-closes an issue: closing keywords the
	// model wrote become plain references; a human decides on merge.
	body = closingKeywordRe.ReplaceAllString(body, "Refs ${2}")
	if x.IssueRef > 0 {
		body += fmt.Sprintf("\n\nRefs #%d", x.IssueRef)
	}
	// Hive's own record of what was verified, so claims in the model's prose
	// are distinguishable from checks that actually ran.
	x.mu.Lock()
	records := append([]verifyRecord(nil), x.verified...)
	x.mu.Unlock()
	if len(records) > 0 {
		body += "\n\n<details><summary>Hive verification (ran before each commit)</summary>\n\n"
		for _, r := range records {
			body += fmt.Sprintf("`%s`: passed\n```\n%s\n```\n", short(r.sha), strings.TrimSpace(tailLines(r.report, 15)))
		}
		body += "</details>"
	} else if x.Verifier == nil {
		body += "\n\n> ⚠️ No build or test verification ran for this draft; any verification claims above are the model's own."
	}
	body += "\n\n---\n<sub>Draft opened by hive-lite (tool-hosted: the model held no credentials; Hive applied its patches). Review before marking ready.</sub>"
	n, url, err := x.Writer.OpenPR(ctx, x.Repo, x.WorkBranch, x.Ref, title, body)
	if err != nil {
		return "", err
	}
	x.mu.Lock()
	x.prNumber = n
	x.mu.Unlock()
	return fmt.Sprintf("opened draft PR #%d %s. Now call task.complete with pr_number %d.", n, url, n), nil
}

type verifyRecord struct{ sha, report string }

// closingKeywordRe matches GitHub's issue-closing keywords before a
// reference (#N, owner/repo#N, or an issue URL).
var closingKeywordRe = regexp.MustCompile(`(?i)\b(close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+((?:[\w.-]+/[\w.-]+)?#\d+|https://github\.com/[\w.-]+/[\w.-]+/issues/\d+)`)

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{"…"}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}

// retained counts how many of old's non-blank lines survive in new.
func retained(old, new string) (kept, total int) {
	have := map[string]int{}
	for _, l := range strings.Split(new, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			have[t]++
		}
	}
	for _, l := range strings.Split(old, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		total++
		if have[t] > 0 {
			have[t]--
			kept++
		}
	}
	return kept, total
}

// readRef is the ref reads resolve against. In a write session whose work
// branch exists (earlier commits this session, or a resumed session), reads
// must see the branch head: live, a resumed session read the base, kept
// patching text the branch had already changed, and looped.
func (x *Executor) readRef(ctx context.Context) string {
	if x.Writer == nil || x.WorkBranch == "" || !x.Mode.CanPush() {
		return x.Ref
	}
	if head, _, err := x.workHead(ctx); err == nil {
		x.mu.Lock()
		cached := x.headSHA
		x.mu.Unlock()
		if cached != "" {
			return cached
		}
		_ = head // branch absent: head is the base commit; reading Ref is equivalent
	}
	return x.Ref
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// PRNumber reports the draft PR this session opened, if any.
func (x *Executor) PRNumber() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.prNumber
}
