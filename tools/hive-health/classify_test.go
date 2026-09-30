package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureNow is when testdata/ was recorded from the live Hives
// (2026-09-26, school/reef/hive.reilly.asia + hub.tunaos.org).
var fixtureNow = time.Date(2026, 9, 26, 3, 17, 35, 0, time.UTC)

var targets = map[string]Target{
	"school": {Name: "school", URL: "https://school.tunaos.org", Namespace: "hive", SessionEnv: "HIVE_HEALTH_SESSION_SCHOOL"},
	"reef":   {Name: "reef", URL: "https://reef.tunaos.org", Namespace: "hive-reef", SessionEnv: "HIVE_HEALTH_SESSION_REEF"},
	"reilly": {Name: "reilly", URL: "https://hive.reilly.asia", Namespace: "hive-hanthor", SessionEnv: "HIVE_HEALTH_SESSION_REILLY"},
}

func load[T any](t *testing.T, name string) *T {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return &v
}

func okHealth(host string) Fetched {
	return Fetched{URL: "https://" + host + "/api/health", Status: 200, Body: []byte(`{"status":"ok"}`), Latency: 120 * time.Millisecond}
}

// publicRaw is what an unauthenticated GitHub runner sees.
func publicRaw(t *testing.T, name string) Raw {
	t.Helper()
	tg := targets[name]
	return Raw{
		Target:     tg,
		Health:     okHealth(tg.Host()),
		Deep:       load[DeepHealth](t, "deep-"+name+".json"),
		Activity:   load[ContributeActivity](t, "activity-"+name+".json"),
		Contribute: &ContributeStatus{Hub: "online", ServedSHA: "2c8584a"},
		Hub:        load[HubSnapshot](t, "hub-activity.json"),
	}
}

// authedRaw adds the recorded /api/status (fields trimmed to what the probe
// reads; liveSummary removed).
func authedRaw(t *testing.T, name string) Raw {
	r := publicRaw(t, name)
	r.Status = load[StatusDoc](t, "status-"+name+".json")
	r.StatusSource = "session"
	return r
}

func agentOf(t *testing.T, h HiveReport, name string) AgentReport {
	t.Helper()
	for _, a := range h.Agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("agent %q not in report", name)
	return AgentReport{}
}

func mustCheck(t *testing.T, h HiveReport, id string, want Level) Check {
	t.Helper()
	c, ok := h.Find(id)
	if !ok {
		t.Fatalf("check %q missing; have %v", id, ids(h.Checks))
	}
	if c.Level != want {
		t.Fatalf("check %q = %s, want %s (%s)", id, c.Level, want, c.Summary)
	}
	return c
}

func ids(cs []Check) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.ID+"="+string(c.Level))
	}
	return out
}

func TestPublicTierLiveFixturesAreHealthy(t *testing.T) {
	th := DefaultThresholds()
	for _, name := range []string{"school", "reef", "reilly"} {
		t.Run(name, func(t *testing.T) {
			h := Evaluate(publicRaw(t, name), nil, th, fixtureNow)
			if h.Level != Pass {
				t.Fatalf("level = %s, want pass; checks %v", h.Level, ids(h.Checks))
			}
			if h.AuthTier != "public" {
				t.Fatalf("tier = %s", h.AuthTier)
			}
			mustCheck(t, h, "reachability", Pass)
			mustCheck(t, h, "governor", Pass)
			mustCheck(t, h, "governor-kicks", Pass)
			mustCheck(t, h, "backlog", Info)
			// brainstorm is operator-paused everywhere.
			if a := agentOf(t, h, "brainstorm"); a.PausedBy != "pause" || a.Level != Skip {
				t.Fatalf("brainstorm = %+v", a)
			}
			// operations and telemetry are cadence-paused in every mode. The
			// public tier cannot see cadence, but must not call them stalled.
			for _, n := range []string{"operations", "telemetry"} {
				a := agentOf(t, h, n)
				if a.PausedBy != "cadence-presumed" || a.Level != Skip {
					t.Fatalf("%s = %+v, want presumed cadence-paused skip", n, a)
				}
				if _, bad := h.Find("agent/" + n + "/kick"); bad {
					t.Fatalf("%s: deliberately-off agent got a kick check", n)
				}
			}
			if len(h.RecentOutput) == 0 {
				t.Fatal("no recent output extracted from the hub snapshot")
			}
			if h.Fingerprint != "" {
				t.Fatalf("passing hive has fingerprint %q", h.Fingerprint)
			}
		})
	}
}

