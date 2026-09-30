package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGetter serves recorded fixtures and records every request.
type fakeGetter struct {
	mu     sync.Mutex
	routes map[string]Fetched
	calls  []call
}

type call struct {
	url     string
	headers map[string]string
}

func (f *fakeGetter) Get(_ context.Context, u string, h map[string]string) Fetched {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{u, h})
	if r, ok := f.routes[u]; ok {
		r.URL = u
		return r
	}
	return Fetched{URL: u, Status: 404, Body: []byte(`{"error":"not found"}`)}
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const secretSession = "sess-1b2c3d4e5f60718293a4b5c6d7e8f9"

func liveRoutes(t *testing.T) map[string]Fetched {
	r := map[string]Fetched{
		"https://hub.tunaos.org/activity.json": {Status: 200, Body: fixtureBytes(t, "hub-activity.json")},
	}
	for name, tg := range targets {
		r[tg.URL+"/api/health"] = Fetched{Status: 200, Body: []byte(`{"status":"ok"}`)}
		r[tg.URL+"/api/health/deep"] = Fetched{Status: 200, Body: fixtureBytes(t, "deep-"+name+".json")}
		r[tg.URL+"/api/contribute/status"] = Fetched{Status: 200, Body: []byte(`{"hub":"online","served_sha":"2c8584a"}`)}
		r[tg.URL+"/api/contribute/activity?limit=10"] = Fetched{Status: 200, Body: fixtureBytes(t, "activity-"+name+".json")}
		r[tg.URL+"/api/status"] = Fetched{Status: 401, Body: []byte(`{"error":"unauthorized"}`)}
	}
	// Only school accepts the (fake) session, and echoes it back in the body
	// to prove the probe scrubs it.
	st := fixtureBytes(t, "status-school.json")
	st = bytes.Replace(st, []byte(`"hiveId": "hive-school-tunaos"`), []byte(`"hiveId": "hive-school-tunaos", "echo": "`+secretSession+`"`), 1)
	r["https://school.tunaos.org/api/status"] = Fetched{Status: 200, Body: st}
	return r
}

