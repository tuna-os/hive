package extwork

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agentmode"
)

// declaringAdapter is the fake adapter plus a declared posture list.
type declaringAdapter struct {
	*fakeAdapter
	modes []string
}

func (d declaringAdapter) Postures() []string { return d.modes }

func TestPostureTable(t *testing.T) {
	cases := []struct {
		mode               string
		starts, hiveWrites bool
		ceiling            agentmode.AgentMode
		untrusted          bool
	}{
		{ModeOff, false, false, agentmode.ModeAdvisory, false},
		{ModeShadow, false, false, agentmode.ModeAdvisory, false},
		{ModeReportOnly, true, false, agentmode.ModeAdvisory, false},
		{ModeToolHosted, true, true, agentmode.ModeIssuesAndPRs, false},
		{ModeGitHubMediated, true, false, agentmode.ModeAdvisory, true},
	}
	for _, c := range cases {
		p, ok := PostureFor(c.mode)
		if !ok || !ValidMode(c.mode) {
			t.Fatalf("%s: unknown", c.mode)
		}
		if p.StartsWork != c.starts || p.HiveExecutesWrites != c.hiveWrites || p.Ceiling != c.ceiling || p.UntrustedOutput != c.untrusted {
			t.Errorf("%s: posture = %+v", c.mode, p)
		}
	}
	if _, ok := PostureFor("enforce"); ok || ValidMode("enforce") {
		t.Fatal("enforce must stay invalid")
	}
}

func TestClampModeNeverGrantsMerge(t *testing.T) {
	for _, m := range []string{ModeOff, ModeShadow, ModeReportOnly, ModeToolHosted, ModeGitHubMediated, "bogus"} {
		if got := ClampMode(m, agentmode.ModeIssuesPRsMerge); got.CanMerge() {
			t.Errorf("%s: clamp kept merge (%v)", m, got)
		}
	}
	if got := ClampMode(ModeToolHosted, agentmode.ModeIssuesPRsMerge); got != agentmode.ModeIssuesAndPRs {
		t.Fatalf("tool-hosted clamp = %v", got)
	}
	if got := ClampMode(ModeToolHosted, agentmode.ModeIssuesOnly); got != agentmode.ModeIssuesOnly {
		t.Fatalf("clamp raised a lower request: %v", got)
	}
	if got := ClampMode(ModeGitHubMediated, agentmode.ModeIssuesAndPRs); got != agentmode.ModeAdvisory {
		t.Fatalf("github-mediated granted %v", got)
	}
}

func TestBindingRefusesUndeclaredPosture(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{ModeToolHosted, ModeGitHubMediated} {
		adm := testAdmission()
		adm.Authority.Mode = mode
		if err := adm.Validate(); err != nil {
			t.Fatalf("%s: validate: %v", mode, err)
		}
		f := newFake()
		sink := &MemorySink{}
		b := New(f, NewMemoryStore(), nil, sink, mode)
		if b.Mode() != mode {
			t.Fatalf("binding mode = %q", b.Mode())
		}
		if _, err := b.Offer(ctx, nil, adm, "s"); !errors.Is(err, ErrPostureUndeclared) {
			t.Fatalf("%s offer on undeclared adapter = %v", mode, err)
		}
		if _, err := b.Dispatch(ctx, adm, testPayload()); !errors.Is(err, ErrPostureUndeclared) || f.starts != 0 {
			t.Fatalf("%s dispatch on undeclared adapter = %v (starts %d)", mode, err, f.starts)
		}
		if !contains(sink.Actions(), EventOfferDeclined) {
			t.Fatalf("refusal not audited: %v", sink.Actions())
		}
		// Declaring a different posture is not enough.
		other := declaringAdapter{fakeAdapter: newFake(), modes: []string{ModeReportOnly}}
		if _, err := New(other, NewMemoryStore(), nil, nil, mode).Dispatch(ctx, adm, testPayload()); !errors.Is(err, ErrPostureUndeclared) {
			t.Fatalf("%s with wrong declaration = %v", mode, err)
		}
		// A declaring adapter is admitted and started.
		d := declaringAdapter{fakeAdapter: newFake(), modes: []string{mode}}
		res, err := New(d, NewMemoryStore(), nil, nil, mode).Dispatch(ctx, adm, testPayload())
		if err != nil || !res.Started || d.starts != 1 {
			t.Fatalf("%s declared dispatch = %+v %v (starts %d)", mode, res, err, d.starts)
		}
	}
	// Base modes need no declaration.
	adm := testAdmission()
	if _, err := New(newFake(), NewMemoryStore(), nil, nil, ModeReportOnly).Dispatch(ctx, adm, testPayload()); err != nil {
		t.Fatalf("report-only without declaration: %v", err)
	}
}

func TestVetMediatedPR(t *testing.T) {
	good := MediatedPR{Number: 9, Author: "octo-user", ChangedFiles: []string{"src/a.go"}, ChecksPassed: true, Approvers: []string{"maintainer"}, HiveIdentities: []string{"hivecommons-hive[bot]"}}
	if err := VetMediatedPR(good, nil); err != nil {
		t.Fatalf("good PR refused: %v", err)
	}
	cases := map[string]func(*MediatedPR){
		"no PR number":             func(p *MediatedPR) { p.Number = 0 },
		"checks have not passed":   func(p *MediatedPR) { p.ChecksPassed = false },
		"protected paths":          func(p *MediatedPR) { p.ChangedFiles = append(p.ChangedFiles, ".github/workflows/ci.yml") },
		"independent":              func(p *MediatedPR) { p.Approvers = nil },
		"independent approving":    func(p *MediatedPR) { p.Approvers = []string{"Octo-User"} },
		"no independent approving": func(p *MediatedPR) { p.Approvers = []string{"HiveCommons-Hive[bot]"} },
	}
	for want, mutate := range cases {
		pr := good
		pr.ChangedFiles = append([]string(nil), good.ChangedFiles...)
		mutate(&pr)
		err := VetMediatedPR(pr, nil)
		if !errors.Is(err, ErrMediatedPR) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
	custom := good
	custom.ChangedFiles = []string{"deploy/secret.yaml"}
	if err := VetMediatedPR(custom, []string{"deploy/"}); err == nil {
		t.Fatal("custom protected list ignored")
	}
}
