package coordstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeD1 executes the four statements D1Store issues against a map, with
// real row-version semantics.
type fakeD1 struct {
	mu   sync.Mutex
	rows map[string]struct {
		val string
		ver int64
	}
	created    bool
	failCreate int
	queries    int
}

func (f *fakeD1) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	if r.Header.Get("Authorization") != "Bearer cf" {
		w.WriteHeader(403)
		io.WriteString(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"result":[]}`)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/accounts/acct/d1/database/db/query") {
		w.WriteHeader(404)
		io.WriteString(w, `not json`)
		return
	}
	var q struct {
		SQL    string `json:"sql"`
		Params []any  `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&q)
	ok := func(results []map[string]any, changes int) {
		b, _ := json.Marshal(map[string]any{"success": true, "errors": []any{}, "result": []any{map[string]any{"success": true, "results": results, "meta": map[string]any{"changes": changes}}}})
		w.Write(b)
	}
	switch {
	case strings.HasPrefix(q.SQL, "CREATE TABLE"):
		if f.failCreate > 0 {
			f.failCreate--
			w.WriteHeader(500)
			io.WriteString(w, `{"success":false,"errors":[{"code":7500,"message":"internal"}],"result":[]}`)
			return
		}
		f.created = true
		ok(nil, 0)
	case !f.created:
		w.WriteHeader(400)
		io.WriteString(w, `{"success":false,"errors":[{"code":7500,"message":"no such table"}],"result":[]}`)
	case strings.HasPrefix(q.SQL, "SELECT"):
		row, found := f.rows[q.Params[0].(string)]
		if !found {
			ok([]map[string]any{}, 0)
			return
		}
		ok([]map[string]any{{"value": row.val, "version": row.ver}}, 0)
	case strings.HasPrefix(q.SQL, "INSERT"):
		k := q.Params[0].(string)
		if _, found := f.rows[k]; found {
			ok(nil, 0)
			return
		}
		f.rows[k] = struct {
			val string
			ver int64
		}{q.Params[1].(string), 1}
		ok(nil, 1)
	case strings.HasPrefix(q.SQL, "UPDATE"):
		k := q.Params[1].(string)
		row, found := f.rows[k]
		if !found || float64(row.ver) != q.Params[2].(float64) {
			ok(nil, 0)
			return
		}
		f.rows[k] = struct {
			val string
			ver int64
		}{q.Params[0].(string), row.ver + 1}
		ok(nil, 1)
	default:
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"success":false,"errors":[{"code":1,"message":"unexpected %s"}],"result":[]}`, q.SQL)
	}
}

func newD1(t *testing.T) (*D1Store, *fakeD1) {
	f := &fakeD1{rows: map[string]struct {
		val string
		ver int64
	}{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &D1Store{AccountID: "acct", DatabaseID: "db", BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "cf", nil }}, f
}

func TestD1StoreCAS(t *testing.T) {
	ctx := context.Background()
	s, f := newD1(t)
	f.failCreate = 1
	if _, _, err := s.Get(ctx, "k"); err == nil || !strings.Contains(err.Error(), "internal") {
		t.Fatalf("failed table create = %v", err)
	}
	if _, _, err := s.Get(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create was not retried: %v", err)
	}
	v1, err := s.Put(ctx, "k", []byte("a"), "")
	if err != nil || v1 != "1" {
		t.Fatalf("create = %q %v", v1, err)
	}
	if _, err := s.Put(ctx, "k", []byte("x"), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("create over existing = %v", err)
	}
	v2, err := s.Put(ctx, "k", []byte("b"), v1)
	if err != nil || v2 != "2" {
		t.Fatalf("update = %q %v", v2, err)
	}
	if _, err := s.Put(ctx, "k", []byte("c"), v1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale = %v", err)
	}
	if _, err := s.Put(ctx, "k", []byte("c"), "sha-from-github"); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign version = %v", err)
	}
	if b, v, err := s.Get(ctx, "k"); err != nil || string(b) != "b" || v != v2 {
		t.Fatalf("get = %s %s %v", b, v, err)
	}
	if _, _, err := s.Get(ctx, "../k"); !errors.Is(err, ErrBadKey) {
		t.Fatal("bad key get")
	}
	if _, err := s.Put(ctx, "../k", nil, ""); !errors.Is(err, ErrBadKey) {
		t.Fatal("bad key put")
	}
	if s.table() != "hive_state" {
		t.Fatal("default table")
	}
}

func TestD1StoreConcurrentUpdateAndLedger(t *testing.T) {
	ctx := context.Background()
	s, _ := newD1(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Update(ctx, s, "n", 50, func(cur []byte) ([]byte, error) {
				n, _ := strconv.Atoi(string(cur))
				return []byte(strconv.Itoa(n + 1)), nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if b, _, _ := s.Get(ctx, "n"); string(b) != "12" {
		t.Fatalf("counter = %s", b)
	}
}

func TestD1StoreErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := newD1(t)
	bad := *s
	bad.Token = func(context.Context) (string, error) { return "nope", nil }
	if _, _, err := (&bad).Get(ctx, "k"); err == nil || !strings.Contains(err.Error(), "Authentication") {
		t.Fatalf("auth error = %v", err)
	}
	wrongDB := &D1Store{AccountID: "acct", DatabaseID: "other", BaseURL: s.BaseURL, Token: s.Token}
	if _, _, err := wrongDB.Get(ctx, "k"); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("non-JSON = %v", err)
	}
	tokErr := &D1Store{AccountID: "acct", DatabaseID: "db", BaseURL: s.BaseURL, Token: func(context.Context) (string, error) { return "", errors.New("no token") }}
	if _, err := tokErr.Put(ctx, "k", nil, ""); err == nil {
		t.Fatal("token error swallowed")
	}
	down := &D1Store{AccountID: "a", DatabaseID: "b", BaseURL: "http://127.0.0.1:1"}
	if _, _, err := down.Get(ctx, "k"); err == nil {
		t.Fatal("transport error swallowed")
	}
	if numString("7") != "7" || numString(int64(3)) != "3" || numString(float64(9)) != "9" {
		t.Fatal("numString")
	}
}

func TestOpen(t *testing.T) {
	for _, spec := range []string{"", "memory"} {
		if s, err := Open(spec, "cf"); s != nil || err != nil {
			t.Fatalf("%q = %v %v", spec, s, err)
		}
	}
	s, err := Open("d1:acct/db", "cf")
	if d, ok := s.(*D1Store); err != nil || !ok || d.AccountID != "acct" || d.DatabaseID != "db" {
		t.Fatalf("d1 = %+v %v", s, err)
	}
	if tok, _ := s.(*D1Store).Token(context.Background()); tok != "cf" {
		t.Fatal("d1 token")
	}
	// The GitHub Contents driver is a GitHub write and never selectable.
	for _, spec := range []string{"github", "github:a/b@state"} {
		s, err := Open(spec, "cf")
		if s != nil || err == nil || !strings.Contains(err.Error(), "GitHub write") {
			t.Errorf("%q = %v %v; want a GitHub-write refusal", spec, s, err)
		}
	}
	for _, bad := range []struct{ spec, cf string }{
		{"d1:acct", "cf"}, {"d1:acct/db/x", "cf"}, {"d1:acct/db", ""}, {"s3:bucket", "cf"},
	} {
		if _, err := Open(bad.spec, bad.cf); err == nil {
			t.Errorf("%q accepted", bad.spec)
		}
	}
}
