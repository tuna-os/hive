package coordstate

import (
	"context"
	"fmt"
	"strings"
)

// Open parses the store spec a binary accepts:
//
//	"" or memory                    no durable store: (nil, nil), the caller
//	                                keeps state in its own process
//	d1:<account-id>/<database-id>   a Cloudflare D1 database
//
// cfToken authenticates the D1 driver.
//
// The GitHub Contents driver (ContentsStore) is deliberately not
// selectable here. Every checkpoint it writes is a commit to a branch, which
// is a GitHub write, and per the maintainer decision on hivecommons/hive#18464
// all GitHub writes stay with the Hive App. A "github" spec is refused with
// that reason; embedders that need the driver construct it explicitly.
func Open(spec, cfToken string) (Store, error) {
	switch {
	case spec == "" || spec == "memory":
		return nil, nil
	case spec == "github" || strings.HasPrefix(spec, "github:"):
		return nil, fmt.Errorf("coordstate: %q is a GitHub write (a commit per checkpoint) and is not selectable; GitHub writes stay with the Hive App (hivecommons/hive#18464). Use memory or d1:<account>/<database>", spec)
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
		return nil, fmt.Errorf("coordstate: unknown store %q (memory, d1:<account>/<database>)", spec)
	}
}
