package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Tests for the hub registration protocol v1, rollout step 1 (#18642). They
// reuse the v6 self-service test env (selfservice_api_test.go): stubbed
// GitHub, temp stores, the flag on.

const hubV1TestID = "byo-alice--lab"

func newHubV1TestEnv(t *testing.T) *selfServiceTestEnv {
	t.Helper()
	t.Setenv(selfServiceMaxReposEnv, "")
	return newSelfServiceTestEnv(t)
}

// v1 sends a request through requireAuth to a v1 handler, filling the {id}
// path value the mux would set in production.
func (env *selfServiceTestEnv) v1(t *testing.T, user, method, path, body string, headers map[string]string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Origin", selfServiceTestOrigin)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if user != "" {
		req.AddCookie(&http.Cookie{
			Name:  "hive_hub_user",
			Value: mintHubUserCookieValueV2(deriveDomainKey(grantableTestSecret, infoSessionEd25519Seed), user),
		})
	}
	if rest, ok := strings.CutPrefix(path, hubV1BasePath+"/registrations/"); ok {
		id, _, _ := strings.Cut(rest, "/")
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	env.s.requireAuth(handler)(rec, req)
	return rec
}

func (env *selfServiceTestEnv) v1Get(t *testing.T, user, id string) *httptest.ResponseRecorder {
	t.Helper()
	return env.v1(t, user, http.MethodGet, hubV1BasePath+"/registrations/"+id, "", nil, env.s.handleHubV1Get)
}

func (env *selfServiceTestEnv) v1Patch(t *testing.T, user, id, ifMatch, body string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if ifMatch != "" {
		headers["If-Match"] = ifMatch
	}
	return env.v1(t, user, http.MethodPatch, hubV1BasePath+"/registrations/"+id, body, headers, env.s.handleHubV1Patch)
}

// etagOf reads the current ETag through GET, as a client would.
func (env *selfServiceTestEnv) etagOf(t *testing.T, id string) string {
	t.Helper()
	rec := env.v1Get(t, selfServiceTestUser, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", id, rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("GET returned no ETag")
	}
	return etag
}

func decodeV1Registration(t *testing.T, rec *httptest.ResponseRecorder) hubV1RegistrationView {
	t.Helper()
	var resp struct {
		Registration hubV1RegistrationView `json:"registration"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode registration: %v (%s)", err, rec.Body.String())
	}
	return resp.Registration
}

func sortedLower(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	sort.Strings(out)
	return out
}

func TestHubV1Discovery(t *testing.T) {
	cases := []struct {
		name         string
		flag         string
		maxReposEnv  string
		wantStatus   int
		wantMaxRepos int
	}{
		{name: "default cap", flag: "true", wantStatus: http.StatusOK, wantMaxRepos: selfServiceMaxReposDefault},
		{name: "configured cap", flag: "true", maxReposEnv: "100", wantStatus: http.StatusOK, wantMaxRepos: 100},
		{name: "cap clamped to ceiling", flag: "true", maxReposEnv: "100000", wantStatus: http.StatusOK, wantMaxRepos: selfServiceMaxReposCeiling},
		{name: "invalid cap falls back", flag: "true", maxReposEnv: "-3", wantStatus: http.StatusOK, wantMaxRepos: selfServiceMaxReposDefault},
		{name: "flag off hides the protocol", flag: "", wantStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			t.Setenv(selfServiceRegistrationEnv, tc.flag)
			t.Setenv(selfServiceMaxReposEnv, tc.maxReposEnv)
			// Discovery is public: no session cookie.
			req := httptest.NewRequest(http.MethodGet, hubV1BasePath, nil)
			rec := httptest.NewRecorder()
			env.s.handleHubV1Discovery(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			var d struct {
				Protocol    string           `json:"protocol"`
				Version     string           `json:"version"`
				BasePath    string           `json:"base_path"`
				Operations  []hubV1Operation `json:"operations"`
				PatchFields []string         `json:"patch_fields"`
				Limits      struct {
					MaxRepos           int    `json:"max_repos"`
					MaxPerUser         int    `json:"max_per_user"`
					Expiry             string `json:"expiry"`
					KeyRotationOverlap string `json:"key_rotation_overlap"`
				} `json:"limits"`
				ProofMethods []string `json:"proof_methods"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			if d.Protocol != hubV1Protocol || d.Version != "1" || d.BasePath != "/api/hub/v1" {
				t.Fatalf("identity = %q %q %q", d.Protocol, d.Version, d.BasePath)
			}
			if d.Limits.MaxRepos != tc.wantMaxRepos || d.Limits.MaxPerUser != selfServiceMaxPerUserDefault ||
				d.Limits.Expiry != selfServiceExpiryDefault.String() || d.Limits.KeyRotationOverlap != selfServiceKeyRotationOverlap.String() {
				t.Fatalf("limits = %+v", d.Limits)
			}
			if !reflect.DeepEqual(d.ProofMethods, []string{"github-user-token"}) {
				t.Fatalf("proof_methods = %v", d.ProofMethods)
			}
			if !reflect.DeepEqual(d.PatchFields, []string{"repos", "dashboard_url"}) {
				t.Fatalf("patch_fields = %v", d.PatchFields)
			}
			got := map[string]hubV1Operation{}
			for _, op := range d.Operations {
				got[op.Name] = op
			}
			want := map[string]string{
				"discovery":  "GET /api/hub/v1",
				"create":     "POST /api/hub/v1/registrations",
				"list":       "GET /api/hub/v1/registrations",
				"get":        "GET /api/hub/v1/registrations/{id}",
				"update":     "PATCH /api/hub/v1/registrations/{id}",
				"rotate_key": "POST /api/hub/v1/registrations/{id}/keys",
				"delete":     "DELETE /api/hub/v1/registrations/{id}",
			}
			if len(got) != len(want) {
				t.Fatalf("operations = %+v", d.Operations)
			}
			for name, route := range want {
				op, ok := got[name]
				if !ok || op.Method+" "+op.Path != route {
					t.Errorf("operation %s = %+v, want %s", name, op, route)
				}
			}
			if got["update"].IfMatch != "required" {
				t.Errorf("update if_match = %q, want required", got["update"].IfMatch)
			}
		})
	}
}

