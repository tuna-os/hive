package dashboard

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Audit entry kinds, derived at serve time. They separate the three things an
// investigator must not conflate when following one piece of work from
// dispatch to outcome.
const (
	// AuditKindOperator: a person acted (dashboard request, login, config).
	AuditKindOperator = "operator"
	// AuditKindScheduler: hive itself decided (watchdog, trajectory review,
	// governor merges, lifecycle events, startup checks).
	AuditKindScheduler = "scheduler"
	// AuditKindAgentResult: an agent reported a result through the mediated
	// GitHub request path (PR/issue/comment created, issue claimed, PR reviewed).
	AuditKindAgentResult = "agent_result"
)

// auditAgentResultActions are the actions written when an agent's request was
// carried out on its behalf. They are recorded under actor "system" (the
// writer is hive), so the action name is what identifies the agent's report.
var auditAgentResultActions = map[string]bool{
	"agent_pr_created":          true,
	"agent_issue_created":       true,
	"agent_comment_created":     true,
	"agent_issue_claimed":       true,
	"agent_pr_reviewed":         true,
	"pr_attribution_reconciled": true,
}

// auditSchedulerActors are non-person actors that make hive-originated
// decisions.
var auditSchedulerActors = map[string]bool{
	"system":     true,
	"watchdog":   true,
	"trajectory": true,
}

// Agent names the hive records for actions no coding agent performed (mirrors
// ghpkg.AttributionAgentGovernor / AttributionAgentDashboard).
const (
	auditAgentGovernor  = "governor"
	auditAgentDashboard = "dashboard"
)

// auditEntryKind classifies an entry. Unclassified users are operators; this
// includes actor "unknown" (a failed login is a person's attempt, not a hive
// decision).
func auditEntryKind(e AuditEntry) string {
	if auditAgentResultActions[e.Action] {
		// The governor's own auto-merge approval and operator-triggered
		// dashboard creations reuse these actions but are not an agent's report.
		switch e.Agent {
		case auditAgentGovernor:
			return AuditKindScheduler
		case auditAgentDashboard:
			return AuditKindOperator
		}
		return AuditKindAgentResult
	}
	if auditSchedulerActors[e.User] {
		return AuditKindScheduler
	}
	return AuditKindOperator
}

// auditDetailValue extracts key's value from a "k=v, k=v" detail string (the
// shape written by auditDetail / agent.FormatAuditDetail). Values containing
// ", " are cut at the separator, which is acceptable for the identifier keys
// (repo, number, epic, key) this is used for.
func auditDetailValue(detail, key string) string {
	for _, part := range strings.Split(detail, ", ") {
		if k, v, ok := strings.Cut(part, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// AuditQuery narrows the audit window. Zero values mean "no constraint".
type AuditQuery struct {
	Agent  string
	Repo   string // matches detail repo=
	Number string // matches detail number= (issue or PR number)
	Actor  string
	Kind   string
	Since  time.Time
	// Actions, when non-empty, restricts to these exact action names.
	Actions map[string]bool
	Limit   int
}

// AuditWindow states how much history the result could have seen, so a caller
// never mistakes a bounded in-memory view for the complete record.
type AuditWindow struct {
	RingSize int    `json:"ring_size"`
	RingCap  int    `json:"ring_cap"`
	Oldest   string `json:"oldest,omitempty"`
	// Complete is false once the ring is full: older events may exist only in
	// the on-disk log, which this view does not read.
	Complete bool `json:"complete"`
}

// AuditResult is a filtered, bounded view of the audit ring.
type AuditResult struct {
	Entries []AuditEntry
	// Matched counts every ring entry matching the filters; Truncated is true
	// when Matched exceeds len(Entries) because of the limit.
	Matched   int
	Truncated bool
	Window    AuditWindow
}

// Query returns up to q.Limit (default and max auditMaxEntries) newest-first
// matching entries from the in-memory ring.
func (a *AuditLog) Query(q AuditQuery) AuditResult {
	limit := q.Limit
	if limit <= 0 || limit > auditMaxEntries {
		limit = auditMaxEntries
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	res := AuditResult{Entries: []AuditEntry{}}
	res.Window = AuditWindow{RingSize: len(a.ring), RingCap: auditRingCap, Complete: len(a.ring) < auditRingCap}
	if len(a.ring) > 0 {
		res.Window.Oldest = a.ring[0].Timestamp
	}
	for i := len(a.ring) - 1; i >= 0; i-- {
		e := a.ring[i]
		if !q.matches(e) {
			continue
		}
		res.Matched++
		if len(res.Entries) < limit {
			e.Kind = auditEntryKind(e)
			res.Entries = append(res.Entries, e)
		}
	}
	res.Truncated = res.Matched > len(res.Entries)
	return res
}

func (q AuditQuery) matches(e AuditEntry) bool {
	if q.Agent != "" && e.Agent != q.Agent {
		return false
	}
	if q.Actor != "" && e.User != q.Actor {
		return false
	}
	if len(q.Actions) > 0 && !q.Actions[e.Action] {
		return false
	}
	if q.Kind != "" && auditEntryKind(e) != q.Kind {
		return false
	}
	if q.Repo != "" && auditDetailValue(e.Detail, "repo") != q.Repo {
		return false
	}
	if q.Number != "" && auditDetailValue(e.Detail, "number") != q.Number {
		return false
	}
	if !q.Since.IsZero() {
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil || t.Before(q.Since) {
			return false
		}
	}
	return true
}

// parseAuditQuery reads the optional filters from GET /api/audit. Unknown or
// malformed values are ignored (the unfiltered default is the old behaviour).
func parseAuditQuery(r *http.Request) AuditQuery {
	v := r.URL.Query()
	q := AuditQuery{
		Agent:  v.Get("agent"),
		Repo:   v.Get("repo"),
		Number: v.Get("number"),
		Actor:  v.Get("actor"),
		Kind:   v.Get("kind"),
	}
	if n, err := strconv.Atoi(v.Get("limit")); err == nil && n > 0 {
		q.Limit = n
	}
	if t, err := time.Parse(time.RFC3339, v.Get("since")); err == nil {
		q.Since = t
	}
	if raw := v.Get("action"); raw != "" {
		q.Actions = map[string]bool{}
		for _, a := range strings.Split(raw, ",") {
			if a = strings.TrimSpace(a); a != "" {
				q.Actions[a] = true
			}
		}
	}
	return q
}
