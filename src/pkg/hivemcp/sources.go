package hivemcp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/agentmode"
)

// TokenEntry maps one bearer token (stored only as its sha256) to a
// contributor principal.
type TokenEntry struct {
	SHA256 string `json:"sha256" yaml:"sha256"`
	Login  string `json:"login" yaml:"login"`
	// Mode must be empty or ADVISORY: the server is read-only
	// (hivecommons/hive#18464 phase 2), and TokenAuth refuses any other
	// mode rather than silently downgrading it.
	Mode string `json:"mode" yaml:"mode"`
}

// TokenAuth authenticates "Authorization: Bearer <token>" against hashed
// entries. Comparison is constant-time over the hash.
func TokenAuth(entries []TokenEntry) (Authenticator, error) {
	type rec struct {
		sum []byte
		p   Principal
	}
	var recs []rec
	for i, e := range entries {
		sum, err := hex.DecodeString(strings.TrimSpace(e.SHA256))
		if err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("hivemcp: token entry %d: sha256 must be 64 hex characters", i)
		}
		if strings.TrimSpace(e.Login) == "" {
			return nil, fmt.Errorf("hivemcp: token entry %d: login is required", i)
		}
		mode := agentmode.ModeAdvisory
		if e.Mode != "" {
			m, ok := agentmode.ParseAgentMode(e.Mode)
			if !ok {
				return nil, fmt.Errorf("hivemcp: token entry %d: unknown mode %q", i, e.Mode)
			}
			if m != agentmode.ModeAdvisory {
				return nil, fmt.Errorf("hivemcp: token entry %d: mode %s refused; this server is read-only and serves ADVISORY only (hivecommons/hive#18464)", i, m)
			}
			mode = m
		}
		recs = append(recs, rec{sum: sum, p: Principal{Login: e.Login, Mode: mode}})
	}
	return func(r *http.Request) (Principal, bool) {
		h := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || strings.TrimSpace(tok) == "" {
			return Principal{}, false
		}
		sum := sha256.Sum256([]byte(strings.TrimSpace(tok)))
		var found Principal
		hit := 0
		for _, rc := range recs {
			if subtle.ConstantTimeCompare(sum[:], rc.sum) == 1 {
				found, hit = rc.p, 1
			}
		}
		return found, hit == 1
	}, nil
}

// HashToken returns the sha256 hex to put in a TokenEntry.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(tok)))
	return hex.EncodeToString(sum[:])
}

// LabelTasks offers open issues (never pull requests) carrying Label, oldest
// first.
type LabelTasks struct {
	Repo    string
	Label   string
	Token   func(context.Context) (string, error)
	BaseURL string
	HTTP    *http.Client
}

// Next implements TaskSource.
func (l *LabelTasks) Next(ctx context.Context, _ Principal, skip []int) (Task, bool, error) {
	base := l.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	q := url.Values{"state": {"open"}, "labels": {l.Label}, "sort": {"created"}, "direction": {"asc"}, "per_page": {"50"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/repos/"+l.Repo+"/issues?"+q.Encode(), nil)
	if err != nil {
		return Task{}, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if l.Token != nil {
		tok, err := l.Token(ctx)
		if err != nil {
			return Task{}, false, err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	c := l.HTTP
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return Task{}, false, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return Task{}, false, fmt.Errorf("hivemcp: list issues: %d", resp.StatusCode)
	}
	var items []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		PullRequest any    `json:"pull_request"`
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		return Task{}, false, err
	}
	for _, it := range items {
		if it.PullRequest != nil || slices.Contains(skip, it.Number) {
			continue
		}
		t := Task{Issue: it.Number, Title: it.Title}
		for _, lb := range it.Labels {
			t.Labels = append(t.Labels, lb.Name)
		}
		return t, true, nil
	}
	return Task{}, false, nil
}