func TestHubV1RoutesAreRegistered(t *testing.T) {
	env := newHubV1TestEnv(t)
	env.s.mux = http.NewServeMux()
	env.s.registerHubV1Routes()
	for _, op := range hubV1Operations {
		path := strings.ReplaceAll(op.Path, "{id}", hubV1TestID)
		req := httptest.NewRequest(op.Method, path, nil)
		if _, pattern := env.s.mux.Handler(req); pattern != op.Method+" "+op.Path {
			t.Errorf("%s %s routed to %q", op.Method, path, pattern)
		}
	}
}

func TestHubV1CreateGetList(t *testing.T) {
	env := newHubV1TestEnv(t)
	rec := env.v1(t, selfServiceTestUser, http.MethodPost, hubV1BasePath+"/registrations", selfServiceHappyBody, nil, env.s.handleHubV1Create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != hubV1BasePath+"/registrations/"+hubV1TestID {
		t.Fatalf("Location = %q", loc)
	}
	etag := rec.Header().Get("ETag")
	var created struct {
		Registration hubV1RegistrationView `json:"registration"`
		Key          string                `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if etag == "" || created.Registration.ETag != etag || created.Key == "" {
		t.Fatalf("create: etag header %q, body etag %q, key set %v", etag, created.Registration.ETag, created.Key != "")
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(created.Key, hubV1TestID); !ok {
		t.Fatal("v1-created key does not authenticate")
	}

	if got := env.etagOf(t, hubV1TestID); got != etag {
		t.Fatalf("GET ETag %q != create ETag %q", got, etag)
	}
	notModified := env.v1(t, selfServiceTestUser, http.MethodGet, hubV1BasePath+"/registrations/"+hubV1TestID, "", map[string]string{"If-None-Match": etag}, env.s.handleHubV1Get)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", notModified.Code)
	}

	list := env.v1(t, selfServiceTestUser, http.MethodGet, hubV1BasePath+"/registrations", "", nil, env.s.handleHubV1List)
	var listed struct {
		Registrations []hubV1RegistrationView `json:"registrations"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Registrations) != 1 || listed.Registrations[0].ETag != etag {
		t.Fatalf("list = %s", list.Body.String())
	}
	if strings.Contains(list.Body.String(), created.Key) || strings.Contains(list.Body.String(), "key_hash") {
		t.Fatal("list leaked key material")
	}
	other := env.v1(t, selfServiceTestOther, http.MethodGet, hubV1BasePath+"/registrations", "", nil, env.s.handleHubV1List)
	if strings.Contains(other.Body.String(), hubV1TestID) {
		t.Fatal("another user's list shows alice's registration")
	}
}

func TestHubV1PatchKeepsKey(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRepos []string
		wantURL   string
		wantCalls []string // GitHub repo checks the PATCH may make
	}{
		{
			name:      "add one repo",
			body:      `{"repos":{"add":["alice/docs"]}}`,
			wantRepos: []string{"alice/lab", "alice/tools", "alice/docs"},
			wantURL:   "https://lab.example.org/",
			wantCalls: []string{"alice/docs"},
		},
		{
			name:      "add and remove",
			body:      `{"repos":{"add":["alice/docs","alice/site"],"remove":["alice/tools"]}}`,
			wantRepos: []string{"alice/lab", "alice/docs", "alice/site"},
			wantURL:   "https://lab.example.org/",
			wantCalls: []string{"alice/docs", "alice/site"},
		},
		{
			name:      "remove only needs no proof",
			body:      `{"repos":{"remove":["alice/tools"]}}`,
			wantRepos: []string{"alice/lab"},
			wantURL:   "https://lab.example.org/",
		},
		{
			name:      "full replacement proves only the new repo",
			body:      `{"repos":["alice/lab","alice/docs"]}`,
			wantRepos: []string{"alice/lab", "alice/docs"},
			wantURL:   "https://lab.example.org/",
			wantCalls: []string{"alice/docs"},
		},
		{
			name:      "dashboard url only",
			body:      `{"dashboard_url":"https://dash.example.org/"}`,
			wantRepos: []string{"alice/lab", "alice/tools"},
			wantURL:   "https://dash.example.org/",
		},
		{
			name:      "clear dashboard url",
			body:      `{"dashboard_url":""}`,
			wantRepos: []string{"alice/lab", "alice/tools"},
			wantURL:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			id, key := env.registerHappy(t)
			before, _ := env.s.selfService.get(id)
			etag := env.etagOf(t, id)

			env.checkedRepos() // drop the registration's checks

			rec := env.v1Patch(t, selfServiceTestUser, id, etag, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
			}
			view := decodeV1Registration(t, rec)
			if !reflect.DeepEqual(sortedLower(view.Repos), sortedLower(tc.wantRepos)) || view.DashboardURL != tc.wantURL {
				t.Fatalf("view repos=%v url=%q", view.Repos, view.DashboardURL)
			}
			if newTag := rec.Header().Get("ETag"); newTag == "" || newTag == etag || newTag != view.ETag {
				t.Fatalf("ETag after PATCH = %q (was %q, body %q)", newTag, etag, view.ETag)
			}
			if calls := env.checkedRepos(); !reflect.DeepEqual(sortedLower(calls), sortedLower(tc.wantCalls)) {
				t.Fatalf("GitHub checks = %v, want %v", calls, tc.wantCalls)
			}

			after, _ := env.s.selfService.get(id)
			if after.KeyHash != before.KeyHash || after.PrevKeyHash != before.PrevKeyHash || after.Owner != before.Owner || !after.CreatedAt.Equal(before.CreatedAt) {
				t.Fatal("PATCH changed the credential or identity fields")
			}
			// The whole point of the edit route: the spoke keeps heartbeating
			// with the key it already has.
			if gen, ok := env.s.verifyHeartbeatBearerGeneration(key, id); !ok || gen != selfServiceBearerGeneration {
				t.Fatalf("key no longer authenticates the heartbeat after PATCH: gen=%d ok=%v", gen, ok)
			}
			if !strings.Contains(env.logs.String(), "audit: selfservice_hive_updated") {
				t.Fatal("no update audit line")
			}
		})
	}
}

