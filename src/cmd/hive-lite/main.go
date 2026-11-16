// Command hive-lite runs one CLI-less, report-only triage of a GitHub issue
// on free-tier inference (RFC hivecommons/hive#18464, Family C). It needs no
// tmux, no agent CLI, no privileged container and no persistent volume: a
// GitHub token, a gateway list and (optionally) a checkout for code search.
//
//	hive-lite -repo owner/name -issue 123 -gateways gateways.yaml [-checkout .] [-post]
//
// Without -post it prints the advisory and JSON result and writes nothing.
// With -post it upserts one marked advisory comment on the issue, unless the
// output side (canary echo, ioscan output policy) withholds it.
//
// gateways.yaml is an ordered failover list; each entry is a Hive Model
// Gateway (same fields as governor.gateways in hive.yaml) plus an optional
// model override and coordinator-side daily request cap:
//
//   - name: workers-ai-glm
//     kind: custom
//     endpoint: https://api.cloudflare.com/client/v4/accounts/<id>/ai/v1
//     api_key_env: CLOUDFLARE_API_TOKEN
//     default_model: "@cf/zai-org/glm-4.7-flash"
//     daily_cap: 150
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/pkg/codeindex"
	"github.com/hivecommons/hive/pkg/hivelite"
	"github.com/hivecommons/hive/pkg/hivetool"
	"github.com/hivecommons/hive/pkg/turn"
	"github.com/hivecommons/hive/pkg/turn/llmrouter"
)

type gatewayEntry struct {
	cruxconfig.GatewayConfig `yaml:",inline"`
	Model                    string `yaml:"model"`
	DailyCap                 int    `yaml:"daily_cap"`
}

func main() {
	repo := flag.String("repo", "", "owner/name")
	issue := flag.Int("issue", 0, "issue number")
	ref := flag.String("ref", "", "ref to read (default: the repository's default branch)")
	gatewaysPath := flag.String("gateways", "", "YAML list of gateways (failover order)")
	checkout := flag.String("checkout", "", "local checkout of -repo for code search (pkg/codeindex); empty disables search")
	post := flag.Bool("post", false, "upsert the advisory comment on the issue")
	maxTurns := flag.Int("max-turns", 14, "session turn budget")
	finishAfter := flag.Int("finish-after", 8, "turn at which only task.complete/task.abort are offered")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall deadline")
	verbose := flag.Bool("v", false, "log each step to stderr")
	flag.Parse()
	if err := run(*repo, *issue, *ref, *gatewaysPath, *checkout, *post, *maxTurns, *finishAfter, *timeout, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "hive-lite:", err)
		var ce *llmrouter.CapacityError
		if errors.As(err, &ce) {
			os.Exit(75) // EX_TEMPFAIL: no free capacity now; retry on a later pass
		}
		os.Exit(1)
	}
}

