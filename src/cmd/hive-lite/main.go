// Command hive-lite runs one CLI-less, report-only triage of a GitHub issue
// on free-tier inference (RFC hivecommons/hive#18464, Family C). It needs no
// tmux, no agent CLI, no privileged container and no persistent volume: a
// read-only GitHub token, a gateway list and (optionally) a checkout for
// code search.
//
//	hive-lite -repo owner/name -issue 123 -gateways gateways.yaml [-checkout .]
//
// Scope per the maintainer decision on #18464 (phase 1): hive-lite is a
// default-off, report-only advisory engine, run in shadow. It reads,
// triages, and prints the advisory and a JSON result to stdout. It never
// writes to GitHub: no comments, labels, branches, commits or pull
// requests, and its GitHub calls are all GETs. Any GitHub write belongs to
// the Hive App. The only state it can write is an optional -state d1:...
// checkpoint store outside GitHub.
//
// gateways.yaml is an ordered failover list; each entry is a Hive Model
// Gateway (same fields as governor.gateways in hive.yaml) plus an optional
// model override, coordinator-side daily request cap, and whether the
// provider's free tier trains on inputs:
//
//   - name: workers-ai-glm
//     kind: custom
//     endpoint: https://api.cloudflare.com/client/v4/accounts/<id>/ai/v1
//     api_key_env: CLOUDFLARE_API_TOKEN
//     default_model: "@cf/zai-org/glm-4.7-flash"
//     daily_cap: 150
//     max_tokens: 4096        # optional; some free endpoints default low
//     trains_on_inputs: false # true skips this gateway for private repos
//
// The file may instead be a mapping with the list under "gateways" and the
// opt-in "allow_training_providers_for_private: true", which lets gateways
// marked trains_on_inputs serve a private repository. Without that opt-in
// (or the flag of the same name) they are skipped for private repos, and a
// repository whose visibility cannot be read is treated as private.
package main

import (
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
	"github.com/hivecommons/hive/pkg/coordstate"
	"github.com/hivecommons/hive/pkg/hivelite"
	"github.com/hivecommons/hive/pkg/hivetool"
	"github.com/hivecommons/hive/pkg/turn"
	"github.com/hivecommons/hive/pkg/turn/llmrouter"
)

type gatewayEntry struct {
	cruxconfig.GatewayConfig `yaml:",inline"`
	Model                    string `yaml:"model"`
	DailyCap                 int    `yaml:"daily_cap"`
	MaxTokens                int    `yaml:"max_tokens"`
	TrainsOnInputs           bool   `yaml:"trains_on_inputs"`
}

// gatewayFile is the mapping form of gateways.yaml.
type gatewayFile struct {
	AllowTrainingForPrivate bool           `yaml:"allow_training_providers_for_private"`
	Gateways                []gatewayEntry `yaml:"gateways"`
}