func TestHubV1PatchRefusals(t *testing.T) {
	cases := []struct {
		name    string
		user    string
		ifMatch string // "current" = read it first, "stale" = from before a change
		body    string
		status  int
	}{
		{name: "stale etag", user: selfServiceTestUser, ifMatch: "stale", body: `{"repos":{"add":["alice/docs"]}}`, status: http.StatusPreconditionFailed},
		{name: "made-up etag", user: selfServiceTestUser, ifMatch: `"nope"`, body: `{"repos":{"add":["alice/docs"]}}`, status: http.StatusPreconditionFailed},
		{name: "weak etag never matches", user: selfServiceTestUser, ifMatch: "weak", body: `{"repos":{"add":["alice/docs"]}}`, status: http.StatusPreconditionFailed},
		{name: "missing if-match", user: selfServiceTestUser, ifMatch: "", body: `{"repos":{"add":["alice/docs"]}}`, status: http.StatusPreconditionRequired},
		{name: "someone else's registration", user: selfServiceTestOther, ifMatch: "current", body: `{"repos":{"add":["alice/docs"]}}`, status: http.StatusNotFound},
		{name: "hub admin cannot edit", user: selfServiceTestAdmin, ifMatch: "current", body: `{"dashboard_url":"https://x.example.org/"}`, status: http.StatusNotFound},
		{name: "not admin of added repo", user: selfServiceTestUser, ifMatch: "current", body: `{"repos":{"add":["alice/readonly"]}}`, status: http.StatusForbidden},
		{name: "added repo invisible to token", user: selfServiceTestUser, ifMatch: "current", body: `{"repos":["alice/lab","alice/secret"]}`, status: http.StatusForbidden},
		{name: "org is not editable", user: selfServiceTestUser, ifMatch: "current", body: `{"org":"evil"}`, status: http.StatusBadRequest},
		{name: "key is not editable", user: selfServiceTestUser, ifMatch: "current", body: `{"key_hash":"00"}`, status: http.StatusBadRequest},
		{name: "empty patch", user: selfServiceTestUser, ifMatch: "current", body: `{}`, status: http.StatusBadRequest},
		{name: "remove every repo", user: selfServiceTestUser, ifMatch: "current", body: `{"repos":{"remove":["alice/lab","alice/tools"]}}`, status: http.StatusBadRequest},
		{name: "add and remove same repo", user: selfServiceTestUser, ifMatch: "current", body: `{"repos":{"add":["alice/docs"],"remove":["alice/docs"]}}`, status: http.StatusBadRequest},
		{name: "bad repo ref", user: selfServiceTestUser, ifMatch: "current", body: `{"repos":{"add":["../x"]}}`, status: http.StatusBadRequest},
		{name: "http dashboard", user: selfServiceTestUser, ifMatch: "current", body: `{"dashboard_url":"http://lab.example.org/"}`, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			id, key := env.registerHappy(t)
			ifMatch := tc.ifMatch
			switch ifMatch {
			case "current":
				ifMatch = env.etagOf(t, id)
			case "weak":
				ifMatch = "W/" + env.etagOf(t, id)
			case "stale":
				ifMatch = env.etagOf(t, id)
				// Another operator edits first.
				if rec := env.v1Patch(t, selfServiceTestUser, id, ifMatch, `{"dashboard_url":"https://other.example.org/"}`); rec.Code != http.StatusOK {
					t.Fatalf("first PATCH: %d %s", rec.Code, rec.Body.String())
				}
			}
			before, _ := env.s.selfService.get(id)

			rec := env.v1Patch(t, tc.user, id, ifMatch, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			after, _ := env.s.selfService.get(id)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("refused PATCH changed the record:\nbefore %+v\nafter  %+v", before, after)
			}
			if _, ok := env.s.verifyHeartbeatBearerGeneration(key, id); !ok {
				t.Fatal("refused PATCH disturbed the key")
			}
			if tc.status == http.StatusForbidden && !strings.Contains(env.logs.String(), "reason="+selfServiceRefusedNotAdmin) {
				t.Fatal("no not_repo_admin audit line")
			}
			if tc.status == http.StatusPreconditionFailed && rec.Header().Get("ETag") != selfServiceETag(after) {
				t.Fatal("412 does not carry the current ETag")
			}
		})
	}
}

