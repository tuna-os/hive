package coordstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// D1Store keeps keys as rows in a Cloudflare D1 table through the D1 HTTP
// query API, so it works from any host (a hosted CI runner, a Worker, a
// laptop). The Version is a per-row integer: create is INSERT ... ON
// CONFLICT DO NOTHING and update is UPDATE ... WHERE version = ?, and the
// changed-row count decides success or ErrConflict. Unlike the Contents
// driver there is no branch-wide serialisation and no history: one write is
// one row update.
type D1Store struct {
	AccountID  string
	DatabaseID string
	Token      func(context.Context) (string, error)
	BaseURL    string // default https://api.cloudflare.com/client/v4
	HTTP       *http.Client
	Table      string // default "hive_state"

	mu    sync.Mutex
	ready bool
}

var _ Store = (*D1Store)(nil)

type d1Result struct {
	Results []map[string]any `json:"results"`
	Success bool             `json:"success"`
	Meta    struct {
		Changes int `json:"changes"`
	} `json:"meta"`
}

func (s *D1Store) table() string {
	if s.Table != "" {
		return s.Table
	}
	return "hive_state"
}

func (s *D1Store) query(ctx context.Context, sql string, params ...any) (d1Result, error) {
	base := s.BaseURL
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"sql": sql, "params": params})
	url := fmt.Sprintf("%s/accounts/%s/d1/database/%s/query", strings.TrimRight(base, "/"), s.AccountID, s.DatabaseID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return d1Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.Token != nil {
		tok, err := s.Token(ctx)
		if err != nil {
			return d1Result{}, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	c := s.HTTP
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return d1Result{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var env struct {
		Success bool       `json:"success"`
		Result  []d1Result `json:"result"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return d1Result{}, fmt.Errorf("coordstate: d1: %d: unreadable response", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || !env.Success || len(env.Result) == 0 {
		msg := fmt.Sprint(resp.StatusCode)
		if len(env.Errors) > 0 {
			msg += ": " + env.Errors[0].Message
		}
		return d1Result{}, errors.New("coordstate: d1: " + msg)
	}
	return env.Result[0], nil
}

// ensure creates the table once per process; a failed attempt is retried
// on the next call rather than remembered.
func (s *D1Store) ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		return nil
	}
	if _, err := s.query(ctx, "CREATE TABLE IF NOT EXISTS "+s.table()+" (key TEXT PRIMARY KEY, value TEXT NOT NULL, version INTEGER NOT NULL)"); err != nil {
		return err
	}
	s.ready = true
	return nil
}

// Get implements Store.
func (s *D1Store) Get(ctx context.Context, key string) ([]byte, Version, error) {
	if err := ValidKey(key); err != nil {
		return nil, "", err
	}
	if err := s.ensure(ctx); err != nil {
		return nil, "", err
	}
	r, err := s.query(ctx, "SELECT value, version FROM "+s.table()+" WHERE key = ?", key)
	if err != nil {
		return nil, "", err
	}
	if len(r.Results) == 0 {
		return nil, "", ErrNotFound
	}
	v, _ := r.Results[0]["value"].(string)
	return []byte(v), Version(numString(r.Results[0]["version"])), nil
}

// Put implements Store.
func (s *D1Store) Put(ctx context.Context, key string, value []byte, expect Version) (Version, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	if err := s.ensure(ctx); err != nil {
		return "", err
	}
	if expect == "" {
		r, err := s.query(ctx, "INSERT INTO "+s.table()+" (key, value, version) VALUES (?, ?, 1) ON CONFLICT(key) DO NOTHING", key, string(value))
		if err != nil {
			return "", err
		}
		if r.Meta.Changes != 1 {
			return "", fmt.Errorf("%w: %s already exists", ErrConflict, key)
		}
		return "1", nil
	}
	cur, err := strconv.ParseInt(string(expect), 10, 64)
	if err != nil {
		return "", fmt.Errorf("%w: %s: version %q is not a D1 version", ErrConflict, key, expect)
	}
	r, err := s.query(ctx, "UPDATE "+s.table()+" SET value = ?, version = version + 1 WHERE key = ? AND version = ?", string(value), key, cur)
	if err != nil {
		return "", err
	}
	if r.Meta.Changes != 1 {
		return "", fmt.Errorf("%w: %s", ErrConflict, key)
	}
	return Version(strconv.FormatInt(cur+1, 10)), nil
}

// numString renders a JSON number (float64 after decoding) as an integer.
func numString(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case string:
		return n
	default:
		return fmt.Sprint(v)
	}
}
