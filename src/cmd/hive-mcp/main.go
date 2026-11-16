// Command hive-mcp serves Hive work to MCP-capable agents (RFC
// hivecommons/hive#18464 §3a, Family E, agent-driven mode). A connected
// agent spends its own inference; Hive executes every tool call under a
// tool-hosted admission and holds no model credential.
//
//	hive-mcp -repo owner/name -label hive-lite -tokens tokens.json -checkout ./repo
//
// tokens.json is a list of {"sha256": "<hex of the bearer token>", "login":
// "<contributor>", "mode": "ADVISORY|ISSUES_ONLY|ISSUES_AND_PRS"}. Print a
// hash with `hive-mcp -hash-token <token>`. GH_TOKEN/GITHUB_TOKEN is Hive's
// own credential for reads (and, with -write, the draft-PR writes).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/codeindex"
	"github.com/hivecommons/hive/pkg/coordstate"
	"github.com/hivecommons/hive/pkg/hivemcp"
	"github.com/hivecommons/hive/pkg/hivetool"
)

func main() {
	repo := flag.String("repo", "", "owner/name")
	ref := flag.String("ref", "main", "ref reads resolve against")
	label := flag.String("label", "hive-lite", "issues carrying this label are offered")
	tokens := flag.String("tokens", "", "JSON file of hashed bearer tokens")
	addr := flag.String("addr", "127.0.0.1:8765", "listen address")
	checkout := flag.String("checkout", "", "local checkout for repo_search_code (pkg/codeindex)")
	write := flag.Bool("write", false, "let ISSUES_AND_PRS principals patch a Hive-owned branch and open draft PRs")
	state := flag.String("state", "", "\"github:owner/name@branch\" keeps sessions on a state branch (stateless hosting); empty keeps them in memory")
	hash := flag.String("hash-token", "", "print the sha256 for a token and exit")
	flag.Parse()
	if *hash != "" {
		fmt.Println(hivemcp.HashToken(*hash))
		return
	}
	if err := run(*repo, *ref, *label, *tokens, *addr, *checkout, *state, *write); err != nil {
		log.Fatal("hive-mcp: ", err)
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

func run(repo, ref, label, tokensPath, addr, checkout, stateSpec string, write bool) error {
	if repo == "" || tokensPath == "" {
		return errors.New("-repo and -tokens are required")
	}
	raw, err := os.ReadFile(tokensPath)
	if err != nil {
		return err
	}
	var entries []hivemcp.TokenEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("%s: %w", tokensPath, err)
	}
	auth, err := hivemcp.TokenAuth(entries)
	if err != nil {
		return err
	}
	tok := githubToken()
	tokFn := func(context.Context) (string, error) { return tok, nil }
	cfg := hivemcp.Config{
		Repo: repo, Ref: ref, Auth: auth,
		Reader: &hivetool.RESTReader{Token: tokFn},
		Tasks:  &hivemcp.LabelTasks{Repo: repo, Label: label, Token: tokFn},
		OnOutcome: func(_ context.Context, p hivemcp.Principal, t hivemcp.Task, o hivetool.Outcome) {
			b, _ := json.Marshal(map[string]any{"login": p.Login, "issue": t.Issue, "outcome": o})
			log.Printf("outcome %s", b)
		},
	}
	if checkout != "" {
		idx, errs := codeindex.Build(context.Background(), []codeindex.Repo{{Name: repo, Root: checkout}})
		for name, e := range errs {
			log.Printf("codeindex %s: %v", name, e)
		}
		cfg.Searcher = hivetool.CodeIndexSearcher{Index: func() *codeindex.Index { return idx }}
	}
	if write {
		if tok == "" {
			return errors.New("-write needs GH_TOKEN with contents and pull_requests write")
		}
		cfg.Writer = &hivetool.RESTWriter{Token: tokFn}
	}
	if stateSpec != "" {
		target, branch, ok := strings.Cut(strings.TrimPrefix(stateSpec, "github:"), "@")
		if !strings.HasPrefix(stateSpec, "github:") || !ok || strings.Count(target, "/") != 1 || branch == "" {
			return fmt.Errorf("-state %q: want github:owner/name@branch", stateSpec)
		}
		cfg.Store = &coordstate.ContentsStore{Repo: target, Branch: branch, Token: tokFn, Author: "hive-mcp state"}
	}
	srv, err := hivemcp.New(cfg)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", logRequests(srv))
	log.Printf("serving %s (label %q) at http://%s/mcp", repo, label, addr)
	hs := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return hs.ListenAndServe()
}

// logRequests logs method, status and latency (never bodies or tokens).
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
