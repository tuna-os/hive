package main

import (
	"fmt"
	"sort"
	"strings"
)

// Model checks (tuna-os/hive#6).
//
// A Hive picks each agent's model from a table someone maintains by hand. The
// refresher that is supposed to keep that table current needs a third-party
// benchmark API key; without one it does nothing, quietly, and the fleet sits
// on whatever was written down months ago — a newer model is only ever
// adopted if a human happens to notice.
//
// The Hive already knows better than the table does: GET /api/config/backends
// asks every CLI backend what it currently offers (src/pkg/dashboard/
// cli_models.go), with no third-party key involved. These checks read that
// list and REPORT:
//
//   - model-discovery: a backend whose list is the Hive's built-in static
//     fallback — discovery failed, so nothing here can see a new model. A
//     check that cannot do its job must not render as a pass.
//   - model-new:       models offered now that were not offered at the
//     previous run. Reported, never adopted: a model that benchmarks or
//     behaves worse for agent work must not be promoted by a probe.
//   - model-drift:     an agent configured for a model its backend no longer
//     offers — a renamed or retired id, caught here instead of at the agent's
//     next start.

// maxModelEvidence bounds how many model ids a single check quotes. The first
// run after a backend's discovery starts working lists everything it offers;
// that belongs in the report, not in a job summary table cell.
const maxModelEvidence = 12

// BackendModels is one backend's model list as the Hive served it, carried in
// the report so the next run can diff against it.
type BackendModels struct {
	ID     string   `json:"id"`
	Models []string `json:"models,omitempty"`
	// Fallback marks a list the Hive substituted because discovery failed.
	// It is recorded so a later run knows this sample proves nothing.
	Fallback bool `json:"fallback,omitempty"`
}

// normalizeModelID is the equivalence key for comparing a configured model id
// with a discovered one. It collapses the "." vs "-" separator drift between
// catalog and CLI spellings of the SAME model (claude-opus-4.6 vs
// claude-opus-4-6) exactly as the dashboard does, and is applied to BOTH
// sides — never used as a display or launch value.
func normalizeModelID(m string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(m)), ".", "-")
}

// offersModel reports whether list contains m. A provider prefix on one side
// only ("openai/gpt-5" vs "gpt-5") still counts as a match: the cost of
// missing a real drift is one quiet run, the cost of inventing one is a
// warning telling an operator to change a model that works.
func offersModel(list []string, m string) bool {
	want := normalizeModelID(m)
	if want == "" {
		return true
	}
	wantBare := want[strings.LastIndex(want, "/")+1:]
	for _, have := range list {
		got := normalizeModelID(have)
		if got == want {
			return true
		}
		if got[strings.LastIndex(got, "/")+1:] == wantBare {
			return true
		}
	}
	return false
}

// modelChecks evaluates the Hive's own model discovery. backends is GET
// /api/config/backends, fetch is that request's outcome (nil when the probe
// never asked: public tier, or an offline status file), agents is /api/status
// agents, and prev is the same Hive in the previous run's report. It returns
// the census to carry into the report plus the checks.
func modelChecks(backends []BackendCatalog, fetch *Fetched, agents []StatusAgent, prev *HiveReport) ([]BackendModels, []Check) {
	if fetch == nil {
		return nil, nil
	}
	if !fetch.OK() || len(backends) == 0 {
		return nil, []Check{{ID: "model-discovery", Kind: "model-discovery", Level: Warn,
			Summary:  "the Hive's model list could not be read, so a model that is new since the tier table was written cannot be seen",
			Evidence: []string{fetch.Describe()}}}
	}

	census := make([]BackendModels, 0, len(backends))
	live := map[string][]string{}
	for _, b := range backends {
		// Inference entries (vllm/llm-d/litellm) are served without a
		// fallback flag by this endpoint, so a degraded list is
		// indistinguishable from a real one. Diffing them produced exactly
		// the toast storms the dashboard had to stop doing (#4426, #4438).
		if b.ID == "" || b.Inference {
			continue
		}
		models := append([]string(nil), b.Models...)
		sort.Strings(models)
		census = append(census, BackendModels{ID: b.ID, Models: models, Fallback: b.Fallback})
		if !b.Fallback && len(models) > 0 {
			live[b.ID] = models
		}
	}
	sort.Slice(census, func(i, j int) bool { return census[i].ID < census[j].ID })
	if len(census) == 0 {
		return nil, nil
	}

	prevCensus := map[string]BackendModels{}
	if prev != nil {
		for _, b := range prev.Models {
			prevCensus[b.ID] = b
		}
	}
	// A sample is only evidence when discovery actually answered: a static
	// fallback list is a hand-written table, and an empty one is a hole.
	evidential := func(b BackendModels) bool { return !b.Fallback && len(b.Models) > 0 }

	checks := []Check{discoveryCheck(census, live, agents)}
	if c, ok := newModelCheck(census, prevCensus, evidential); ok {
		checks = append(checks, c)
	}
	checks = append(checks, driftChecks(live, prevCensus, agents, evidential)...)
	return census, checks
}

