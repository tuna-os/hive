// Package coordstate is the small ephemeral-state store a stateless Hive
// coordinator keeps between passes (RFC hivecommons/hive#18464 §7): session
// checkpoints and the router ledger. Governance state (labels, claims, PRs)
// stays on GitHub; only what a pass needs to resume lives here.
//
// The contract is compare-and-swap per key. Every driver must make Put with
// a stale version fail with ErrConflict, because two passes can overlap (a
// cron runner is "at most every 5 minutes, without guarantee"). On GitHub the
// only per-file CAS is the Contents API sha precondition; a git ref update
// with force=false only rejects non-fast-forwards and is not a CAS.
package coordstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Version is an opaque per-key version token (a blob sha, an etag, a row
// version). The empty Version means "the key does not exist yet".
type Version string

// Store is a compare-and-swap key/value store.
type Store interface {
	// Get returns the value and its version, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, Version, error)
	// Put writes value only if the key is currently at expect (empty:
	// create-only). It returns the new version, or ErrConflict when another
	// writer got there first.
	Put(ctx context.Context, key string, value []byte, expect Version) (Version, error)
}

// Errors every driver maps onto.
var (
	ErrNotFound = errors.New("coordstate: not found")
	ErrConflict = errors.New("coordstate: version conflict")
	ErrBadKey   = errors.New("coordstate: invalid key")
)

// ValidKey reports whether key is a safe relative slash path of lowercase
// letters, digits, '-', '_', '.' segments (no "..", no leading slash).
func ValidKey(key string) error {
	if key == "" || len(key) > 200 || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("%w: %q", ErrBadKey, key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: %q", ErrBadKey, key)
		}
		for _, r := range seg {
			ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
			if !ok {
				return fmt.Errorf("%w: %q", ErrBadKey, key)
			}
		}
	}
	return nil
}

// SessionKey maps a free-form session ID ("hive-lite:o/r#7") to a valid key
// under sessions/.
func SessionKey(sessionID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(sessionID) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), ".")
	if name == "" || len(name) > 120 {
		sum := sha256.Sum256([]byte(sessionID))
		name = hex.EncodeToString(sum[:12])
	}
	return "sessions/" + name + ".json"
}

// MemoryStore is the in-process Store, for tests and single-pass runs.
type MemoryStore struct {
	mu   sync.Mutex
	data map[string][]byte
	ver  map[string]int
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: map[string][]byte{}, ver: map[string]int{}}
}

// Get implements Store.
func (m *MemoryStore) Get(ctx context.Context, key string) ([]byte, Version, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if err := ValidKey(key); err != nil {
		return nil, "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return nil, "", ErrNotFound
	}
	return append([]byte(nil), v...), Version(fmt.Sprint(m.ver[key])), nil
}

// Put implements Store.
func (m *MemoryStore) Put(ctx context.Context, key string, value []byte, expect Version) (Version, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := ValidKey(key); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := Version("")
	if _, ok := m.data[key]; ok {
		cur = Version(fmt.Sprint(m.ver[key]))
	}
	if cur != expect {
		return "", fmt.Errorf("%w: %s at %q, expected %q", ErrConflict, key, cur, expect)
	}
	m.ver[key]++
	m.data[key] = append([]byte(nil), value...)
	return Version(fmt.Sprint(m.ver[key])), nil
}

// Update applies fn to the current value of key (nil when absent) and writes
// the result with CAS, retrying up to attempts times on ErrConflict. fn must
// be a pure merge: it may run more than once.
func Update(ctx context.Context, s Store, key string, attempts int, fn func(cur []byte) ([]byte, error)) error {
	if attempts <= 0 {
		attempts = 4
	}
	var last error
	for i := 0; i < attempts; i++ {
		cur, ver, err := s.Get(ctx, key)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		next, err := fn(cur)
		if err != nil {
			return err
		}
		if _, err := s.Put(ctx, key, next, ver); err == nil {
			return nil
		} else if !errors.Is(err, ErrConflict) {
			return err
		} else {
			last = err
		}
	}
	return fmt.Errorf("coordstate: %s: gave up after %d conflicting writes: %w", key, attempts, last)
}