func githubToken() string {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func run(repo string, issue int, ref, gatewaysPath, checkout string, post bool, maxTurns, finishAfter int, timeout time.Duration, verbose bool) error {
	if repo == "" || issue <= 0 || gatewaysPath == "" {
		return errors.New("-repo, -issue and -gateways are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tok := githubToken()
	gh := &githubClient{token: tok}
	if ref == "" {
		var err error
		if ref, err = gh.defaultBranch(ctx, repo); err != nil {
			return err
		}
	}

	providers, err := loadProviders(gatewaysPath)
	if err != nil {
		return err
	}
	served := map[string]int{}
	hook := func(a llmrouter.Attempt) {
		if a.Outcome == llmrouter.OutcomeServed {
			served[a.Provider]++
		}
		if verbose {
			fmt.Fprintf(os.Stderr, "  provider %s: %s %d %s\n", a.Provider, a.Outcome, a.Status, clip(a.Detail, 100))
		}
	}
	// One ledger for both routers so cooldowns and caps are shared.
	ledger := llmrouter.NewMemoryLedger()
	explore, err := llmrouter.New(providers, llmrouter.WithLedger(ledger), llmrouter.WithAttemptHook(hook))
	if err != nil {
		return err
	}
	finish, _ := llmrouter.New(providers, llmrouter.WithLedger(ledger), llmrouter.WithAttemptHook(hook), llmrouter.WithToolChoice("required"))

	var searcher hivetool.CodeSearcher
	if checkout != "" {
		idx, errs := codeindex.Build(ctx, []codeindex.Repo{{Name: repo, Root: checkout}})
		for name, e := range errs {
			fmt.Fprintf(os.Stderr, "hive-lite: codeindex %s: %v\n", name, e)
		}
		searcher = hivetool.CodeIndexSearcher{Index: func() *codeindex.Index { return idx }}
	}

	cfg := hivelite.Config{
		Repo: repo, Ref: ref, Issue: issue,
		LLM: explore, FinishLLM: finish,
		Reader:   &hivetool.RESTReader{Token: func(context.Context) (string, error) { return tok, nil }},
		Searcher: searcher,
		MaxTurns: maxTurns, FinishAfter: finishAfter,
	}
	if verbose {
		cfg.Log = func(s string) { fmt.Fprintln(os.Stderr, " ", s) }
	}
	res, err := hivelite.RunTriage(ctx, cfg)
	model := modelOf(providers, served)
	out, _ := json.MarshalIndent(struct {
		hivelite.Result
		Model  string         `json:"model"`
		Served map[string]int `json:"served_by"`
	}{res, model, served}, "", "  ")
	fmt.Println(string(out))
	if err != nil {
		if errors.Is(err, turn.ErrInferenceCapacity) {
			return err
		}
		if errors.Is(err, hivelite.ErrNoResult) {
			fmt.Fprintln(os.Stderr, "hive-lite: no grounded result; nothing to post")
			return nil
		}
		return err
	}
	comment := hivelite.RenderComment(res, model)
	fmt.Println("\n" + comment)
	if !post {
		return nil
	}
	if res.Withheld != "" {
		return fmt.Errorf("not posting: %s", res.Withheld)
	}
	if tok == "" {
		return errors.New("-post needs GH_TOKEN or GITHUB_TOKEN")
	}
	url, err := gh.upsertComment(ctx, repo, issue, hivelite.CommentMarker, comment)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "hive-lite: posted", url)
	return nil
}

func loadProviders(path string) ([]llmrouter.Provider, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []gatewayEntry
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s: no gateways", path)
	}
	out := make([]llmrouter.Provider, 0, len(entries))
	for _, e := range entries {
		p, err := llmrouter.FromGateway(e.GatewayConfig, e.Model, e.DailyCap)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func modelOf(ps []llmrouter.Provider, served map[string]int) string {
	best, n := "", -1
	for _, p := range ps {
		if served[p.Name] > n {
			best, n = p.Name+" ("+p.Model+")", served[p.Name]
		}
	}
	return best
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- minimal GitHub client for the two calls the CLI makes ----

type githubClient struct{ token string }

func (g *githubClient) do(ctx context.Context, method, path string, body any, v any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github %s %s: %d %s", method, path, resp.StatusCode, clip(string(b), 200))
	}
	if v != nil {
		return json.Unmarshal(b, v)
	}
	return nil
}

func (g *githubClient) defaultBranch(ctx context.Context, repo string) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.do(ctx, http.MethodGet, "/repos/"+repo, nil, &r); err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

func (g *githubClient) upsertComment(ctx context.Context, repo string, issue int, marker, body string) (string, error) {
	var comments []struct {
		ID      int64  `json:"id"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	}
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, issue), nil, &comments); err != nil {
		return "", err
	}
	var res struct {
		HTMLURL string `json:"html_url"`
	}
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			err := g.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, c.ID), map[string]string{"body": body}, &res)
			return res.HTMLURL, err
		}
	}
	err := g.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, issue), map[string]string{"body": body}, &res)
	return res.HTMLURL, err
}
