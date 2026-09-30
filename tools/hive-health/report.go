package main

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Level is a check outcome. Only pass/warn/fail affect the verdict; skip marks
// something deliberately not evaluated (a paused agent), info is context.
type Level string

const (
	Pass Level = "pass"
	Warn Level = "warn"
	Fail Level = "fail"
	Skip Level = "skip"
	Info Level = "info"
)

func (l Level) rank() int {
	switch l {
	case Fail:
		return 3
	case Warn:
		return 2
	case Pass:
		return 1
	default:
		return 0
	}
}

// worst returns the most severe of the verdict-bearing levels (pass when
// nothing bearing a verdict was seen).
func worst(levels ...Level) Level {
	w := Pass
	for _, l := range levels {
		if l.rank() > w.rank() {
			w = l
		}
	}
	return w
}

// Check is one evaluated condition.
type Check struct {
	ID       string   `json:"id"`
	Level    Level    `json:"level"`
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence,omitempty"`
	// Kind groups checks for remediation advice (see remediation.go).
	Kind string `json:"kind"`
}

// AgentReport is the per-agent roll-up.
type AgentReport struct {
	Name  string `json:"name"`
	State string `json:"state,omitempty"`
	CLI   string `json:"cli,omitempty"`
	Level Level  `json:"level"`
	// Why the level was assigned, one line.
	Reason string `json:"reason"`
	// Cadence is the agent's cadence in the current governor mode, when known
	// (authenticated tier only). "paused" means the governor never kicks it.
	Cadence string `json:"cadence,omitempty"`
	// PausedBy is set when the agent is deliberately off: "pause" (operator
	// pause), "config" (disabled), "cadence" (cadence paused in the current
	// mode, from /api/status) or "cadence-presumed" (public tier inference).
	PausedBy       string     `json:"paused_by,omitempty"`
	LastKick       *time.Time `json:"last_kick,omitempty"`
	LastKickAge    string     `json:"last_kick_age,omitempty"`
	LastOutput     *time.Time `json:"last_output,omitempty"`
	LastOutputAge  string     `json:"last_output_age,omitempty"`
	NeedsLogin     bool       `json:"needs_login,omitempty"`
	StatusEvidence string     `json:"status_evidence,omitempty"`
}

// OutputItem is a piece of recent agent output readable via the API.
type OutputItem struct {
	At     time.Time `json:"at"`
	Agent  string    `json:"agent,omitempty"`
	Source string    `json:"source"`
	Text   string    `json:"text"`
}

// QueueSnapshot is the backlog as the governor sees it.
type QueueSnapshot struct {
	Mode       string `json:"mode,omitempty"`
	Issues     int    `json:"issues"`
	PRs        int    `json:"prs"`
	Hold       int    `json:"hold"`
	HoldPRs    *int   `json:"hold_prs,omitempty"`
	Actionable int    `json:"actionable"`
}

// HiveReport is the verdict for one Hive.
type HiveReport struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Namespace string `json:"namespace,omitempty"`
	HiveID    string `json:"hive_id,omitempty"`
	Level     Level  `json:"level"`
	// AuthTier: "public" (no credential), "session" (read-role session
	// accepted), "session-rejected" (credential configured but refused),
	// "status-file" (operator supplied /api/status offline).
	AuthTier     string         `json:"auth_tier"`
	ServedSHA    string         `json:"served_sha,omitempty"`
	Checks       []Check        `json:"checks"`
	Agents       []AgentReport  `json:"agents"`
	Queue        *QueueSnapshot `json:"queue,omitempty"`
	RecentOutput []OutputItem   `json:"recent_output,omitempty"`
	// Fingerprint identifies the SET of failing checks, so the issue sync can
	// tell "still failing the same way" from "failing differently".
	Fingerprint string `json:"fingerprint"`
}

// Report is the whole probe run.
type Report struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Level       Level        `json:"level"`
	Hub         []Check      `json:"hub,omitempty"`
	Hives       []HiveReport `json:"hives"`
}

func (h *HiveReport) add(c Check) { h.Checks = append(h.Checks, c) }

// finalize computes the Hive level and fingerprint.
func (h *HiveReport) finalize() {
	lv := Pass
	var failing []string
	for _, c := range h.Checks {
		lv = worst(lv, c.Level)
		if c.Level == Fail {
			failing = append(failing, c.ID)
		}
	}
	h.Level = lv
	sort.Strings(failing)
	sum := sha256.Sum256([]byte(strings.Join(failing, "\n")))
	if len(failing) == 0 {
		h.Fingerprint = ""
	} else {
		h.Fingerprint = hex.EncodeToString(sum[:])[:12]
	}
}

// ChecksAt returns the checks at the given level.
func (h HiveReport) ChecksAt(l Level) []Check {
	var out []Check
	for _, c := range h.Checks {
		if c.Level == l {
			out = append(out, c)
		}
	}
	return out
}

// Find returns the check with the given id.
func (h HiveReport) Find(id string) (Check, bool) {
	for _, c := range h.Checks {
		if c.ID == id {
			return c, true
		}
	}
	return Check{}, false
}

// humanAge formats a duration compactly: 45s, 12m, 3h20m, 4d6h.
func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		h, m := int(d/time.Hour), int((d%time.Hour)/time.Minute)
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h" + strconv.Itoa(m) + "m"
	default:
		days, h := int(d/(24*time.Hour)), int((d%(24*time.Hour))/time.Hour)
		if h == 0 {
			return strconv.Itoa(days) + "d"
		}
		return strconv.Itoa(days) + "d" + strconv.Itoa(h) + "h"
	}
}