func TestHubV1ConfigurableRepoCap(t *testing.T) {
	repos := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("alice/bulk-%02d", i)
		}
		return out
	}
	quoted := func(rs []string) string {
		b, _ := json.Marshal(rs)
		return string(b)
	}
	cases := []struct {
		name        string
		env         string
		registerN   int
		wantCreate  int
		patchAddN   int // extra repos a PATCH adds after a successful create
		wantPatch   int
		wantPatchIn string
	}{
		{name: "default 20 refuses 21 on create", env: "", registerN: 21, wantCreate: http.StatusBadRequest},
		{name: "default 20 accepts 20", env: "", registerN: 20, wantCreate: http.StatusCreated},
		{name: "raised cap accepts 21", env: "25", registerN: 21, wantCreate: http.StatusCreated},
		{name: "patch past the cap is 413 naming the limit", env: "22", registerN: 21, wantCreate: http.StatusCreated, patchAddN: 2, wantPatch: http.StatusRequestEntityTooLarge, wantPatchIn: `"limit":"max_repos"`},
		{name: "patch up to the cap", env: "22", registerN: 21, wantCreate: http.StatusCreated, patchAddN: 1, wantPatch: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			t.Setenv(selfServiceMaxReposEnv, tc.env)
			all := repos(tc.registerN + tc.patchAddN)
			body := fmt.Sprintf(`{"name":"big","repos":%s}`, quoted(all[:tc.registerN]))
			// The v6 route reads the same cap.
			rec := env.register(t, selfServiceTestUser, body)
			if rec.Code != tc.wantCreate {
				t.Fatalf("create: %d, want %d (%s)", rec.Code, tc.wantCreate, rec.Body.String())
			}
			if tc.patchAddN == 0 {
				return
			}
			id := "byo-alice--big"
			patch := fmt.Sprintf(`{"repos":{"add":%s}}`, quoted(all[tc.registerN:]))
			before, _ := env.s.selfService.get(id)
			rec = env.v1Patch(t, selfServiceTestUser, id, env.etagOf(t, id), patch)
			if rec.Code != tc.wantPatch {
				t.Fatalf("PATCH: %d, want %d (%s)", rec.Code, tc.wantPatch, rec.Body.String())
			}
			if tc.wantPatchIn != "" && !strings.Contains(rec.Body.String(), tc.wantPatchIn) {
				t.Fatalf("PATCH body %s lacks %s", rec.Body.String(), tc.wantPatchIn)
			}
			after, _ := env.s.selfService.get(id)
			if tc.wantPatch != http.StatusOK && !reflect.DeepEqual(before, after) {
				t.Fatal("refused PATCH changed the record")
			}
			if tc.wantPatch == http.StatusOK && len(after.Repos) != tc.registerN+tc.patchAddN {
				t.Fatalf("repos after PATCH = %d", len(after.Repos))
			}
		})
	}
}