// discoveryCheck reports backends whose served list is the built-in static
// fallback. Only backends an agent actually runs on count: a Hive with no
// gemini agent is not unhealthy because gemini has no key.
func discoveryCheck(census []BackendModels, live map[string][]string, agents []StatusAgent) Check {
	inUse := map[string]bool{}
	for _, a := range agents {
		if a.CLI != "" {
			inUse[a.CLI] = true
		}
	}
	var degraded []string
	for _, b := range census {
		if b.Fallback && inUse[b.ID] {
			degraded = append(degraded, fmt.Sprintf("%s: discovery failed, serving a built-in list of %d model ids", b.ID, len(b.Models)))
		}
	}
	if len(degraded) > 0 {
		return Check{ID: "model-discovery", Kind: "model-discovery", Level: Warn,
			Summary: fmt.Sprintf("model discovery failed for %d backend(s) agents run on; the Hive is serving its built-in static list there, so a model added since that list was written cannot be seen",
				len(degraded)),
			Evidence: degraded}
	}
	liveBackends := 0
	total := 0
	for _, b := range census {
		if _, ok := live[b.ID]; ok {
			liveBackends++
			total += len(b.Models)
		}
	}
	if liveBackends == 0 {
		return Check{ID: "model-discovery", Kind: "model-discovery", Level: Info,
			Summary: "no CLI backend returned a discovered model list (none of the configured backends supports discovery, or none is in use)"}
	}
	return Check{ID: "model-discovery", Kind: "model-discovery", Level: Pass,
		Summary: fmt.Sprintf("model discovery answered live for %d backend(s), %d models in total", liveBackends, total)}
}

// newModelCheck lists models a backend offers now and did not offer at the
// previous run. It never changes a verdict: a new model is news for an
// operator, not a fault, and promoting one on an unverified score is worse
// than staying put.
func newModelCheck(census []BackendModels, prevCensus map[string]BackendModels, evidential func(BackendModels) bool) (Check, bool) {
	var added []string
	for _, b := range census {
		p, ok := prevCensus[b.ID]
		if !ok || !evidential(p) || !evidential(b) {
			continue
		}
		for _, m := range b.Models {
			if !offersModel(p.Models, m) {
				added = append(added, b.ID+": "+m)
			}
		}
	}
	if len(added) == 0 {
		return Check{}, false
	}
	ev := added
	if len(ev) > maxModelEvidence {
		ev = append(append([]string(nil), ev[:maxModelEvidence]...),
			fmt.Sprintf("… and %d more (full list in the JSON report)", len(added)-maxModelEvidence))
	}
	return Check{ID: "model-new", Kind: "model-refresh", Level: Info,
		Summary: fmt.Sprintf("%d model(s) are offered that were not at the previous probe run — reported only; nothing was adopted and every agent still runs the model it is configured with",
			len(added)),
		Evidence: ev}, true
}

// driftChecks flags an agent whose configured model its backend does not
// offer. The absence must hold across TWO consecutive runs: a single live
// sample is not proof — the Copilot models endpoint is nondeterministic
// mid-rollout, which is why the Hive itself only drops a model after several
// consecutive misses (cli_models.go, cliModelDropAfterMisses).
func driftChecks(live map[string][]string, prevCensus map[string]BackendModels, agents []StatusAgent, evidential func(BackendModels) bool) []Check {
	sorted := append([]StatusAgent(nil), agents...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var out []Check
	for _, a := range sorted {
		if a.Name == "" || a.CLI == "" || a.Model == "" {
			continue
		}
		models, ok := live[a.CLI]
		if !ok || offersModel(models, a.Model) {
			continue
		}
		p, ok := prevCensus[a.CLI]
		if !ok || !evidential(p) || offersModel(p.Models, a.Model) {
			continue // first sighting, or no comparable earlier sample: hold
		}
		out = append(out, Check{ID: "model-drift/" + a.Name, Kind: "model-drift", Level: Warn,
			Summary: fmt.Sprintf("%s is configured for model %q, which %s has not offered in the last two probe runs — a renamed or retired model id fails the agent at its next start",
				a.Name, a.Model, a.CLI),
			Evidence: []string{fmt.Sprintf("%s currently offers %d models, none of them %s", a.CLI, len(models), a.Model)}})
	}
	return out
}
