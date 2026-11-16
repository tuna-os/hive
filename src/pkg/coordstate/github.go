package coordstate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ContentsStore keeps each key as one file on a dedicated state branch,
// using the GitHub Contents API. The blob sha is the Version: PUT with a
// stale sha fails with 409, which is the per-file CAS the RFC requires.
//
// The branch inherits the repository's visibility. On a public repository
// every value is public, so callers must only store what may be published
// (hive-lite strips its canary before checkpointing).
type ContentsStore struct {
	Repo    string // owner/name
	Branch  string // e.g. "hive-lite-state"; created on first write
	Prefix  string // optional directory inside the branch
	Token   func(context.Context) (string, error)
	BaseURL string // default https://api.github.com
	HTTP    *http.Client
	// Author is the commit message prefix; defaults to "hive-lite state".
	Author string
	// BranchRetries bounds retries after a 409 caused by a concurrent write
	// to another file on the branch (default 5); Backoff is the base delay
	// (default 150ms, linear with jitter).
	BranchRetries int
	Backoff       time.Duration
}

func (s *ContentsStore) conflictRetries() int {
	if s.BranchRetries > 0 {
		return s.BranchRetries
	}
	return 5
}

func (s *ContentsStore) backoff(ctx context.Context, attempt int) error {
	base := s.Backoff
	if base <= 0 {
		base = 150 * time.Millisecond
	}
	d := base*time.Duration(attempt+1) + time.Duration(rand.Int64N(int64(base)))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var _ Store = (*ContentsStore)(nil)

func (s *ContentsStore) base() string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	return "https://api.github.com"
}

func (s *ContentsStore) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *ContentsStore) path(key string) string {
	if s.Prefix == "" {
		return key
	}
	return strings.Trim(s.Prefix, "/") + "/" + key
}

