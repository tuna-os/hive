package main

import (
	"strings"
	"testing"
	"time"
)

func backendsOK() *Fetched {
	return &Fetched{URL: "https://school.tunaos.org/api/config/backends", Status: 200}
}

// modelRaw is a session-tier Raw with a healthy agent, the given /api/status
// agents, and the given backend catalog.
func modelRaw(t *testing.T, agents []StatusAgent, backends []BackendCatalog, fetch *Fetched) Raw {
	t.Helper()
	deepAgents := map[string]DeepAgent{}
	for _, a := range agents {
		deepAgents[a.Name] = running(time.Minute)
	}
	if len(deepAgents) == 0 {
		deepAgents["supervisor"] = running(time.Minute)
	}
	r := rawWith(t, deep(t, deepAgents, nil))
	if len(agents) > 0 {
		r.Status, r.StatusSource = &StatusDoc{Agents: agents}, "session"
	}
	r.Backends, r.BackendsFetch = backends, fetch
	return r
}

func prevWith(models ...BackendModels) *HiveReport {
	return &HiveReport{Name: "school", Models: models}
}

func TestPublicTierHasNoModelChecks(t *testing.T) {
	h := Evaluate(publicRaw(t, "school"), nil, DefaultThresholds(), fixtureNow)
	for _, c := range h.Checks {
		if strings.HasPrefix(c.ID, "model-") {
			t.Fatalf("public tier reported %q without ever reading the model list", c.ID)
		}
	}
	if h.Models != nil {
		t.Fatalf("census recorded without a backends fetch: %+v", h.Models)
	}
}

func TestUnreadableModelListWarns(t *testing.T) {
	r := modelRaw(t, []StatusAgent{{Name: "a", State: "running", Cadence: "5m", CLI: "claude", Model: "claude-opus-5"}},
		nil, &Fetched{URL: "https://school.tunaos.org/api/config/backends", Status: 500})
	h := Evaluate(r, nil, DefaultThresholds(), fixtureNow)
	c := mustCheck(t, h, "model-discovery", Warn)
	if len(c.Evidence) == 0 {
		t.Fatal("no evidence for an unreadable model list")
	}
}

func TestStaticFallbackWarnsOnlyForBackendsInUse(t *testing.T) {
	agents := []StatusAgent{{Name: "a", State: "running", Cadence: "5m", CLI: "claude", Model: "claude-opus-5"}}
	unused := []BackendCatalog{
		{ID: "claude", Models: []string{"claude-opus-5"}},
		{ID: "gemini", Models: []string{"gemini-3.6-flash"}, Fallback: true},
	}
	h := Evaluate(modelRaw(t, agents, unused, backendsOK()), nil, DefaultThresholds(), fixtureNow)
	mustCheck(t, h, "model-discovery", Pass)
	if h.Level != Pass {
		t.Fatalf("an unused backend's failed discovery changed the verdict: %s %v", h.Level, ids(h.Checks))
	}

	inUse := []BackendCatalog{{ID: "claude", Models: []string{"claude-opus-5"}, Fallback: true}}
	h = Evaluate(modelRaw(t, agents, inUse, backendsOK()), nil, DefaultThresholds(), fixtureNow)
	c := mustCheck(t, h, "model-discovery", Warn)
	if !strings.Contains(strings.Join(c.Evidence, " "), "claude") {
		t.Fatalf("evidence does not name the degraded backend: %v", c.Evidence)
	}
}

func TestNewModelsAreReportedWithoutChangingTheVerdict(t *testing.T) {
	now := []BackendCatalog{{ID: "gemini", Models: []string{"gemini-3.6-flash", "gemini-3.7-flash"}}}
	prev := prevWith(BackendModels{ID: "gemini", Models: []string{"gemini-3.6-flash"}})
	agents := []StatusAgent{{Name: "a", State: "running", Cadence: "5m", CLI: "gemini", Model: "gemini-3.6-flash"}}

	h := Evaluate(modelRaw(t, agents, now, backendsOK()), prev, DefaultThresholds(), fixtureNow)
	c := mustCheck(t, h, "model-new", Info)
	if !strings.Contains(strings.Join(c.Evidence, " "), "gemini-3.7-flash") {
		t.Fatalf("new model not named: %v", c.Evidence)
	}
	if h.Level != Pass {
		t.Fatalf("a newly offered model made the Hive unhealthy: %s", h.Level)
	}
	// The agent stays on its configured model: the probe reports, never adopts.
	if a := agentOf(t, h, "a"); a.Level == Fail {
		t.Fatalf("agent flagged for not being on the new model: %+v", a)
	}
}

func TestNewModelsNeedTwoEvidentialSamples(t *testing.T) {
	now := []BackendCatalog{{ID: "gemini", Models: []string{"gemini-3.6-flash", "gemini-3.7-flash"}}}
	cases := map[string]*HiveReport{
		"no previous run":    nil,
		"previous was empty": prevWith(BackendModels{ID: "gemini"}),
		"previous was static fallback": prevWith(BackendModels{ID: "gemini",
			Models: []string{"gemini-3.6-flash"}, Fallback: true}),
	}
	for name, prev := range cases {
		h := Evaluate(modelRaw(t, nil, now, backendsOK()), prev, DefaultThresholds(), fixtureNow)
		if c, ok := h.Find("model-new"); ok {
			t.Fatalf("%s: diffed against a sample that proves nothing: %s", name, c.Summary)
		}
	}
}