func TestHubV1RotateAndDelete(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		suffix     string
		user       string
		ifMatch    string // "current", "stale" or ""
		body       string
		wantStatus int
	}{
		{name: "rotate without precondition", method: http.MethodPost, suffix: "/keys", user: selfServiceTestUser, wantStatus: http.StatusCreated},
		{name: "rotate with current etag", method: http.MethodPost, suffix: "/keys", user: selfServiceTestUser, ifMatch: "current", wantStatus: http.StatusCreated},
		{name: "rotate with stale etag", method: http.MethodPost, suffix: "/keys", user: selfServiceTestUser, ifMatch: "stale", wantStatus: http.StatusPreconditionFailed},
		{name: "rotate grace is not offered yet", method: http.MethodPost, suffix: "/keys", user: selfServiceTestUser, body: `{"grace":"1h"}`, wantStatus: http.StatusBadRequest},
		{name: "rotate someone else's", method: http.MethodPost, suffix: "/keys", user: selfServiceTestOther, wantStatus: http.StatusNotFound},
		{name: "delete with stale etag", method: http.MethodDelete, user: selfServiceTestUser, ifMatch: "stale", wantStatus: http.StatusPreconditionFailed},
		{name: "delete with current etag", method: http.MethodDelete, user: selfServiceTestUser, ifMatch: "current", wantStatus: http.StatusOK},
		{name: "delete by hub admin", method: http.MethodDelete, user: selfServiceTestAdmin, wantStatus: http.StatusOK},
		{name: "delete someone else's", method: http.MethodDelete, user: selfServiceTestOther, wantStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			id, key := env.registerHappy(t)
			ifMatch := ""
			switch tc.ifMatch {
			case "current":
				ifMatch = env.etagOf(t, id)
			case "stale":
				ifMatch = env.etagOf(t, id)
				if rec := env.v1Patch(t, selfServiceTestUser, id, ifMatch, `{"dashboard_url":""}`); rec.Code != http.StatusOK {
					t.Fatalf("setup PATCH: %d", rec.Code)
				}
			}
			headers := map[string]string{}
			if ifMatch != "" {
				headers["If-Match"] = ifMatch
			}
			handler := env.s.handleHubV1Delete
			if tc.suffix == "/keys" {
				handler = env.s.handleHubV1RotateKey
			}
			rec := env.v1(t, tc.user, tc.method, hubV1BasePath+"/registrations/"+id+tc.suffix, tc.body, headers, handler)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			_, stillThere := env.s.selfService.get(id)
			_, keyOK := env.s.verifyHeartbeatBearerGeneration(key, id)
			switch {
			case tc.method == http.MethodDelete && tc.wantStatus == http.StatusOK:
				if stillThere || keyOK {
					t.Fatal("delete left the record or the key alive")
				}
			case tc.suffix == "/keys" && tc.wantStatus == http.StatusCreated:
				var resp struct {
					Key string `json:"key"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &resp)
				if _, ok := env.s.verifyHeartbeatBearerGeneration(resp.Key, id); !ok || resp.Key == key {
					t.Fatal("rotation did not mint a working new key")
				}
				if !keyOK {
					t.Fatal("old key refused inside the rotation overlap")
				}
				if stored, _ := env.s.selfService.get(id); rec.Header().Get("ETag") != selfServiceETag(stored) {
					t.Fatal("rotation response ETag is not the record's")
				}
			default:
				if !stillThere || !keyOK {
					t.Fatal("refused request removed the record or broke the key")
				}
			}
		})
	}
}

// TestHubV1LegacyRoutesUnchanged pins the v6 /api/hub/selfservice/* response
// shapes so the v1 work cannot drift them, and checks that a v1 edit is
// visible through the v6 list.
func TestHubV1LegacyRoutesUnchanged(t *testing.T) {
	topKeys := func(t *testing.T, rec *httptest.ResponseRecorder) []string {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("body is not a JSON object: %s", rec.Body.String())
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	env := newHubV1TestEnv(t)

	reg := env.register(t, selfServiceTestUser, selfServiceHappyBody)
	list := env.list(t, selfServiceTestUser)
	status := env.status(t, selfServiceTestUser)
	cases := []struct {
		name     string
		rec      *httptest.ResponseRecorder
		wantCode int
		wantKeys []string
	}{
		{name: "register", rec: reg, wantCode: http.StatusCreated, wantKeys: []string{"hive", "hub_url", "key", "snippets"}},
		{name: "list", rec: list, wantCode: http.StatusOK, wantKeys: []string{"hives"}},
		{name: "status", rec: status, wantCode: http.StatusOK, wantKeys: []string{"count", "enabled", "expiry", "max_per_user"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (%s)", tc.rec.Code, tc.wantCode, tc.rec.Body.String())
			}
			if got := topKeys(t, tc.rec); !reflect.DeepEqual(got, tc.wantKeys) {
				t.Fatalf("keys = %v, want %v", got, tc.wantKeys)
			}
			if tc.rec.Header().Get("ETag") != "" {
				t.Fatal("v6 route started sending an ETag")
			}
			for _, leaked := range []string{`"etag"`, `"revision"`, `"key_hash"`} {
				if strings.Contains(tc.rec.Body.String(), leaked) {
					t.Fatalf("v6 body gained %s: %s", leaked, tc.rec.Body.String())
				}
			}
		})
	}

	// A v1 PATCH is visible through the v6 list, and the v6 rotate and delete
	// still answer as before.
	if rec := env.v1Patch(t, selfServiceTestUser, hubV1TestID, env.etagOf(t, hubV1TestID), `{"repos":{"add":["alice/docs"]}}`); rec.Code != http.StatusOK {
		t.Fatalf("v1 PATCH: %d %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Hives []selfServiceHiveView `json:"hives"`
	}
	if err := json.Unmarshal(env.list(t, selfServiceTestUser).Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Hives) != 1 || len(listed.Hives[0].Repos) != 3 {
		t.Fatalf("v6 list after v1 PATCH = %+v", listed.Hives)
	}
	rot := env.rotate(t, selfServiceTestUser, hubV1TestID)
	if rot.Code != http.StatusOK || !reflect.DeepEqual(topKeys(t, rot), []string{"hub_url", "id", "key", "overlap", "snippets"}) {
		t.Fatalf("v6 rotate: %d %s", rot.Code, rot.Body.String())
	}
	del := env.remove(t, selfServiceTestUser, hubV1TestID)
	if del.Code != http.StatusOK || !reflect.DeepEqual(topKeys(t, del), []string{"id", "removed"}) {
		t.Fatalf("v6 delete: %d %s", del.Code, del.Body.String())
	}
}