func (s *ContentsStore) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base()+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.Token != nil {
		tok, err := s.Token(ctx)
		if err != nil {
			return 0, nil, err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, data, err
}

func (s *ContentsStore) contentsPath(key string) string {
	segs := strings.Split(s.path(key), "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return "/repos/" + s.Repo + "/contents/" + strings.Join(segs, "/")
}

// Get implements Store.
func (s *ContentsStore) Get(ctx context.Context, key string) ([]byte, Version, error) {
	if err := ValidKey(key); err != nil {
		return nil, "", err
	}
	code, data, err := s.do(ctx, http.MethodGet, s.contentsPath(key)+"?ref="+url.QueryEscape(s.Branch), nil)
	if err != nil {
		return nil, "", err
	}
	if code == http.StatusNotFound {
		return nil, "", ErrNotFound // missing file or missing branch
	}
	if code != http.StatusOK {
		return nil, "", apiError("get "+key, code, data)
	}
	var f struct {
		Type     string `json:"type"`
		SHA      string `json:"sha"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, "", fmt.Errorf("coordstate: get %s: %w", key, err)
	}
	if f.Type != "file" || f.Encoding != "base64" {
		return nil, "", fmt.Errorf("coordstate: get %s: not a base64 file (type %q, encoding %q)", key, f.Type, f.Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	if err != nil {
		return nil, "", fmt.Errorf("coordstate: get %s: %w", key, err)
	}
	return raw, Version(f.SHA), nil
}

// Put implements Store.
func (s *ContentsStore) Put(ctx context.Context, key string, value []byte, expect Version) (Version, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	author := s.Author
	if author == "" {
		author = "hive-lite state"
	}
	body := map[string]any{
		"message": author + ": " + key,
		"content": base64.StdEncoding.EncodeToString(value),
		"branch":  s.Branch,
	}
	if expect != "" {
		body["sha"] = string(expect)
	}
	bootstrapped := false
	for attempt := 0; ; attempt++ {
		code, data, err := s.do(ctx, http.MethodPut, s.contentsPath(key), body)
		if err != nil {
			return "", err
		}
		switch {
		case code == http.StatusOK || code == http.StatusCreated:
			var out struct {
				Content struct {
					SHA string `json:"sha"`
				} `json:"content"`
			}
			if err := json.Unmarshal(data, &out); err != nil || out.Content.SHA == "" {
				return "", fmt.Errorf("coordstate: put %s: unreadable response", key)
			}
			return Version(out.Content.SHA), nil
		case code == http.StatusConflict:
			// The Contents API serialises commits per branch: a concurrent
			// write to a DIFFERENT file also answers 409 (measured live,
			// two hive-mcp sessions opening at once). Only a changed
			// version of this key is a real conflict; otherwise retry.
			if attempt >= s.conflictRetries() {
				return "", fmt.Errorf("%w: %s (branch busy)", ErrConflict, key)
			}
			_, cur, gerr := s.Get(ctx, key)
			if gerr != nil && !errors.Is(gerr, ErrNotFound) {
				return "", gerr
			}
			if cur != expect {
				return "", fmt.Errorf("%w: %s", ErrConflict, key)
			}
			if err := s.backoff(ctx, attempt); err != nil {
				return "", err
			}
			continue
		case code == http.StatusUnprocessableEntity && expect == "" && bytes.Contains(data, []byte("sha")):
			// Create-only write, but the file exists: someone else created
			// it. GitHub answers `"sha" wasn't supplied` (JSON-escaped).
			return "", fmt.Errorf("%w: %s already exists", ErrConflict, key)
		case code == http.StatusNotFound && !bootstrapped:
			// The state branch does not exist yet.
			if err := s.EnsureBranch(ctx); err != nil {
				return "", err
			}
			bootstrapped = true
			continue
		default:
			return "", apiError("put "+key, code, data)
		}
	}
}

// EnsureBranch creates the state branch as an orphan (no shared history with
// the code) holding a README, unless it already exists.
func (s *ContentsStore) EnsureBranch(ctx context.Context) error {
	code, _, err := s.do(ctx, http.MethodGet, "/repos/"+s.Repo+"/git/ref/heads/"+s.Branch, nil)
	if err != nil {
		return err
	}
	if code == http.StatusOK {
		return nil
	}
	readme := "# " + s.Branch + "\n\nEphemeral coordinator state written by hive-lite (session checkpoints, router ledger). Safe to delete when no session is running; it is not part of the code history.\n"
	code, data, err := s.do(ctx, http.MethodPost, "/repos/"+s.Repo+"/git/trees", map[string]any{
		"tree": []map[string]string{{"path": "README.md", "mode": "100644", "type": "blob", "content": readme}},
	})
	if err != nil {
		return err
	}
	if code != http.StatusCreated {
		return apiError("create state tree", code, data)
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	_ = json.Unmarshal(data, &tree)
	code, data, err = s.do(ctx, http.MethodPost, "/repos/"+s.Repo+"/git/commits", map[string]any{
		"message": "hive-lite: initialise state branch", "tree": tree.SHA, "parents": []string{},
	})
	if err != nil {
		return err
	}
	if code != http.StatusCreated {
		return apiError("create state commit", code, data)
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	_ = json.Unmarshal(data, &commit)
	code, data, err = s.do(ctx, http.MethodPost, "/repos/"+s.Repo+"/git/refs", map[string]any{
		"ref": "refs/heads/" + s.Branch, "sha": commit.SHA,
	})
	if err != nil {
		return err
	}
	if code == http.StatusCreated || code == http.StatusUnprocessableEntity && bytes.Contains(data, []byte("already exists")) {
		return nil
	}
	return apiError("create state branch", code, data)
}

func apiError(op string, code int, data []byte) error {
	msg := strings.TrimSpace(string(data))
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &e) == nil && e.Message != "" {
		msg = e.Message
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return errors.New("coordstate: " + op + ": " + fmt.Sprint(code) + ": " + msg)
}
