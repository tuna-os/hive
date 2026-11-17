package coordstate

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/turn/llmrouter"
)

// LedgerKey is where the router ledger lives in the store.
const LedgerKey = "ledger.json"

// LedgerEntry is one provider's persisted budget state.
type LedgerEntry struct {
	Day           string    `json:"day"` // UTC date the Used count belongs to
	Used          int       `json:"used"`
	CooldownUntil time.Time `json:"cooldown_until,omitzero"`
}

// Ledger is an llmrouter.Ledger that loads once per pass and flushes once at
// the end. A remote store cannot take one write per model request, so the
// pass works in memory and Flush merges its delta: request counts for the
// same day add (two overlapping passes both spent budget), and the later
// cooldown wins. Flush is a CAS update, so a concurrent pass is merged, not
// overwritten.
type Ledger struct {
	store Store

	mu    sync.Mutex
	base  map[string]LedgerEntry // as loaded
	spent map[string]map[string]int
	cool  map[string]time.Time
}

var _ llmrouter.Ledger = (*Ledger)(nil)

// LoadLedger reads the ledger from store (an absent ledger is empty).
func LoadLedger(ctx context.Context, store Store) (*Ledger, error) {
	l := &Ledger{store: store, base: map[string]LedgerEntry{}, spent: map[string]map[string]int{}, cool: map[string]time.Time{}}
	raw, _, err := store.Get(ctx, LedgerKey)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &l.base); err != nil {
			return nil, err
		}
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}
	return l, nil
}

// Acquire implements llmrouter.Ledger.
func (l *Ledger) Acquire(provider string, dailyCap int, now time.Time) (bool, time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := l.cooldownLocked(provider); now.Before(until) {
		return false, until, nil
	}
	day := now.UTC().Format(time.DateOnly)
	used := l.spent[provider][day]
	if b := l.base[provider]; b.Day == day {
		used += b.Used
	}
	if dailyCap > 0 && used >= dailyCap {
		y, m, d := now.UTC().Date()
		return false, time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC), nil
	}
	if l.spent[provider] == nil {
		l.spent[provider] = map[string]int{}
	}
	l.spent[provider][day]++
	return true, time.Time{}, nil
}

// Cooldown implements llmrouter.Ledger.
func (l *Ledger) Cooldown(provider string, until time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until.After(l.cool[provider]) {
		l.cool[provider] = until
	}
	return nil
}

func (l *Ledger) cooldownLocked(provider string) time.Time {
	c := l.base[provider].CooldownUntil
	if l.cool[provider].After(c) {
		c = l.cool[provider]
	}
	return c
}

// Snapshot returns the merged view this pass would write.
func (l *Ledger) Snapshot() map[string]LedgerEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mergeLocked(l.base)
}

func (l *Ledger) mergeLocked(into map[string]LedgerEntry) map[string]LedgerEntry {
	out := make(map[string]LedgerEntry, len(into))
	for k, v := range into {
		out[k] = v
	}
	for p, days := range l.spent {
		for day, n := range days {
			e := out[p]
			if e.Day < day {
				e.Day, e.Used = day, 0
			}
			if e.Day == day {
				e.Used += n
			}
			out[p] = e
		}
	}
	for p, until := range l.cool {
		e := out[p]
		if until.After(e.CooldownUntil) {
			e.CooldownUntil = until
		}
		out[p] = e
	}
	return out
}

// Flush merges this pass's spend and cooldowns into the stored ledger. After
// a successful flush the pass's delta is folded into its base, so a second
// Flush does not double-count.
func (l *Ledger) Flush(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.spent) == 0 && len(l.cool) == 0 {
		return nil
	}
	var merged map[string]LedgerEntry
	err := Update(ctx, l.store, LedgerKey, 0, func(cur []byte) ([]byte, error) {
		remote := map[string]LedgerEntry{}
		if cur != nil {
			if err := json.Unmarshal(cur, &remote); err != nil {
				return nil, err
			}
		}
		merged = l.mergeLocked(remote)
		return json.MarshalIndent(merged, "", "  ")
	})
	if err != nil {
		return err
	}
	l.base = merged
	l.spent = map[string]map[string]int{}
	l.cool = map[string]time.Time{}
	return nil
}
