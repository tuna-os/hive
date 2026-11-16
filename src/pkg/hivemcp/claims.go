package hivemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/coordstate"
)

// claimRecord is the ephemeral per-issue lease connected agents coordinate
// on. Done claims stay so a finished issue is not offered again while its
// label is still set.
type claimRecord struct {
	Login   string    `json:"login"`
	Expires time.Time `json:"expires"`
	Done    string    `json:"done,omitempty"` // terminal tool once finished
}

var errClaimed = errors.New("hivemcp: issue is claimed by another contributor")

func claimKey(issue int) string { return fmt.Sprintf("claims/issue-%d.json", issue) }

func (s *Server) claim(ctx context.Context, issue int) (claimRecord, bool, error) {
	raw, _, err := s.cfg.Store.Get(ctx, claimKey(issue))
	if errors.Is(err, coordstate.ErrNotFound) {
		return claimRecord{}, false, nil
	}
	if err != nil {
		return claimRecord{}, false, err
	}
	var c claimRecord
	if err := json.Unmarshal(raw, &c); err != nil {
		return claimRecord{}, false, nil // unreadable: treat as free
	}
	return c, true, nil
}

// claimOpenTo reports whether p may take the issue: unclaimed, expired, or
// already p's (and not finished).
func (s *Server) claimOpenTo(c claimRecord, p Principal) bool {
	if c.Done != "" {
		return false
	}
	return strings.EqualFold(c.Login, p.Login) || !s.cfg.Now().Before(c.Expires)
}

// takeClaim acquires or renews the lease under CAS; a concurrent taker
// loses with errClaimed.
func (s *Server) takeClaim(ctx context.Context, issue int, p Principal) error {
	return s.writeClaim(ctx, issue, p, "")
}

func (s *Server) finishClaim(ctx context.Context, issue int, p Principal, tool string) error {
	return s.writeClaim(ctx, issue, p, tool)
}

func (s *Server) writeClaim(ctx context.Context, issue int, p Principal, done string) error {
	raw, ver, err := s.cfg.Store.Get(ctx, claimKey(issue))
	if err != nil && !errors.Is(err, coordstate.ErrNotFound) {
		return err
	}
	if err == nil {
		var c claimRecord
		if json.Unmarshal(raw, &c) == nil && !s.claimOpenTo(c, p) {
			return fmt.Errorf("%w (#%d, %s)", errClaimed, issue, c.Login)
		}
	}
	next, _ := json.Marshal(claimRecord{Login: p.Login, Expires: s.cfg.Now().Add(s.cfg.ClaimTTL).UTC(), Done: done})
	if _, err := s.cfg.Store.Put(ctx, claimKey(issue), next, ver); err != nil {
		if errors.Is(err, coordstate.ErrConflict) {
			return fmt.Errorf("%w (#%d, taken concurrently)", errClaimed, issue)
		}
		return err
	}
	return nil
}
