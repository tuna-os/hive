package coordstate

import (
	"context"
	"fmt"
	"strings"
)

// Open parses a store spec:
//
//	github                          branch hive-lite-state in defaultRepo
//	github:owner/name@branch        a branch in another repository
//	d1:<account-id>/<database-id>   a Cloudflare D1 database
//
// ghToken authenticates the GitHub driver, cfToken the D1 driver. An empty
// spec returns (nil, nil): no store.
func Open(spec, defaultRepo, ghToken, cfToken, author string) (Store, error) {
	switch {
	case spec == "":
		return nil, nil
	case spec == "github" || strings.HasPrefix(spec, "github:"):
		target, branch := defaultRepo, "hive-lite-state"
		if rest, ok := strings.CutPrefix(spec, "github:"); ok {
			target = rest
			if at := strings.LastIndex(rest, "@"); at >= 0 {
				target, branch = rest[:at], rest[at+1:]
			}
		}
		if strings.Count(target, "/") != 1 || strings.HasPrefix(target, "/") || strings.HasSuffix(target, "/") || branch == "" {
			return nil, fmt.Errorf("coordstate: %q: want github or github:owner/name@branch", spec)
		}
		if ghToken == "" {
			return nil, fmt.Errorf("coordstate: %q needs a GitHub token with contents write", spec)
		}
		return &ContentsStore{Repo: target, Branch: branch, Author: author, Token: func(context.Context) (string, error) { return ghToken, nil }}, nil
	case strings.HasPrefix(spec, "d1:"):
		acct, db, ok := strings.Cut(strings.TrimPrefix(spec, "d1:"), "/")
		if !ok || acct == "" || db == "" || strings.Contains(db, "/") {
			return nil, fmt.Errorf("coordstate: %q: want d1:<account-id>/<database-id>", spec)
		}
		if cfToken == "" {
			return nil, fmt.Errorf("coordstate: %q needs a Cloudflare token with D1 write", spec)
		}
		return &D1Store{AccountID: acct, DatabaseID: db, Token: func(context.Context) (string, error) { return cfToken, nil }}, nil
	default:
		return nil, fmt.Errorf("coordstate: unknown store %q (github, github:owner/name@branch, d1:<account>/<database>)", spec)
	}
}