func TestRetiredModelIDWarnsAfterTwoRuns(t *testing.T) {
	agents := []StatusAgent{{Name: "quality", State: "running", Cadence: "5m", CLI: "claude", Model: "claude-opus-4-5"}}
	now := []BackendCatalog{{ID: "claude", Models: []string{"claude-opus-5", "claude-haiku-5"}}}

	// First sighting: the live list is nondeterministic mid-rollout, so one
	// sample is not proof.
	stillThere := prevWith(BackendModels{ID: "claude", Models: []string{"claude-opus-4-5", "claude-opus-5"}})
	h := Evaluate(modelRaw(t, agents, now, backendsOK()), stillThere, DefaultThresholds(), fixtureNow)
	if c, ok := h.Find("model-drift/quality"); ok {
		t.Fatalf("warned on a single sample: %s", c.Summary)
	}

	gone := prevWith(BackendModels{ID: "claude", Models: []string{"claude-opus-5"}})
	h = Evaluate(modelRaw(t, agents, now, backendsOK()), gone, DefaultThresholds(), fixtureNow)
	c := mustCheck(t, h, "model-drift/quality", Warn)
	if !strings.Contains(c.Summary, "claude-opus-4-5") {
		t.Fatalf("summary does not name the configured model: %s", c.Summary)
	}
}

func TestDriftIgnoresUnmeasurableBackends(t *testing.T) {
	gone := prevWith(
		BackendModels{ID: "claude", Models: []string{"claude-opus-5"}},
		BackendModels{ID: "litellm", Models: []string{"deepseek-v4-flash"}},
	)
	cases := map[string]struct {
		agents   []StatusAgent
		backends []BackendCatalog
	}{
		"backend not in the catalog at all": {
			agents:   []StatusAgent{{Name: "a", State: "running", Cadence: "5m", CLI: "pi", Model: "pi-1"}},
			backends: []BackendCatalog{{ID: "claude", Models: []string{"claude-opus-5"}}},
		},
		"backend served a static fallback": {
			agents:   []StatusAgent{{Name: "a", State: "running", Cadence: "5m", CLI: "claude", Model: "claude-opus-4-5"}},
			backends: []BackendCatalog{{ID: "claude", Models: []string{"claude-opus-5"}, Fallback: true}},
		},
		"inference backends carry no fallback flag": {
			agents:   []StatusAgent{{Name: "a", State: "running", Cadence: "5m", CLI: "litellm", Model: "deepseek-v4-pro"}},
			backends: []BackendCatalog{{ID: "litellm", Models: []string{"deepseek-v4-flash"}, Inference: true}},
		},
		"agent has no model recorded": {
			agents:   []StatusAgent{{Name: "a", State: "running", Cadence: "5m", CLI: "claude"}},
			backends: []BackendCatalog{{ID: "claude", Models: []string{"claude-opus-5"}}},
		},
	}
	for name, tc := range cases {
		h := Evaluate(modelRaw(t, tc.agents, tc.backends, backendsOK()), gone, DefaultThresholds(), fixtureNow)
		if c, ok := h.Find("model-drift/a"); ok {
			t.Fatalf("%s: %s", name, c.Summary)
		}
	}
}

func TestSpellingDriftIsNotModelDrift(t *testing.T) {
	// Same model, different spellings on the two sides: separator drift and a
	// provider prefix must never read as a retired id.
	for _, tc := range []struct{ configured, offered string }{
		{"claude-opus-4.6", "claude-opus-4-6"},
		{"claude-opus-4-6", "claude-opus-4.6"},
		{"gpt-5", "openai/gpt-5"},
		{"openai/gpt-5", "gpt-5"},
		{"Claude-Opus-5", "claude-opus-5"},
	} {
		agents := []StatusAgent{{Name: "a", State: "running", Cadence: "5m", CLI: "claude", Model: tc.configured}}
		now := []BackendCatalog{{ID: "claude", Models: []string{tc.offered}}}
		prev := prevWith(BackendModels{ID: "claude", Models: []string{tc.offered}})
		h := Evaluate(modelRaw(t, agents, now, backendsOK()), prev, DefaultThresholds(), fixtureNow)
		if c, ok := h.Find("model-drift/a"); ok {
			t.Fatalf("%q vs %q: %s", tc.configured, tc.offered, c.Summary)
		}
	}
}

func TestCensusIsCarriedIntoTheReport(t *testing.T) {
	backends := []BackendCatalog{
		{ID: "claude", Models: []string{"claude-opus-5", "claude-haiku-5"}},
		{ID: "vllm", Models: []string{"qwen3"}, Inference: true},
	}
	h := Evaluate(modelRaw(t, nil, backends, backendsOK()), nil, DefaultThresholds(), fixtureNow)
	if len(h.Models) != 1 || h.Models[0].ID != "claude" {
		t.Fatalf("census = %+v, want the CLI backends only", h.Models)
	}
	// Sorted, so the next run's diff compares like with like.
	if got := h.Models[0].Models; got[0] != "claude-haiku-5" || got[1] != "claude-opus-5" {
		t.Fatalf("census models not sorted: %v", got)
	}
}