func main() {
	repo := flag.String("repo", "", "owner/name")
	issue := flag.Int("issue", 0, "issue number")
	ref := flag.String("ref", "", "ref to read (default: the repository's default branch)")
	gatewaysPath := flag.String("gateways", "", "YAML list of gateways (failover order)")
	checkout := flag.String("checkout", "", "local checkout of -repo for code search (pkg/codeindex); empty disables search")
	allowTraining := flag.Bool("allow-training-providers-for-private", false, "let gateways marked trains_on_inputs serve a private repository (default: they are skipped)")
	maxTurns := flag.Int("max-turns", 14, "session turn budget")
	finishAfter := flag.Int("finish-after", 8, "turn at which only task.complete/task.abort are offered")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall deadline")
	verbose := flag.Bool("v", false, "log each step to stderr")
	state := flag.String("state", "", "checkpoint store: empty or \"memory\" keeps state in this process; \"d1:<account-id>/<database-id>\" (CLOUDFLARE_API_TOKEN with D1 write) lets a run paused for capacity resume and persists the router ledger. A GitHub-branch store is not selectable: it would be a GitHub write")
	flag.Parse()
	if err := run(*repo, *issue, *ref, *gatewaysPath, *checkout, *state, *allowTraining, *maxTurns, *finishAfter, *timeout, *verbose); err != nil {
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

func run(repo string, issue int, ref, gatewaysPath, checkout, stateSpec string, allowTraining bool, maxTurns, finishAfter int, timeout time.Duration, verbose bool) error {
	if repo == "" || issue <= 0 || gatewaysPath == "" {
		return errors.New("-repo, -issue and -gateways are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tok := githubToken()
	gh := &githubClient{token: tok}
	info, err := gh.repoInfo(ctx, repo)
	if err != nil {
		if ref == "" {
			return err
		}
		// Visibility unknown: fail safe and treat the repository as private.
		fmt.Fprintln(os.Stderr, "hive-lite: repository visibility unknown, treating it as private:", err)
		info.Private = true
	}
	if ref == "" {
		ref = info.DefaultBranch
	}

	providers, fileAllow, err := loadProviders(gatewaysPath)
	if err != nil {
		return err
	}
	allowTraining = allowTraining || fileAllow
	served := map[string]int{}
	hook := func(a llmrouter.Attempt) {
		if a.Outcome == llmrouter.OutcomeServed {
			served[a.Provider]++
		}
		if verbose {
			fmt.Fprintf(os.Stderr, "  provider %s: %s %d %s\n", a.Provider, a.Outcome, a.Status, clip(a.Detail, 100))
		}
	}
	// One ledger for both routers so cooldowns and caps are shared. With a
	// state store it is loaded from and flushed back to the store, so daily
	// caps and cooldowns hold across passes.
	var ledger llmrouter.Ledger = llmrouter.NewMemoryLedger()
	store, err := coordstate.Open(stateSpec, cfToken())
	if err != nil {
		return err
	}
	if store != nil {
		sl, err := coordstate.LoadLedger(ctx, store)
		if err != nil {
			return fmt.Errorf("load ledger: %w", err)
		}
		defer func() {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := sl.Flush(fctx); err != nil {
				fmt.Fprintln(os.Stderr, "hive-lite: flush ledger:", err)
			}
		}()
		ledger = sl
	}
	privacy := []llmrouter.Option{llmrouter.WithPrivateRepo(info.Private), llmrouter.WithAllowTrainingProvidersForPrivate(allowTraining)}
	explore, err := llmrouter.New(providers, append(privacy, llmrouter.WithLedger(ledger), llmrouter.WithAttemptHook(hook))...)
	if err != nil {
		return err
	}
	finish, err := llmrouter.New(providers, append(privacy, llmrouter.WithLedger(ledger), llmrouter.WithAttemptHook(hook), llmrouter.WithToolChoice("required"))...)
	if err != nil {
		return err
	}

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
		State: store,
	}
	if verbose {
		cfg.Log = func(s string) { fmt.Fprintln(os.Stderr, " ", s) }
	}
	res, err := hivelite.RunTriage(ctx, cfg)
	model := modelOf(providers, served)
	shown := res
	if shown.Withheld != "" {
		// A withheld summary (canary echo, output-policy block) is not shown
		// anywhere, including this JSON, which may land in a public log.
		shown.Outcome.Summary, shown.Outcome.Reason = "[withheld]", ""
	}
	out, _ := json.MarshalIndent(struct {
		hivelite.Result
		Model  string         `json:"model"`
		Served map[string]int `json:"served_by"`
	}{shown, model, served}, "", "  ")
	fmt.Println(string(out))
	if err != nil {
		if errors.Is(err, turn.ErrInferenceCapacity) {
			return err
		}
		if errors.Is(err, hivelite.ErrNoResult) {
			fmt.Fprintln(os.Stderr, "hive-lite: no grounded result")
			return nil
		}
		return err
	}
	if res.Withheld != "" {
		fmt.Fprintln(os.Stderr, "hive-lite: advisory withheld:", res.Withheld)
		return nil
	}
	fmt.Println("\n" + hivelite.RenderComment(res, model))
	return nil
}

// loadProviders reads gateways.yaml in either form (a bare list, or a
// mapping with "gateways" and "allow_training_providers_for_private").
func loadProviders(path string) ([]llmrouter.Provider, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	var file gatewayFile
	if len(doc.Content) > 0 {
		switch root := doc.Content[0]; root.Kind {
		case yaml.SequenceNode:
			err = root.Decode(&file.Gateways)
		case yaml.MappingNode:
			err = root.Decode(&file)
		default:
			err = errors.New("want a list of gateways or a mapping with gateways:")
		}
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
	}
	if len(file.Gateways) == 0 {
		return nil, false, fmt.Errorf("%s: no gateways", path)
	}
	out := make([]llmrouter.Provider, 0, len(file.Gateways))
	for _, e := range file.Gateways {
		p, err := llmrouter.FromGateway(e.GatewayConfig, e.Model, e.DailyCap)
		if err != nil {
			return nil, false, err
		}
		p.MaxTokens = e.MaxTokens
		p.TrainsOnInputs = e.TrainsOnInputs
		out = append(out, p)
	}
	return out, file.AllowTrainingForPrivate, nil
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

// ---- minimal read-only GitHub client ----

// githubClient issues GET requests only: hive-lite never writes to GitHub.
type githubClient struct{ token string }

func (g *githubClient) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
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
		return fmt.Errorf("github GET %s: %d %s", path, resp.StatusCode, clip(string(b), 200))
	}
	if v != nil {
		return json.Unmarshal(b, v)
	}
	return nil
}

// repoInfo is the subset of GET /repos/{owner}/{repo} hive-lite needs.
type repoInfo struct {
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Visibility    string `json:"visibility"`
}

func (g *githubClient) repoInfo(ctx context.Context, repo string) (repoInfo, error) {
	var r repoInfo
	if err := g.get(ctx, "/repos/"+repo, &r); err != nil {
		return repoInfo{}, err
	}
	// "internal" (enterprise) repositories are not public either.
	r.Private = r.Private || (r.Visibility != "" && r.Visibility != "public")
	return r, nil
}

// cfToken is the Cloudflare token for a d1: state store.
func cfToken() string {
	for _, k := range []string{"HIVE_STATE_CF_TOKEN", "CLOUDFLARE_API_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
