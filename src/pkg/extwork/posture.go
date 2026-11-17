package extwork

import (
	"errors"
	"fmt"
	"slices"

	"github.com/hivecommons/hive/crux/agentmode"
)

// ModeToolHosted is the non-CLI admission mode (RFC #18464 §2, scoped by the
// maintainer decision on that RFC): the remote side holds no GitHub
// credential, and every tool call is executed by Hive under its own gates
// (ioscan, toolapprove, pushbroker). Any GitHub write is Hive's own, through
// the Hive App; phase 1 wires no write tools at all. The ceiling is
// ISSUES_AND_PRS, permanently: merge never comes from this path, and no
// configuration raises it.
//
// There is deliberately no mode in which a vendor agent writes to GitHub
// under its own identity. Every GitHub write stays with the Hive App; a
// vendor-written PR is not an admission posture Hive offers.
const ModeToolHosted = "tool-hosted"

// ToolHostedCeiling is the permanent cap on what a tool-hosted admission may
// be granted.
const ToolHostedCeiling = agentmode.ModeIssuesAndPRs

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
}

// postures is unexported and PostureFor returns a copy, so no caller can
// raise a ceiling at run time.
var postures = map[string]Posture{
	ModeOff:        {Ceiling: agentmode.ModeAdvisory},
	ModeShadow:     {Ceiling: agentmode.ModeAdvisory},
	ModeReportOnly: {StartsWork: true, Ceiling: agentmode.ModeAdvisory},
	ModeToolHosted: {StartsWork: true, HiveExecutesWrites: true, Ceiling: ToolHostedCeiling},
}

// PostureFor returns the posture of mode; ok is false for an unknown mode.
func PostureFor(mode string) (Posture, bool) {
	p, ok := postures[mode]
	return p, ok
}

// ClampMode caps a requested agent mode at the admission mode's ceiling. It
// never returns a mode above requested, above the posture's ceiling, or above
// ToolHostedCeiling. An unknown admission mode clamps to ADVISORY, and so
// does an out-of-range request (below ADVISORY is not a mode).
func ClampMode(mode string, requested agentmode.AgentMode) agentmode.AgentMode {
	p, ok := PostureFor(mode)
	if !ok || requested < agentmode.ModeAdvisory {
		return agentmode.ModeAdvisory
	}
	return min(requested, p.Ceiling, ToolHostedCeiling)
}

// PostureDeclarer is implemented by adapters that support modes beyond
// off/shadow/report-only. A mode only raises the ceiling an adapter may
// declare; it never forces one, so the binding refuses tool-hosted for any
// adapter that does not list it.
type PostureDeclarer interface {
	Postures() []string
}

// ErrPostureUndeclared is returned when an admission asks for a mode the
// adapter has not declared.
var ErrPostureUndeclared = errors.New("extwork: adapter does not declare this mode")

func requiresDeclaration(mode string) bool {
	return mode == ModeToolHosted
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
