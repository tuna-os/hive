package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// seedInvestigation records one dispatch -> agent result -> merge story:
// operator kick, scheduler pause, agent PR report, governor merge, plus noise.
func seedInvestigation(a *AuditLog) {
	a.Log("alice", "kick", "", "scanner")
	a.Log("watchdog", "watchdog-crashloop-pause", "reason=crashloop", "scanner")
	a.Log("system", "agent_issue_claimed", "repo=tuna-os/hive, number=161, backend=claude", "guide")
	a.Log("system", "agent_pr_created", "repo=tuna-os/hive, number=300, backend=claude", "guide")
	a.Log("system", "pr_merged", "repo=tuna-os/hive, number=300", "governor")
	a.Log("system", "agent_pr_created", "repo=tuna-os/other, number=300", "guide")
}

func TestAuditQueryFiltersAndKinds(t *testing.T) {
	a := &AuditLog{}
	seedInvestigation(a)

	res := a.Query(AuditQuery{Repo: "tuna-os/hive", Number: "300"})
	if res.Matched != 2 || len(res.Entries) != 2 || res.Truncated {
		t.Fatalf("repo+number: %+v", res)
	}
	if res.Entries[0].Action != "pr_merged" || res.Entries[0].Kind != AuditKindScheduler {
		t.Fatalf("newest first / scheduler kind: %+v", res.Entries[0])
	}
	if res.Entries[1].Kind != AuditKindAgentResult {
		t.Fatalf("agent result kind: %+v", res.Entries[1])
	}
	if got := a.Query(AuditQuery{Kind: AuditKindOperator}).Entries; len(got) != 1 || got[0].User != "alice" {
		t.Fatalf("operator filter: %+v", got)
	}
	if got := a.Query(AuditQuery{Agent: "scanner", Actor: "watchdog"}).Entries; len(got) != 1 || got[0].Kind != AuditKindScheduler {
		t.Fatalf("agent+actor filter: %+v", got)
	}
	got := a.Query(AuditQuery{Actions: map[string]bool{"kick": true, "pr_merged": true}})
	if got.Matched != 2 {
		t.Fatalf("action filter: %+v", got)
	}
}

func TestAuditQueryTruncationAndWindow(t *testing.T) {
	a := &AuditLog{}
	for i := 0; i < 5; i++ {
		a.Log("alice", "kick", "", "scanner")
	}
	res := a.Query(AuditQuery{Limit: 2})
	if len(res.Entries) != 2 || res.Matched != 5 || !res.Truncated {
		t.Fatalf("limit truncation not explicit: %+v", res)
	}
	if !res.Window.Complete || res.Window.RingSize != 5 || res.Window.RingCap != auditRingCap {
		t.Fatalf("window: %+v", res.Window)
	}
	for i := 0; i < auditRingCap; i++ {
		a.Log("alice", "kick", "", "scanner")
	}
	if a.Query(AuditQuery{}).Window.Complete {
		t.Fatal("full ring must report an incomplete window")
	}
	if n := len(a.Query(AuditQuery{Limit: 100000}).Entries); n != auditMaxEntries {
		t.Fatalf("limit not capped: %d", n)
	}
}

func TestHandleAuditLogQueryParams(t *testing.T) {
	server := &Server{audit: &AuditLog{}}
	seedInvestigation(server.audit)
	req := httptest.NewRequest(http.MethodGet,
		"/api/audit?repo=tuna-os/hive&number=300&action=agent_pr_created,pr_merged&since=2000-01-01T00:00:00Z&limit=1", nil)
	req.Header.Set("X-Hive-Role", config.RoleReadWrite)
	rec := httptest.NewRecorder()
	server.handleAuditLog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Entries   []AuditEntry `json:"entries"`
		Count     int          `json:"count"`
		Matched   int          `json:"matched"`
		Truncated bool         `json:"truncated"`
		Window    AuditWindow  `json:"window"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 1 || body.Matched != 2 || !body.Truncated || body.Entries[0].Kind == "" {
		t.Fatalf("body: %+v", body)
	}
	if strings.Contains(rec.Body.String(), "token") {
		t.Fatal("unexpected token material in payload")
	}
}

func TestAuditDetailValue(t *testing.T) {
	d := "repo=a/b, number=3, url=https://x/y"
	if auditDetailValue(d, "number") != "3" || auditDetailValue(d, "repo") != "a/b" || auditDetailValue(d, "nope") != "" {
		t.Fatal("auditDetailValue")
	}
}

func TestAuditEntryKindGovernorAndLoginError(t *testing.T) {
	cases := []struct {
		name string
		e    AuditEntry
		want string
	}{
		{"governor approval", AuditEntry{User: "system", Action: "agent_pr_reviewed", Agent: "governor"}, AuditKindScheduler},
		{"dashboard creation", AuditEntry{User: "system", Action: "agent_issue_created", Agent: "dashboard"}, AuditKindOperator},
		{"agent review", AuditEntry{User: "system", Action: "agent_pr_reviewed", Agent: "reviewer"}, AuditKindAgentResult},
		{"failed login", AuditEntry{User: "unknown", Action: "login_error"}, AuditKindOperator},
	}
	for _, c := range cases {
		if got := auditEntryKind(c.e); got != c.want {
			t.Errorf("%s: kind=%q want %q", c.name, got, c.want)
		}
	}
}
