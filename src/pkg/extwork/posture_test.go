package extwork

import (
	"context"
	"errors"
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
	}{
		{ModeOff, false, false, agentmode.ModeAdvisory},
		{ModeShadow, false, false, agentmode.ModeAdvisory},
		{ModeReportOnly, true, false, agentmode.ModeAdvisory},
		{ModeToolHosted, true, true, agentmode.ModeIssuesAndPRs},
	}
	for _, c := range cases {
		p, ok := PostureFor(c.mode)
		if !ok || !ValidMode(c.mode) {
			t.Fatalf("%s: unknown", c.mode)
		}
		if p.StartsWork != c.starts || p.HiveExecutesWrites != c.hiveWrites || p.Ceiling != c.ceiling {
			t.Errorf("%s: posture = %+v", c.mode, p)
		}
	}
	for _, bad := range []string{"enforce", "github-mediated"} {
		if _, ok := PostureFor(bad); ok || ValidMode(bad) {
			t.Fatalf("%s must stay invalid", bad)
		}
	}
}

// TestNoVendorWriteMode pins the maintainer decision on RFC #18464: vendor
// agents never write to GitHub, so a github-mediated admission does not
// exist, validates nowhere, and a binding configured with it resolves off.
func TestNoVendorWriteMode(t *testing.T) {
	adm := testAdmission()
	adm.Authority.Mode = "github-mediated"
	if err := adm.Validate(); !errors.Is(err, ErrInvalidAdmission) {
		t.Fatalf("github-mediated admission validated: %v", err)
	}
	if b := New(newFake(), NewMemoryStore(), nil, nil, "github-mediated"); b.Mode() != ModeOff {
		t.Fatalf("binding mode = %q, want off", b.Mode())
	}
}

// TestClampModeNeverRaises: for every admission mode (known or not) and
// every requested agent mode, including out-of-range values, the clamp is
// never above the request, never above the posture's ceiling, and never
// above ISSUES_AND_PRS.
func TestClampModeNeverRaises(t *testing.T) {
	modes := []string{ModeOff, ModeShadow, ModeReportOnly, ModeToolHosted, "github-mediated", "bogus", ""}
	for _, m := range modes {
		ceiling := agentmode.ModeAdvisory
		if p, ok := PostureFor(m); ok {
			ceiling = p.Ceiling
		}
		for req := agentmode.AgentMode(-3); req <= agentmode.ModeIssuesPRsMerge+3; req++ {
			got := ClampMode(m, req)
			if got > ToolHostedCeiling || got > ceiling || got.CanMerge() {
				t.Errorf("ClampMode(%q, %d) = %v: above ceiling %v", m, req, got, ceiling)
			}
			if req >= agentmode.ModeAdvisory && got > req {
				t.Errorf("ClampMode(%q, %v) = %v: raised the request", m, req, got)
			}
			if got < agentmode.ModeAdvisory {
				t.Errorf("ClampMode(%q, %d) = %d: below ADVISORY", m, req, got)
			}
		}
	}
	if ToolHostedCeiling != agentmode.ModeIssuesAndPRs {
		t.Fatalf("tool-hosted ceiling = %v", ToolHostedCeiling)
	}
	// PostureFor hands out a copy: mutating it changes nothing.
	p, _ := PostureFor(ModeToolHosted)
	p.Ceiling = agentmode.ModeIssuesPRsMerge
	_ = p
	if got := ClampMode(ModeToolHosted, agentmode.ModeIssuesPRsMerge); got != agentmode.ModeIssuesAndPRs {
		t.Fatalf("mutated posture copy raised the clamp to %v", got)
	}
}

func TestClampModeNeverGrantsMerge(t *testing.T) {
	for _, m := range []string{ModeOff, ModeShadow, ModeReportOnly, ModeToolHosted, "bogus"} {
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
	if got := ClampMode(ModeReportOnly, agentmode.ModeIssuesAndPRs); got != agentmode.ModeAdvisory {
		t.Fatalf("report-only granted %v", got)
	}
}

func TestBindingRefusesUndeclaredPosture(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{ModeToolHosted} {
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