func writeTargets(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "targets.json")
	if err := os.WriteFile(p, fixtureBytes(t, "../targets.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunPublicOnlySendsNoCredentials(t *testing.T) {
	g := &fakeGetter{routes: liveRoutes(t)}
	dir := t.TempDir()
	var out, errb bytes.Buffer
	code := run([]string{"-targets", writeTargets(t), "-json", filepath.Join(dir, "r.json"), "-markdown", filepath.Join(dir, "s.md")},
		&out, &errb, func(string) string { return "" }, g, func() time.Time { return fixtureNow })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, c := range g.calls {
		if len(c.headers) != 0 {
			t.Fatalf("public run sent headers to %s", c.url)
		}
		if strings.HasSuffix(c.url, "/api/status") {
			t.Fatalf("public run requested %s", c.url)
		}
	}
	var r Report
	b, _ := os.ReadFile(filepath.Join(dir, "r.json"))
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if r.Level != Pass || len(r.Hives) != 3 {
		t.Fatalf("report level %s, %d hives", r.Level, len(r.Hives))
	}
	md, _ := os.ReadFile(filepath.Join(dir, "s.md"))
	if !strings.Contains(string(md), "## Hive health") {
		t.Fatal("markdown summary missing")
	}
	if !strings.Contains(out.String(), "== school") {
		t.Fatalf("stdout = %s", out.String())
	}
}

func TestRunSessionIsScopedAndNeverPrinted(t *testing.T) {
	g := &fakeGetter{routes: liveRoutes(t)}
	dir := t.TempDir()
	env := func(k string) string {
		if k == "HIVE_HEALTH_SESSION_SCHOOL" || k == "HIVE_HEALTH_SESSION_REEF" {
			return secretSession
		}
		return ""
	}
	var out, errb bytes.Buffer
	code := run([]string{"-targets", writeTargets(t), "-json", filepath.Join(dir, "r.json"), "-markdown", filepath.Join(dir, "s.md"), "-fail-on-unhealthy"},
		&out, &errb, env, g, func() time.Time { return fixtureNow })
	if code != 2 {
		t.Fatalf("exit %d, want 2 (school fails on the authenticated tier)", code)
	}
	for _, c := range g.calls {
		if len(c.headers) == 0 {
			continue
		}
		if !strings.HasSuffix(c.url, "/api/status") {
			t.Fatalf("credential sent to %s", c.url)
		}
		if strings.Contains(c.url, "reilly") {
			t.Fatal("credential sent to a hive with no session configured")
		}
		if c.headers["Cookie"] != "hive_session="+secretSession || len(c.headers) != 1 {
			t.Fatalf("unexpected headers %v", c.headers)
		}
	}
	r, _ := os.ReadFile(filepath.Join(dir, "r.json"))
	md, _ := os.ReadFile(filepath.Join(dir, "s.md"))
	for name, b := range map[string]string{"json": string(r), "markdown": string(md), "stdout": out.String(), "stderr": errb.String()} {
		if strings.Contains(b, secretSession) {
			t.Fatalf("session value leaked into %s", name)
		}
	}
	var rep Report
	if err := json.Unmarshal(r, &rep); err != nil {
		t.Fatal(err)
	}
	tiers := map[string]string{}
	for _, h := range rep.Hives {
		tiers[h.Name] = h.AuthTier
	}
	if tiers["school"] != "session" || tiers["reef"] != "session-rejected" || tiers["reilly"] != "public" {
		t.Fatalf("tiers = %v", tiers)
	}
}

func TestRunOnlyAndBadFlags(t *testing.T) {
	g := &fakeGetter{routes: liveRoutes(t)}
	var out, errb bytes.Buffer
	env := func(string) string { return "" }
	clock := func() time.Time { return fixtureNow }
	if code := run([]string{"-targets", writeTargets(t), "-only", "reef", "-no-hub"}, &out, &errb, env, g, clock); code != 0 {
		t.Fatalf("exit %d %s", code, errb.String())
	}
	if strings.Contains(out.String(), "== school") || !strings.Contains(out.String(), "== reef") {
		t.Fatalf("-only ignored: %s", out.String())
	}
	for _, c := range g.calls {
		if strings.Contains(c.url, "hub.tunaos.org") {
			t.Fatal("-no-hub fetched the hub")
		}
	}
	if code := run([]string{"-targets", writeTargets(t), "-only", "nope"}, &out, &errb, env, g, clock); code != 1 {
		t.Fatalf("unknown -only accepted: %d", code)
	}
	if code := run([]string{"-targets", "/nonexistent.json"}, &out, &errb, env, g, clock); code != 1 {
		t.Fatal("missing targets accepted")
	}
}

func TestRunStatusFileAndPrevious(t *testing.T) {
	g := &fakeGetter{routes: liveRoutes(t)}
	dir := t.TempDir()
	prev := Report{Hives: []HiveReport{{Name: "reef", Queue: &QueueSnapshot{Hold: 100, Actionable: 275, PRs: 94}}}}
	pb, _ := json.Marshal(prev)
	pp := filepath.Join(dir, "prev.json")
	_ = os.WriteFile(pp, pb, 0o644)
	var out, errb bytes.Buffer
	code := run([]string{"-targets", writeTargets(t), "-only", "reef", "-previous", pp,
		"-status-file", "reef=" + filepath.Join("testdata", "status-reef.json"), "-json", filepath.Join(dir, "r.json")},
		&out, &errb, func(string) string { return "" }, g, func() time.Time { return fixtureNow })
	if code != 0 {
		t.Fatalf("exit %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "tier status-file") || !strings.Contains(out.String(), "backlog-spike") {
		t.Fatalf("stdout = %s", out.String())
	}
	for _, c := range g.calls {
		if strings.HasSuffix(c.url, "/api/status") {
			t.Fatal("status-file run also fetched /api/status")
		}
	}
	// A missing previous report is normal on the first run.
	if code := run([]string{"-targets", writeTargets(t), "-only", "reef", "-previous", filepath.Join(dir, "none.json")},
		&out, &errb, func(string) string { return "" }, g, func() time.Time { return fixtureNow }); code != 0 {
		t.Fatal("missing -previous should not fail")
	}
}

func TestPlanIssuesCommand(t *testing.T) {
	dir := t.TempDir()
	rep := Report{GeneratedAt: fixtureNow, Hives: []HiveReport{failingSchool(t)}}
	rb, _ := json.Marshal(rep)
	_ = os.WriteFile(filepath.Join(dir, "r.json"), rb, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "open.json"), []byte(`[]`), 0o644)
	var out, errb bytes.Buffer
	code := run([]string{"plan-issues", "-report", filepath.Join(dir, "r.json"), "-open-issues", filepath.Join(dir, "open.json"),
		"-out", filepath.Join(dir, "plan")}, &out, &errb, nil, nil, time.Now)
	if code != 0 {
		t.Fatalf("exit %d %s", code, errb.String())
	}
	var plan []map[string]any
	pb, _ := os.ReadFile(filepath.Join(dir, "plan", "plan.json"))
	if err := json.Unmarshal(pb, &plan); err != nil || len(plan) != 1 || plan[0]["op"] != "create" {
		t.Fatalf("plan = %s (%v)", pb, err)
	}
	body, err := os.ReadFile(plan[0]["body_file"].(string))
	if err != nil || !strings.Contains(string(body), IssueMarker("school")) {
		t.Fatalf("body file: %v", err)
	}
	if plan[0]["labels"] != "hive-health,agent/operations" {
		t.Fatalf("labels = %v", plan[0]["labels"])
	}
	if code := run([]string{"plan-issues"}, &out, &errb, nil, nil, time.Now); code != 1 {
		t.Fatal("plan-issues without flags accepted")
	}
}

func TestHTTPGetterOnlyGETsAndRetries(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("method %s", r.Method)
		}
		if r.Header.Get("User-Agent") != userAgent {
			t.Errorf("UA %q", r.Header.Get("User-Agent"))
		}
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	g := NewHTTPGetter(5 * time.Second)
	g.Client.Transport = srv.Client().Transport
	g.Backoff = time.Millisecond
	f := g.Get(context.Background(), srv.URL+"/api/health", nil)
	if !f.OK() || hits != 2 {
		t.Fatalf("fetched %+v after %d hits", f, hits)
	}
	if !strings.Contains(f.Describe(), "HTTP 200") {
		t.Fatal(f.Describe())
	}
}