func TestPublicTierReilly_ReviewerOperatorPaused(t *testing.T) {
	h := Evaluate(publicRaw(t, "reilly"), nil, DefaultThresholds(), fixtureNow)
	if a := agentOf(t, h, "reviewer"); a.PausedBy != "pause" {
		t.Fatalf("reviewer = %+v", a)
	}
}

// The recorded school /api/status has reviewer (copilot) on a login screen
// and crash-looping on "login token refreshed" — the case a public probe
// cannot see and the authenticated tier must fail on.
func TestAuthedTierSchool_ReviewerOnLoginScreenFails(t *testing.T) {
	h := Evaluate(authedRaw(t, "school"), nil, DefaultThresholds(), fixtureNow)
	if h.AuthTier != "session" {
		t.Fatalf("tier = %s", h.AuthTier)
	}
	if h.Level != Fail {
		t.Fatalf("level = %s; %v", h.Level, ids(h.Checks))
	}
	c := mustCheck(t, h, "agent/reviewer/needs-login", Fail)
	if !strings.Contains(c.Summary, "copilot") || !strings.Contains(c.Summary, "2026-09-24T16:04:45Z") {
		t.Fatalf("needs-login summary lacks backend/since: %q", c.Summary)
	}
	if !strings.Contains(strings.Join(c.Evidence, " "), "watchdog mode: observe") {
		t.Fatalf("evidence should say the watchdog is only observing: %v", c.Evidence)
	}
	b := mustCheck(t, h, "agent/reviewer/blocked", Fail)
	if b.Kind != "crash-loop" || !strings.Contains(b.Evidence[0], "login token refreshed") {
		t.Fatalf("blocked = %+v", b)
	}
	// Restarts caused by the rotation CronJob are context, not a verdict.
	op := mustCheck(t, h, "agent/scanner/blocked", Info)
	if op.Kind != "crash-loop-operator" {
		t.Fatalf("scanner blocked kind = %s", op.Kind)
	}
	// Cadence-paused in every mode, authoritatively.
	for _, n := range []string{"operations", "telemetry"} {
		a := agentOf(t, h, n)
		if a.PausedBy != "cadence" || !strings.Contains(a.Reason, "every governor mode") {
			t.Fatalf("%s = %+v", n, a)
		}
	}
	// The watchdog's "alive but not producing" alerts for those two are
	// suppressed: they are off on purpose.
	for _, c := range h.Checks {
		if strings.HasPrefix(c.ID, "alert/watchdog-producing-") {
			t.Fatalf("alert for a cadence-paused agent leaked through: %s", c.ID)
		}
	}
	if h.Queue == nil || h.Queue.HoldPRs == nil || *h.Queue.HoldPRs != 112 {
		t.Fatalf("queue = %+v", h.Queue)
	}
	if h.Fingerprint == "" {
		t.Fatal("failing hive has no fingerprint")
	}
}

func TestAuthedTierReef_NeedsLoginAndQuotaAlert(t *testing.T) {
	h := Evaluate(authedRaw(t, "reef"), nil, DefaultThresholds(), fixtureNow)
	mustCheck(t, h, "agent/ci-maintainer/needs-login", Fail)
	mustCheck(t, h, "agent/quality/needs-login", Fail)
	// provider-budget-exceeded is an error alert that heals on its own.
	mustCheck(t, h, "alert/provider-budget-exceeded", Warn)
}

func TestAuthedTierReilly_OnlyInfoFromRotation(t *testing.T) {
	h := Evaluate(authedRaw(t, "reilly"), nil, DefaultThresholds(), fixtureNow)
	if h.Level == Fail {
		t.Fatalf("reilly should not fail: %v", ids(h.ChecksAt(Fail)))
	}
	mustCheck(t, h, "agent/guide/blocked", Info)
	if a := agentOf(t, h, "scanner"); a.Cadence != "4h" {
		t.Fatalf("scanner cadence = %q", a.Cadence)
	}
}

