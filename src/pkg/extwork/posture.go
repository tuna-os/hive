package extwork

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/crux/pushbroker"
)

// Non-CLI admission modes (RFC #18464 §2). Both start external work. They
// differ in who holds the write credential:
//
//   - ModeToolHosted: the remote side holds no GitHub credential. Every read
//     and write is a hive-tool call Hive executes under its own gates
//     (ioscan, toolapprove, pushbroker, PR-open authorization), so Hive can
//     grant up to ISSUES_AND_PRS. Merge never comes from this path.
//   - ModeGitHubMediated: the vendor writes to GitHub as a user identity
//     Hive does not mediate. Hive grants nothing; whatever lands is an
//     untrusted outside-contributor PR (see VetMediatedPR).
const (
	ModeToolHosted     = "tool-hosted"
	ModeGitHubMediated = "github-mediated"
)

// Posture is what an admission mode lets an admitted run do.
type Posture struct {
	// StartsWork is false only for off and shadow.
	StartsWork bool
	// HiveExecutesWrites is true when every side effect is a Hive-executed
	// tool call: the remote holds no credential.
	HiveExecutesWrites bool
	// Ceiling is the highest agent mode Hive's tool host may grant. It is a
	// cap, not a grant: the effective mode is min(requested, Ceiling).
	Ceiling agentmode.AgentMode
	// UntrustedOutput marks output written outside Hive's mediation.
	UntrustedOutput bool
}

var postures = map[string]Posture{
	ModeOff:            {Ceiling: agentmode.ModeAdvisory},
	ModeShadow:         {Ceiling: agentmode.ModeAdvisory},
	ModeReportOnly:     {StartsWork: true, Ceiling: agentmode.ModeAdvisory},
	ModeToolHosted:     {StartsWork: true, HiveExecutesWrites: true, Ceiling: agentmode.ModeIssuesAndPRs},
	ModeGitHubMediated: {StartsWork: true, Ceiling: agentmode.ModeAdvisory, UntrustedOutput: true},
}

// PostureFor returns the posture of mode; ok is false for an unknown mode.
func PostureFor(mode string) (Posture, bool) {
	p, ok := postures[mode]
	return p, ok
}

// ClampMode caps a requested agent mode at the admission mode's ceiling. An
// unknown admission mode clamps to ADVISORY.
func ClampMode(mode string, requested agentmode.AgentMode) agentmode.AgentMode {
	p, ok := PostureFor(mode)
	if !ok {
		return agentmode.ModeAdvisory
	}
	return min(requested, p.Ceiling)
}

// PostureDeclarer is implemented by adapters that support modes beyond
// off/shadow/report-only. A mode only raises the ceiling an adapter may
// declare; it never forces one, so the binding refuses tool-hosted and
// github-mediated for any adapter that does not list them.
type PostureDeclarer interface {
	Postures() []string
}

// ErrPostureUndeclared is returned when an admission asks for a mode the
// adapter has not declared.
var ErrPostureUndeclared = errors.New("extwork: adapter does not declare this mode")

func requiresDeclaration(mode string) bool {
	return mode == ModeToolHosted || mode == ModeGitHubMediated
}

// checkPosture refuses a declaration-only mode the adapter does not declare.
func checkPosture(adapter Adapter, adm Admission) error {
	if !requiresDeclaration(adm.Authority.Mode) {
		return nil
	}
	d, ok := adapter.(PostureDeclarer)
	if !ok || !slices.Contains(d.Postures(), adm.Authority.Mode) {
		return fmt.Errorf("%w: %s for engine %s", ErrPostureUndeclared, adm.Authority.Mode, adm.Engine)
	}
	return nil
}

// MediatedPR is the evidence Hive gathers about a PR a github-mediated run
// produced before anything may merge it.
type MediatedPR struct {
	Number       int
	Author       string
	ChangedFiles []string
	// ChecksPassed is true when every required check concluded success on
	// the PR head.
	ChecksPassed bool
	// Approvers are the logins with an approving review on the PR head.
	Approvers []string
	// HiveIdentities are the logins Hive itself acts as; an approval by one
	// of them is not independent.
	HiveIdentities []string
}

// ErrMediatedPR wraps every VetMediatedPR refusal.
var ErrMediatedPR = errors.New("extwork: github-mediated PR not acceptable")

// VetMediatedPR applies the outside-contributor bar to a vendor-written PR:
// green required checks, no protected paths, and at least one approving
// review by someone who is neither the author nor a Hive identity. Passing
// grants nothing; it only means the PR may enter the ordinary merge gates.
func VetMediatedPR(pr MediatedPR, protected []string) error {
	var problems []string
	if pr.Number <= 0 {
		problems = append(problems, "no PR number")
	}
	if !pr.ChecksPassed {
		problems = append(problems, "required checks have not passed")
	}
	if protected == nil {
		protected = pushbroker.DefaultProtectedPaths
	}
	if bad := pushbroker.ProtectedPathViolations(pr.ChangedFiles, protected); len(bad) > 0 {
		problems = append(problems, "touches protected paths: "+strings.Join(bad, ", "))
	}
	independent := false
	for _, a := range pr.Approvers {
		if strings.EqualFold(a, pr.Author) || containsFold(pr.HiveIdentities, a) {
			continue
		}
		independent = true
		break
	}
	if !independent {
		problems = append(problems, "no independent approving review")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: PR #%d: %s", ErrMediatedPR, pr.Number, strings.Join(problems, "; "))
	}
	return nil
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