func TestHTTPGetterRefusesCrossHostRedirect(t *testing.T) {
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("cookie followed a cross-host redirect")
		}
	}))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer srv.Close()
	g := NewHTTPGetter(5 * time.Second)
	g.Client.Transport = srv.Client().Transport
	g.Retries = 0
	f := g.Get(context.Background(), srv.URL+"/api/status", map[string]string{"Cookie": "hive_session=x"})
	if f.Err == "" || !strings.Contains(f.Err, "cross-host") {
		t.Fatalf("redirect followed: %+v", f)
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []string{
		`{"hives":[]}`,
		`{"hives":[{"name":"a","url":"http://insecure"}]}`,
		`{"hives":[{"name":"a","url":"https://a"},{"name":"a","url":"https://b"}]}`,
		`{"hives":[{"url":"https://a"}]}`,
		`{"hives":[{"name":"a","url":"https://a"}],"hub_snapshot_url":"http://hub"}`,
		`{"hives":[{"name":"a","url":"https://a","typo":1}]}`,
	}
	for _, b := range bad {
		if _, err := ParseConfig([]byte(b)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
	c, err := ParseConfig([]byte(`{"hives":[{"name":"a","url":"https://a/"}],"thresholds":{"governor_warn":"10m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Hives[0].URL != "https://a" || c.Thresholds.GovernorWarn.Duration != 10*time.Minute || c.Thresholds.KickFailFactor != 6 {
		t.Fatalf("%+v", c)
	}
	if _, err := LoadConfig("targets.json"); err != nil {
		t.Fatalf("shipped targets.json invalid: %v", err)
	}
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"nope"`)); err == nil {
		t.Fatal("bad duration accepted")
	}
	if b, _ := d.MarshalJSON(); string(b) != `"0s"` {
		t.Fatal(string(b))
	}
}
