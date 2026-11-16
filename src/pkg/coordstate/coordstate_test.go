package coordstate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidKeyAndSessionKey(t *testing.T) {
	for _, k := range []string{"ledger.json", "sessions/hive-lite_o_r_7.json"} {
		if err := ValidKey(k); err != nil {
			t.Errorf("%q: %v", k, err)
		}
	}
	for _, k := range []string{"", "/abs", "a/../b", "a//b", "dir/", "Upper", "sp ace", strings.Repeat("a", 201)} {
		if err := ValidKey(k); !errors.Is(err, ErrBadKey) {
			t.Errorf("%q accepted", k)
		}
	}
	if got := SessionKey("hive-lite:O/r#7"); got != "sessions/hive-lite_o_r_7.json" {
		t.Fatalf("SessionKey = %q", got)
	}
	long := SessionKey(strings.Repeat("x", 300))
	if ValidKey(long) != nil || len(long) > 60 {
		t.Fatalf("long key = %q", long)
	}
	if ValidKey(SessionKey("...")) != nil {
		t.Fatal("dots-only id")
	}
}

func TestMemoryStoreCAS(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	if _, _, err := s.Get(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	v1, err := s.Put(ctx, "k", []byte("a"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "k", []byte("b"), ""); !errors.Is(err, ErrConflict) {
		t.Fatal("create-only over existing key must conflict")
	}
	v2, err := s.Put(ctx, "k", []byte("b"), v1)
	if err != nil || v2 == v1 {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "k", []byte("c"), v1); !errors.Is(err, ErrConflict) {
		t.Fatal("stale version must conflict")
	}
	if b, v, _ := s.Get(ctx, "k"); string(b) != "b" || v != v2 {
		t.Fatalf("%s %s", b, v)
	}
	if _, _, err := s.Get(ctx, "../x"); !errors.Is(err, ErrBadKey) {
		t.Fatal("bad key read")
	}
	if _, err := s.Put(ctx, "../x", nil, ""); !errors.Is(err, ErrBadKey) {
		t.Fatal("bad key write")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.Get(cctx, "k"); err == nil {
		t.Fatal("cancelled get")
	}
	if _, err := s.Put(cctx, "k", nil, v2); err == nil {
		t.Fatal("cancelled put")
	}
}

// racingStore lets another writer in between Get and Put a fixed number of times.
type racingStore struct {
	*MemoryStore
	races int
}

func (r *racingStore) Put(ctx context.Context, key string, value []byte, expect Version) (Version, error) {
	if r.races > 0 {
		r.races--
		cur, ver, _ := r.MemoryStore.Get(ctx, key)
		if _, err := r.MemoryStore.Put(ctx, key, append(cur, '+'), ver); err != nil {
			return "", err
		}
	}
	return r.MemoryStore.Put(ctx, key, value, expect)
}

func TestUpdateRetriesConflicts(t *testing.T) {
	ctx := context.Background()
	s := &racingStore{MemoryStore: NewMemoryStore(), races: 2}
	calls := 0
	err := Update(ctx, s, "k", 0, func(cur []byte) ([]byte, error) {
		calls++
		return append(cur, 'x'), nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if b, _, _ := s.Get(ctx, "k"); string(b) != "++x" {
		t.Fatalf("value = %q", b)
	}
	s.races = 10
	if err := Update(ctx, s, "k", 2, func(cur []byte) ([]byte, error) { return cur, nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("give-up err = %v", err)
	}
	boom := errors.New("boom")
	if err := Update(ctx, NewMemoryStore(), "k", 1, func([]byte) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if err := Update(ctx, NewMemoryStore(), "../k", 1, func([]byte) ([]byte, error) { return nil, nil }); !errors.Is(err, ErrBadKey) {
		t.Fatal(err)
	}
}

func TestLedgerCapsCooldownsAndMergesConcurrentPasses(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)

	a, err := LoadLedger(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := LoadLedger(ctx, store) // an overlapping pass, loaded before a flushes
	for i := 0; i < 3; i++ {
		if ok, _, _ := a.Acquire("groq", 5, now); !ok {
			t.Fatal("a under cap")
		}
	}
	for i := 0; i < 2; i++ {
		if ok, _, _ := b.Acquire("groq", 5, now); !ok {
			t.Fatal("b under cap")
		}
	}
	_ = a.Cooldown("gemini", now.Add(time.Hour))
	_ = b.Cooldown("gemini", now.Add(10*time.Minute)) // shorter: must not win
	if err := a.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Flush(ctx); err != nil { // idempotent: no double count
		t.Fatal(err)
	}

	c, _ := LoadLedger(ctx, store)
	snap := c.Snapshot()
	if snap["groq"].Used != 5 || snap["groq"].Day != "2026-11-16" {
		t.Fatalf("groq = %+v (concurrent spend lost or doubled)", snap["groq"])
	}
	if !snap["gemini"].CooldownUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("gemini cooldown = %v", snap["gemini"].CooldownUntil)
	}
	ok, retry, _ := c.Acquire("groq", 5, now)
	if ok || !retry.Equal(time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("cap not enforced from store: ok=%v retry=%v", ok, retry)
	}
	if ok, retry, _ := c.Acquire("gemini", 0, now); ok || !retry.Equal(now.Add(time.Hour)) {
		t.Fatal("stored cooldown not enforced")
	}
	// A new UTC day resets the count.
	tomorrow := now.Add(2 * time.Hour)
	if ok, _, _ := c.Acquire("groq", 5, tomorrow); !ok {
		t.Fatal("new day still capped")
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ := LoadLedger(ctx, store)
	if e := d.Snapshot()["groq"]; e.Day != "2026-11-17" || e.Used != 1 {
		t.Fatalf("day rollover = %+v", e)
	}
	// Nothing spent: Flush writes nothing.
	empty, _ := LoadLedger(ctx, NewMemoryStore())
	if err := empty.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLoadLedgerErrors(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	_, _ = s.Put(ctx, LedgerKey, []byte("not json"), "")
	if _, err := LoadLedger(ctx, s); err == nil {
		t.Fatal("corrupt ledger accepted")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := LoadLedger(cctx, NewMemoryStore()); err == nil {
		t.Fatal("store error swallowed")
	}
	l, _ := LoadLedger(ctx, NewMemoryStore())
	_, _ = l.Acquire("p", 0, time.Now())
	bad := NewMemoryStore()
	_, _ = bad.Put(ctx, LedgerKey, []byte("[]"), "")
	l.store = bad
	if err := l.Flush(ctx); err == nil {
		t.Fatal("merge into corrupt remote ledger must fail")
	}
}

// fakeGitHub is a minimal Contents + Git Data API with real sha CAS.
type fakeGitHub struct {
	mu       sync.Mutex
	branches map[string]bool
	files    map[string]string // path -> content
	shas     map[string]string
	n        int
	puts     int
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{branches: map[string]bool{}, files: map[string]string{}, shas: map[string]string{}}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(401)
		return
	}
	const contents = "/repos/o/r/contents/"
	var body map[string]any
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, contents):
		p := strings.TrimPrefix(r.URL.Path, contents)
		if !f.branches[r.URL.Query().Get("ref")] {
			w.WriteHeader(404)
			return
		}
		c, ok := f.files[p]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if p == "dir" {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprintf(w, `{"type":"file","encoding":"base64","sha":%q,"content":%q}`, f.shas[p], base64.StdEncoding.EncodeToString([]byte(c))[:4]+"\n"+base64.StdEncoding.EncodeToString([]byte(c))[4:])
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, contents):
		f.puts++
		p := strings.TrimPrefix(r.URL.Path, contents)
		if !f.branches[body["branch"].(string)] {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Branch not found"}`)
			return
		}
		sha, _ := body["sha"].(string)
		cur, exists := f.shas[p]
		switch {
		case exists && sha == "":
			w.WriteHeader(422)
			fmt.Fprint(w, `{"message":"Invalid request.\n\n\"sha\" wasn't supplied."}`)
			return
		case exists && sha != cur, !exists && sha != "":
			w.WriteHeader(409)
			fmt.Fprintf(w, `{"message":"%s does not match"}`, p)
			return
		}
		raw, _ := base64.StdEncoding.DecodeString(body["content"].(string))
		f.n++
		f.files[p], f.shas[p] = string(raw), fmt.Sprintf("sha%d", f.n)
		w.WriteHeader(200)
		fmt.Fprintf(w, `{"content":{"sha":%q}}`, f.shas[p])
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/ref/heads/"):
		if !f.branches[strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/ref/heads/")] {
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, `{}`)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/git/trees":
		w.WriteHeader(201)
		fmt.Fprint(w, `{"sha":"tree1"}`)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/git/commits":
		if ps, _ := body["parents"].([]any); len(ps) != 0 {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(201)
		fmt.Fprint(w, `{"sha":"commit1"}`)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/git/refs":
		f.branches[strings.TrimPrefix(body["ref"].(string), "refs/heads/")] = true
		w.WriteHeader(201)
		fmt.Fprint(w, `{}`)
	default:
		w.WriteHeader(500)
		fmt.Fprint(w, `{"message":"unexpected `+r.Method+` `+r.URL.Path+`"}`)
	}
}

func TestContentsStoreCASAndBranchBootstrap(t *testing.T) {
	gh := newFakeGitHub()
	srv := httptest.NewServer(gh)
	defer srv.Close()
	ctx := context.Background()
	s := &ContentsStore{Repo: "o/r", Branch: "hive-lite-state", Prefix: "state/", BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }}

	if _, _, err := s.Get(ctx, "ledger.json"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing branch get = %v", err)
	}
	v1, err := s.Put(ctx, "ledger.json", []byte(`{"a":1}`), "")
	if err != nil {
		t.Fatalf("first put (creates branch) = %v", err)
	}
	if !gh.branches["hive-lite-state"] || gh.files["state/ledger.json"] != `{"a":1}` {
		t.Fatalf("branch/file not created: %+v", gh.files)
	}
	got, v, err := s.Get(ctx, "ledger.json")
	if err != nil || string(got) != `{"a":1}` || v != v1 {
		t.Fatalf("get = %s %s %v", got, v, err)
	}
	if _, err := s.Put(ctx, "ledger.json", []byte("x"), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("create over existing = %v", err)
	}
	v2, err := s.Put(ctx, "ledger.json", []byte(`{"a":2}`), v1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "ledger.json", []byte(`{"a":3}`), v1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale sha = %v", err)
	}
	if err := s.EnsureBranch(ctx); err != nil {
		t.Fatal("ensure on existing branch")
	}

	// The ledger works end to end over the Contents driver.
	l, err := LoadLedger(ctx, s)
	if err == nil {
		t.Fatalf("ledger {\"a\":2} is not a ledger map; want decode error, got %v", l)
	}
	_, _ = s.Put(ctx, LedgerKey, []byte(`{}`), v2)
	l, err = LoadLedger(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = l.Acquire("groq", 0, time.Now())
	if err := l.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gh.files["state/ledger.json"], `"used": 1`) {
		t.Fatalf("ledger = %s", gh.files["state/ledger.json"])
	}
}

func TestContentsStoreErrors(t *testing.T) {
	gh := newFakeGitHub()
	gh.branches["b"] = true
	gh.files["dir"] = ""
	srv := httptest.NewServer(gh)
	defer srv.Close()
	ctx := context.Background()
	s := &ContentsStore{Repo: "o/r", Branch: "b", BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }}
	if _, _, err := s.Get(ctx, "dir"); err == nil {
		t.Fatal("directory read as file")
	}
	if _, _, err := s.Get(ctx, "Bad"); !errors.Is(err, ErrBadKey) {
		t.Fatal("bad key")
	}
	if _, err := s.Put(ctx, "Bad", nil, ""); !errors.Is(err, ErrBadKey) {
		t.Fatal("bad key put")
	}
	anon := &ContentsStore{Repo: "o/r", Branch: "b", BaseURL: srv.URL}
	if _, _, err := anon.Get(ctx, "k"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthenticated = %v", err)
	}
	if _, err := anon.Put(ctx, "k", nil, ""); err == nil {
		t.Fatal("unauthenticated put")
	}
	if err := anon.EnsureBranch(ctx); err == nil {
		t.Fatal("unauthenticated ensure")
	}
	tokErr := &ContentsStore{Repo: "o/r", Branch: "b", BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "", errors.New("no token") }}
	if _, _, err := tokErr.Get(ctx, "k"); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatal(err)
	}
	down := &ContentsStore{Repo: "o/r", Branch: "b", BaseURL: "http://127.0.0.1:1"}
	if _, _, err := down.Get(ctx, "k"); err == nil {
		t.Fatal("transport")
	}
	if _, err := down.Put(ctx, "k", nil, ""); err == nil {
		t.Fatal("transport put")
	}
	if err := down.EnsureBranch(ctx); err == nil {
		t.Fatal("transport ensure")
	}
	if (&ContentsStore{}).base() != "https://api.github.com" || (&ContentsStore{}).client() == nil {
		t.Fatal("defaults")
	}
	if e := apiError("op", 500, []byte(strings.Repeat("z", 400))); len(e.Error()) > 340 {
		t.Fatal("apiError not bounded")
	}
}