func TestUnreachableHiveFails(t *testing.T) {
	r := Raw{
		Target:    targets["school"],
		Health:    Fetched{URL: "https://school.tunaos.org/api/health", Err: "dial tcp: i/o timeout"},
		DeepFetch: Fetched{URL: "https://school.tunaos.org/api/health/deep", Status: 522},
	}
	h := Evaluate(r, nil, DefaultThresholds(), fixtureNow)
	if h.Level != Fail {
		t.Fatalf("level = %s", h.Level)
	}
	c := mustCheck(t, h, "reachability", Fail)
	if !strings.Contains(c.Evidence[0], "i/o timeout") {
		t.Fatalf("evidence = %v", c.Evidence)
	}
	mustCheck(t, h, "deep-health", Fail)
	if len(h.Agents) != 0 {
		t.Fatal("agents evaluated without deep health")
	}
}

func deep(t *testing.T, agents map[string]DeepAgent, extra map[string]any) *DeepHealth {
	t.Helper()
	checks := map[string]any{
		"agents":   agents,
		"governor": map[string]any{"status": "pass", "mode": "BUSY", "issues": 10, "prs": 5, "hold": 3},
		"queue":    map[string]any{"status": "pass", "actionable": 10},
		"config":   map[string]any{"status": "pass", "hive_id": "hive-test"},
	}
	for k, v := range extra {
		checks[k] = v
	}
	b, _ := json.Marshal(map[string]any{"status": "ok", "checks": checks})
	var d DeepHealth
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

func kickAgo(d time.Duration) string { return fixtureNow.Add(-d).Format(time.RFC3339) }

func running(ago time.Duration) DeepAgent {
	return DeepAgent{State: "running", Status: "pass", LastKick: kickAgo(ago), LastPromptLen: 1000}
}

func rawWith(t *testing.T, d *DeepHealth) Raw {
	return Raw{Target: targets["school"], Health: okHealth("school.tunaos.org"), Deep: d}
}

func TestAgentNotRunningFails(t *testing.T) {
	d := deep(t, map[string]DeepAgent{
		"scanner":    running(5 * time.Minute),
		"supervisor": {State: "stopped", Status: "fail", LastKick: kickAgo(time.Hour)},
	}, nil)
	h := Evaluate(rawWith(t, d), nil, DefaultThresholds(), fixtureNow)
	mustCheck(t, h, "agent/supervisor/state", Fail)
	if h.Level != Fail {
		t.Fatalf("level = %s", h.Level)
	}
}

func TestDisabledAgentIsSkipped(t *testing.T) {
	d := deep(t, map[string]DeepAgent{
		"scanner": running(5 * time.Minute),
		"guide":   {State: "stopped", Status: "skip", Disabled: true},
	}, nil)
	h := Evaluate(rawWith(t, d), nil, DefaultThresholds(), fixtureNow)
	if a := agentOf(t, h, "guide"); a.PausedBy != "config" || a.Level != Skip {
		t.Fatalf("guide = %+v", a)
	}
	if h.Level != Pass {
		t.Fatalf("level = %s: %v", h.Level, ids(h.Checks))
	}
}

func TestKickFreshnessUnknownCadence(t *testing.T) {
	th := DefaultThresholds()
	cases := []struct {
		age  time.Duration
		want Level // "" = no kick check
	}{
		{2 * time.Hour, ""},
		{13 * time.Hour, Warn},
		{40 * time.Hour, Fail},
	}
	for _, tc := range cases {
		d := deep(t, map[string]DeepAgent{
			"supervisor": running(time.Minute),
			"guide":      running(tc.age),
		}, nil)
		h := Evaluate(rawWith(t, d), nil, th, fixtureNow)
		c, ok := h.Find("agent/guide/kick")
		if tc.want == "" {
			if ok {
				t.Fatalf("age %s: unexpected %s", tc.age, c.Summary)
			}
			continue
		}
		if !ok || c.Level != tc.want {
			t.Fatalf("age %s: got %v, want %s", tc.age, ids(h.Checks), tc.want)
		}
	}
}

func TestKickFreshnessKnownCadenceHasFloors(t *testing.T) {
	th := DefaultThresholds()
	st := &StatusDoc{Agents: []StatusAgent{
		{Name: "supervisor", State: "running", Cadence: "1m"},
		{Name: "scanner", State: "running", Cadence: "5m"},
		{Name: "guide", State: "running", Cadence: "4h"},
	}}
	d := deep(t, map[string]DeepAgent{
		// 1m cadence, 35m since kick: a long supervisor turn, not a stall
		// (warn floor 45m).
		"supervisor": running(35 * time.Minute),
		// 5m cadence, 3h: past the 2h fail floor.
		"scanner": running(3 * time.Hour),
		// 4h cadence, 13h: 3x4h+15m = 12h15m warn, 6x = 24h15m fail.
		"guide": running(13 * time.Hour),
	}, nil)
	r := rawWith(t, d)
	r.Status, r.StatusSource = st, "session"
	h := Evaluate(r, nil, th, fixtureNow)
	if _, ok := h.Find("agent/supervisor/kick"); ok {
		t.Fatalf("supervisor flagged inside the floor: %v", ids(h.Checks))
	}
	mustCheck(t, h, "agent/scanner/kick", Fail)
	mustCheck(t, h, "agent/guide/kick", Warn)
}

func TestPresumedPauseNeedsKickedPeers(t *testing.T) {
	// Nothing has been kicked since start: that is a governor problem, not a
	// set of deliberately paused lanes.
	d := deep(t, map[string]DeepAgent{
		"operations": {State: "running", Status: "warn", Detail: noKickMessage, LastKick: kickAgo(500 * time.Hour)},
		"scanner":    {State: "running", Status: "warn", Detail: noKickMessage, LastKick: kickAgo(3 * time.Hour)},
	}, nil)
	h := Evaluate(rawWith(t, d), nil, DefaultThresholds(), fixtureNow)
	if a := agentOf(t, h, "operations"); a.PausedBy != "" {
		t.Fatalf("operations presumed paused without kicked peers: %+v", a)
	}
	mustCheck(t, h, "agent/operations/kick", Fail)
	mustCheck(t, h, "governor-kicks", Fail)
}

func TestGovernorNotKicking(t *testing.T) {
	d := deep(t, map[string]DeepAgent{
		"supervisor": running(50 * time.Minute),
		"scanner":    running(55 * time.Minute),
	}, nil)
	h := Evaluate(rawWith(t, d), nil, DefaultThresholds(), fixtureNow)
	mustCheck(t, h, "governor-kicks", Warn)

	d = deep(t, map[string]DeepAgent{"supervisor": running(3 * time.Hour)}, nil)
	h = Evaluate(rawWith(t, d), nil, DefaultThresholds(), fixtureNow)
	mustCheck(t, h, "governor-kicks", Fail)
}

func TestMissingGovernorFails(t *testing.T) {
	d := deep(t, map[string]DeepAgent{"supervisor": running(time.Minute)}, map[string]any{"governor": map[string]any{}})
	h := Evaluate(rawWith(t, d), nil, DefaultThresholds(), fixtureNow)
	mustCheck(t, h, "governor", Fail)
}

func TestDeepChecksMapped(t *testing.T) {
	d := deep(t, map[string]DeepAgent{
		"supervisor": running(time.Minute),
		"operations": {State: "running", Status: "warn", Detail: noKickMessage, LastKick: kickAgo(400 * time.Hour)},
	}, map[string]any{
		"github_auth":     map[string]any{"status": "fail", "detail": "GitHub App not installed"},
		"ready":           map[string]any{"status": "fail", "detail": "status not yet available"},
		"hub_heartbeat":   map[string]any{"status": "warn", "detail": "hub has not accepted a heartbeat recently"},
		"tokens":          map[string]any{"status": "warn"},
		"stall_detection": map[string]any{"status": "warn", "agents": []string{"operations"}},
	})
	d.Status = "critical"
	h := Evaluate(rawWith(t, d), nil, DefaultThresholds(), fixtureNow)
	mustCheck(t, h, "github-auth", Fail)
	mustCheck(t, h, "ready", Fail)
	mustCheck(t, h, "hub-heartbeat", Warn)
	mustCheck(t, h, "tokens", Warn)
	mustCheck(t, h, "deep-health", Fail)
	// operations is presumed cadence-paused, so its stall is filtered out.
	if _, ok := h.Find("stall-detection"); ok {
		t.Fatal("stall reported for a deliberately-off agent")
	}
}

func TestBacklogSpikeAgainstPreviousRun(t *testing.T) {
	d := deep(t, map[string]DeepAgent{"supervisor": running(time.Minute)},
		map[string]any{"governor": map[string]any{"status": "pass", "mode": "SURGE", "issues": 300, "prs": 90, "hold": 400}})
	prev := &HiveReport{Queue: &QueueSnapshot{Issues: 290, PRs: 88, Hold: 300, Actionable: 10}}
	h := Evaluate(rawWith(t, d), prev, DefaultThresholds(), fixtureNow)
	c := mustCheck(t, h, "backlog-spike", Warn)
	if len(c.Evidence) != 1 || !strings.Contains(c.Evidence[0], "on hold 300 -> 400") {
		t.Fatalf("evidence = %v", c.Evidence)
	}
	// Small moves are not spikes.
	prev.Queue.Hold = 390
	h = Evaluate(rawWith(t, d), prev, DefaultThresholds(), fixtureNow)
	mustCheck(t, h, "backlog", Info)
}

func TestRejectedSessionFallsBackToPublic(t *testing.T) {
	r := publicRaw(t, "school")
	r.StatusSource = "session"
	r.StatusFetch = &Fetched{URL: "https://school.tunaos.org/api/status", Status: 401}
	h := Evaluate(r, nil, DefaultThresholds(), fixtureNow)
	if h.AuthTier != "session-rejected" {
		t.Fatalf("tier = %s", h.AuthTier)
	}
	mustCheck(t, h, "auth-tier", Warn)
	if a := agentOf(t, h, "operations"); a.PausedBy != "cadence-presumed" {
		t.Fatalf("public fallback lost: %+v", a)
	}
}

func TestProviderBlockedAndGenericBlocked(t *testing.T) {
	st := &StatusDoc{Agents: []StatusAgent{
		{Name: "a", State: "running", Cadence: "5m", StructuredStatus: "BLOCKED", StatusEvidence: "blocked: inference (quota): 429"},
		{Name: "b", State: "running", Cadence: "5m", StructuredStatus: "BLOCKED", StatusEvidence: "blocked: binary not found (3 consecutive failed starts)"},
	}}
	d := deep(t, map[string]DeepAgent{"a": running(time.Minute), "b": running(time.Minute)}, nil)
	r := rawWith(t, d)
	r.Status, r.StatusSource = st, "status-file"
	h := Evaluate(r, nil, DefaultThresholds(), fixtureNow)
	if h.AuthTier != "status-file" {
		t.Fatalf("tier = %s", h.AuthTier)
	}
	mustCheck(t, h, "agent/a/blocked", Warn)
	mustCheck(t, h, "agent/b/blocked", Fail)
}

func TestLoginGraceWarnsFirst(t *testing.T) {
	st := &StatusDoc{Agents: []StatusAgent{{Name: "a", State: "running", Cadence: "5m", NeedsLogin: true, CLI: "claude",
		Conditions: []Condition{{Type: "Authenticated", Status: "False", LastTransitionTime: fixtureNow.Add(-5 * time.Minute)}}}}}
	d := deep(t, map[string]DeepAgent{"a": running(time.Minute)}, nil)
	r := rawWith(t, d)
	r.Status, r.StatusSource = st, "session"
	h := Evaluate(r, nil, DefaultThresholds(), fixtureNow)
	mustCheck(t, h, "agent/a/needs-login", Warn)
}

func TestNotReadyAndOutputStale(t *testing.T) {
	st := &StatusDoc{Agents: []StatusAgent{{Name: "a", State: "running", Cadence: "1h",
		Conditions: []Condition{{Type: "Ready", Status: "False", Reason: "no-ready-signature", LastTransitionTime: fixtureNow.Add(-2 * time.Hour)}}}}}
	d := deep(t, map[string]DeepAgent{"a": running(10 * time.Minute)}, nil)
	r := rawWith(t, d)
	r.Status, r.StatusSource = st, "session"
	old := fixtureNow.Add(-30 * time.Hour)
	r.Hub = &HubSnapshot{GeneratedAt: fixtureNow, Spokes: []HubSpoke{{Spoke: "s", HiveID: "hive-test"}},
		Beads: []HubBead{{Spoke: "s", Agent: "a", Title: "did a thing", Updated: old}}}
	h := Evaluate(r, nil, DefaultThresholds(), fixtureNow)
	mustCheck(t, h, "agent/a/not-ready", Warn)
	mustCheck(t, h, "agent/a/output", Warn)
	mustCheck(t, h, "production", Warn)
}

func TestHubChecks(t *testing.T) {
	th := DefaultThresholds()
	if cs := HubChecks(nil, Fetched{}, th, fixtureNow); cs != nil {
		t.Fatal("no hub URL should mean no hub checks")
	}
	cs := HubChecks(nil, Fetched{URL: "https://hub/x", Status: 502}, th, fixtureNow)
	if cs[0].Level != Warn {
		t.Fatal(cs)
	}
	cs = HubChecks(&HubSnapshot{GeneratedAt: fixtureNow.Add(-2 * time.Hour)}, Fetched{URL: "https://hub/x"}, th, fixtureNow)
	if cs[0].Level != Warn {
		t.Fatal(cs)
	}
	hs := load[HubSnapshot](t, "hub-activity.json")
	cs = HubChecks(hs, Fetched{URL: "https://hub/x"}, th, fixtureNow)
	if cs[0].Level != Pass {
		t.Fatal(cs)
	}
}

func TestWatchdogAlertAgent(t *testing.T) {
	for id, want := range map[string]string{
		"watchdog-producing-operations":    "operations",
		"watchdog-producing-ci-maintainer": "ci-maintainer",
		"provider-budget-exceeded":         "",
		"watchdog-x":                       "",
	} {
		if got := watchdogAlertAgent(id); got != want {
			t.Errorf("%s: got %q want %q", id, got, want)
		}
	}
	c, ok := alertCheck(SystemAlert{ID: "watchdog-crashloop-scanner", Severity: "error", Message: "x"}, map[string]bool{})
	if !ok || c.Level != Fail {
		t.Fatalf("%+v", c)
	}
	c, _ = alertCheck(SystemAlert{ID: "other", Severity: "note"}, nil)
	if c.Level != Info {
		t.Fatalf("%+v", c)
	}
}

func TestParseCadence(t *testing.T) {
	for in, want := range map[string]time.Duration{"5m": 5 * time.Minute, "4h": 4 * time.Hour, "paused": 0, "": 0, "on demand": 0, "-1m": 0} {
		got, ok := parseCadence(in)
		if got != want || ok != (want > 0) {
			t.Errorf("%q: %v %v", in, got, ok)
		}
	}
}

func TestHumanAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second:             "30s",
		12 * time.Minute:             "12m",
		3*time.Hour + 10*time.Minute: "3h10m",
		5 * time.Hour:                "5h",
		100 * time.Hour:              "4d4h",
		72 * time.Hour:               "3d",
		-time.Second:                 "0s",
	} {
		if got := humanAge(d); got != want {
			t.Errorf("%v: %q want %q", d, got, want)
		}
	}
}

func TestGitHubRateLimitNearlyExhausted(t *testing.T) {
	st := &StatusDoc{Agents: []StatusAgent{{Name: "a", State: "running", Cadence: "5m"}}}
	st.GHRateLimits.Core.Limit, st.GHRateLimits.Core.Remaining = 5000, 12
	d := deep(t, map[string]DeepAgent{"a": running(time.Minute)}, nil)
	r := rawWith(t, d)
	r.Status, r.StatusSource = st, "session"
	h := Evaluate(r, nil, DefaultThresholds(), fixtureNow)
	mustCheck(t, h, "github-rate-limit", Warn)
	st.GHRateLimits.Core.Remaining = 4000
	h = Evaluate(r, nil, DefaultThresholds(), fixtureNow)
	if _, ok := h.Find("github-rate-limit"); ok {
		t.Fatal("healthy budget flagged")
	}
}
