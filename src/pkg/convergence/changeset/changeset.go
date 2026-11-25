// Package changeset is a bounded, read-only planning view for a coordinated
// change spanning several explicitly selected repositories (tuna-os/hive#162),
// for example a package producer feeding an image consumer, or an installer
// backend feeding its frontends.
//
// The pattern — a dependency DAG whose every edge carries an explicit
// compatibility contract, with participants declared up front — is borrowed
// from the archived frostyard/bobsled multi-repository change-set contracts
// (MIT); its runtime is not reused.
//
// Authority: a Plan grants NOTHING. It does not claim mutation scope
// (convergence/mutation), does not satisfy or weaken exact-head proof
// (convergence/proof), and does not touch the outcome ledger. It performs no
// I/O: no GitHub writes, no workers. Callers pass in already-observed facts
// (candidate head, proof result, published flag) and receive layers and
// blocking reasons. Issue text is never parsed as authority; a Spec is a typed
// value the operator supplies. Nothing consults this package unless a caller
// opts in, so convergence off/shadow/enforce behavior is unchanged.
package changeset

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/convergence/proof"
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Member is one selected repository and its candidate change.
type Member struct {
	// Repo is the canonical owner/repo spelling.
	Repo string `json:"repo"`
	// Issue is the tracking issue number (informational reference only).
	Issue int `json:"issue,omitempty"`
	// PR is the candidate pull request number.
	PR int `json:"pr"`
	// HeadSHA is the exact full 40-hex candidate head.
	HeadSHA string `json:"head_sha"`
	// Proof is the caller-observed exact-head proof result for HeadSHA. The
	// planner only reads it; it never produces or upgrades evidence. Empty or
	// unknown values count as unverified.
	Proof proof.Result `json:"proof,omitempty"`
	// Published reports that the candidate is available for consumers to
	// build against.
	Published bool `json:"published,omitempty"`
}

// Compatibility is the executable scenario a consumer must pass against the
// producer candidate, tied to exact candidate revisions.
type Compatibility struct {
	Scenario        string `json:"scenario"`
	ProducerHeadSHA string `json:"producer_head_sha"`
	ConsumerHeadSHA string `json:"consumer_head_sha"`
}

// Edge states that Consumer depends on Producer.
type Edge struct {
	Producer      string        `json:"producer"`
	Consumer      string        `json:"consumer"`
	Compatibility Compatibility `json:"compatibility"`
}

// Spec is the operator-supplied change set.
type Spec struct {
	Name    string   `json:"name"`
	Members []Member `json:"members"`
	Edges   []Edge   `json:"edges"`
}

// ValidationError lists every reason a Spec was rejected.
type ValidationError struct{ Reasons []string }

func (e *ValidationError) Error() string {
	return "invalid change set: " + strings.Join(e.Reasons, "; ")
}

// Blocked explains why a member cannot proceed yet.
type Blocked struct {
	Repo    string   `json:"repo"`
	Reasons []string `json:"reasons"`
}

// Plan is the read-only result.
type Plan struct {
	Name string `json:"name"`
	// Layers are dependency layers; members of one layer have no dependency
	// on each other and may run concurrently.
	Layers [][]string `json:"layers"`
	// Ready lists members whose producers are all published and verified.
	Ready []string `json:"ready"`
	// Blocked lists consumers waiting on unpublished or unverified producers.
	Blocked []Blocked `json:"blocked"`
}

// Build validates spec and computes the plan. It is pure.
func Build(spec Spec) (*Plan, error) {
	if reasons := validate(spec); len(reasons) > 0 {
		return nil, &ValidationError{Reasons: reasons}
	}
	members := map[string]Member{}
	for _, m := range spec.Members {
		members[m.Repo] = m
	}
	producers := map[string][]string{}
	for _, e := range spec.Edges {
		producers[e.Consumer] = append(producers[e.Consumer], e.Producer)
	}

	// Longest-path depth; validate already proved the graph acyclic.
	depth := map[string]int{}
	var depthOf func(string) int
	depthOf = func(r string) int {
		if d, ok := depth[r]; ok {
			return d
		}
		d := 0
		for _, p := range producers[r] {
			if pd := depthOf(p) + 1; pd > d {
				d = pd
			}
		}
		depth[r] = d
		return d
	}
	plan := &Plan{Name: spec.Name, Ready: []string{}, Blocked: []Blocked{}}
	for _, m := range spec.Members {
		d := depthOf(m.Repo)
		for len(plan.Layers) <= d {
			plan.Layers = append(plan.Layers, nil)
		}
		plan.Layers[d] = append(plan.Layers[d], m.Repo)
	}
	for _, l := range plan.Layers {
		sort.Strings(l)
	}

	for _, layer := range plan.Layers {
		for _, repo := range layer {
			var reasons []string
			ps := append([]string(nil), producers[repo]...)
			sort.Strings(ps)
			for _, p := range ps {
				pm := members[p]
				if !pm.Published {
					reasons = append(reasons, fmt.Sprintf("producer %s is not published", p))
				}
				if pm.Proof != proof.ResultSuccess {
					reasons = append(reasons, fmt.Sprintf("producer %s head %s is not verified (proof %q)", p, pm.HeadSHA, orUnknown(pm.Proof)))
				}
			}
			if len(reasons) == 0 {
				plan.Ready = append(plan.Ready, repo)
			} else {
				plan.Blocked = append(plan.Blocked, Blocked{Repo: repo, Reasons: reasons})
			}
		}
	}
	sort.Strings(plan.Ready)
	return plan, nil
}

func orUnknown(r proof.Result) string {
	if r == "" {
		return "unknown"
	}
	return string(r)
}

func validate(spec Spec) []string {
	var rs []string
	add := func(f string, a ...any) { rs = append(rs, fmt.Sprintf(f, a...)) }

	if strings.TrimSpace(spec.Name) == "" {
		add("change set requires a name")
	}
	if len(spec.Members) == 0 {
		add("change set declares no members")
	}
	members := map[string]Member{}
	for _, m := range spec.Members {
		if m.Repo == "" || strings.Count(m.Repo, "/") != 1 || strings.ContainsAny(m.Repo, "@#!| \t") {
			add("member repo %q is not a canonical owner/repo spelling", m.Repo)
			continue
		}
		if _, dup := members[m.Repo]; dup {
			add("duplicate member %s", m.Repo)
			continue
		}
		members[m.Repo] = m
		if m.PR < 1 {
			add("member %s requires a candidate pull request number", m.Repo)
		}
		if !shaPattern.MatchString(m.HeadSHA) {
			add("member %s head sha %q is not a full 40-hex commit id", m.Repo, m.HeadSHA)
		}
	}

	seen := map[[2]string]bool{}
	adj := map[string][]string{}
	for _, e := range spec.Edges {
		label := e.Producer + " -> " + e.Consumer
		pm, pok := members[e.Producer]
		cm, cok := members[e.Consumer]
		if !pok {
			add("edge %s: producer %q is not a declared member (undeclared participant)", label, e.Producer)
		}
		if !cok {
			add("edge %s: consumer %q is not a declared member (undeclared participant)", label, e.Consumer)
		}
		if e.Producer == e.Consumer {
			add("edge %s: a member cannot depend on itself", label)
			continue
		}
		key := [2]string{e.Producer, e.Consumer}
		if seen[key] {
			add("duplicate dependency %s", label)
			continue
		}
		seen[key] = true
		if strings.TrimSpace(e.Compatibility.Scenario) == "" {
			add("edge %s: no compatibility scenario declared", label)
		}
		if pok && e.Compatibility.ProducerHeadSHA != pm.HeadSHA {
			add("edge %s: compatibility producer head %q does not match candidate head %q", label, e.Compatibility.ProducerHeadSHA, pm.HeadSHA)
		}
		if cok && e.Compatibility.ConsumerHeadSHA != cm.HeadSHA {
			add("edge %s: compatibility consumer head %q does not match candidate head %q", label, e.Compatibility.ConsumerHeadSHA, cm.HeadSHA)
		}
		if pok && cok {
			adj[e.Producer] = append(adj[e.Producer], e.Consumer)
		}
	}

	if cyc := findCycle(spec.Members, adj); cyc != nil {
		add("dependency cycle: %s", strings.Join(cyc, " -> "))
	}
	return rs
}

// findCycle returns one cycle path (first node repeated at the end) or nil.
func findCycle(ms []Member, adj map[string][]string) []string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack, cyc []string
	var visit func(string) bool
	visit = func(n string) bool {
		color[n] = grey
		stack = append(stack, n)
		for _, next := range adj[n] {
			switch color[next] {
			case grey:
				i := len(stack) - 1
				for stack[i] != next {
					i--
				}
				cyc = append(append([]string(nil), stack[i:]...), next)
				return true
			case white:
				if visit(next) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return false
	}
	repos := make([]string, 0, len(ms))
	for _, m := range ms {
		repos = append(repos, m.Repo)
	}
	sort.Strings(repos)
	for _, r := range repos {
		if color[r] == white && visit(r) {
			return cyc
		}
	}
	return nil
}
