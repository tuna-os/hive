package main

import (
	"encoding/json"
	"time"
)

// The shapes below mirror what the Hive serves. Only the fields the probe
// reads are declared; everything is optional because the probe must degrade,
// not crash, when an upstream sync renames or drops a field.
//
// Sources (src/pkg/dashboard):
//   GET /api/health           server.go handleHealth       public
//   GET /api/health/deep      server.go handleHealthDeep   public
//   GET /api/contribute/status   api_contribute.go         public
//   GET /api/contribute/activity api_contribute.go         public
//   GET /api/status           server.go handleStatus       session required
// plus the hub snapshot https://hub.tunaos.org/activity.json, which the
// cluster's hive-activity CronJob regenerates every 5 minutes.

// DeepHealth is GET /api/health/deep.
type DeepHealth struct {
	Status string                     `json:"status"`
	Fails  int                        `json:"fails"`
	Checks map[string]json.RawMessage `json:"checks"`
}

// DeepCheck is the common shape of a /api/health/deep check.
type DeepCheck struct {
	Status string   `json:"status"`
	Detail string   `json:"detail,omitempty"`
	Agents []string `json:"agents,omitempty"`
}

// DeepAgent is one entry of checks.agents.
type DeepAgent struct {
	State         string `json:"state"`
	Status        string `json:"status"`
	Paused        bool   `json:"paused,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
	Detail        string `json:"detail,omitempty"`
	LastKick      string `json:"last_kick,omitempty"`
	LastKickAge   string `json:"last_kick_age,omitempty"`
	LastPromptLen int    `json:"last_prompt_len,omitempty"`
}

// DeepGovernor is checks.governor.
type DeepGovernor struct {
	Status string `json:"status"`
	Mode   string `json:"mode"`
	Issues int    `json:"issues"`
	PRs    int    `json:"prs"`
	Hold   int    `json:"hold"`
}

// DeepConfig is checks.config.
type DeepConfig struct {
	Status    string `json:"status"`
	Org       string `json:"org"`
	Repos     int    `json:"repos"`
	HiveID    string `json:"hive_id"`
	ACMMLevel *int   `json:"acmm_level,omitempty"`
}

// DeepQueue is checks.queue.
type DeepQueue struct {
	Status     string `json:"status"`
	Actionable int    `json:"actionable"`
}

// DeepHeartbeat is checks.hub_heartbeat.
type DeepHeartbeat struct {
	Status         string `json:"status"`
	Detail         string `json:"detail,omitempty"`
	LastSuccess    string `json:"last_success,omitempty"`
	LastSuccessAge string `json:"last_success_age,omitempty"`
}

func (d *DeepHealth) check(name string, into any) bool {
	if d == nil || d.Checks == nil {
		return false
	}
	raw, ok := d.Checks[name]
	if !ok {
		return false
	}
	return json.Unmarshal(raw, into) == nil
}

// Agents returns checks.agents, or nil.
func (d *DeepHealth) Agents() map[string]DeepAgent {
	var m map[string]DeepAgent
	if !d.check("agents", &m) {
		return nil
	}
	return m
}

// ContributeStatus is GET /api/contribute/status.
type ContributeStatus struct {
	Hub                string `json:"hub"`
	ActiveContributors int    `json:"active_contributors"`
	ActionableItems    int    `json:"actionable_items"`
	ServedSHA          string `json:"served_sha"`
	APIVersion         string `json:"api_version"`
}

// ContributeActivity is GET /api/contribute/activity.
type ContributeActivity struct {
	Activity []struct {
		Timestamp time.Time `json:"timestamp"`
		Username  string    `json:"username"`
		Action    string    `json:"action"`
		CLI       string    `json:"cli,omitempty"`
		Task      string    `json:"task,omitempty"`
	} `json:"activity"`
}

// StatusDoc is the subset of GET /api/status the probe reads.
type StatusDoc struct {
	HiveID   string `json:"hiveId"`
	Governor struct {
		Active bool   `json:"active"`
		Mode   string `json:"mode"`
		Issues int    `json:"issues"`
		PRs    int    `json:"prs"`
	} `json:"governor"`
	Hold struct {
		Issues int `json:"issues"`
		PRs    int `json:"prs"`
		Total  int `json:"total"`
	} `json:"hold"`
	GHRateLimits struct {
		Core struct {
			Limit     int    `json:"limit"`
			Remaining int    `json:"remaining"`
			Reset     string `json:"reset"`
		} `json:"core"`
	} `json:"ghRateLimits"`
	SystemAlerts  []SystemAlert `json:"systemAlerts"`
	CadenceMatrix []CadenceRow  `json:"cadenceMatrix"`
	Agents        []StatusAgent `json:"agents"`
}

// SystemAlert is a dashboard system alert.
type SystemAlert struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// CadenceRow is one agent's cadence per governor mode.
type CadenceRow struct {
	Agent string `json:"agent"`
	Idle  string `json:"idle"`
	Quiet string `json:"quiet"`
	Busy  string `json:"busy"`
	Surge string `json:"surge"`
}

// Modes returns the four per-mode cadences.
func (c CadenceRow) Modes() map[string]string {
	return map[string]string{"idle": c.Idle, "quiet": c.Quiet, "busy": c.Busy, "surge": c.Surge}
}

// Condition is a watchdog condition (Ready / Authenticated / Producing).
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// StatusAgent is one entry of /api/status agents. liveSummary (the raw pane
// tail) is deliberately NOT read: it is terminal output and can contain
// anything the agent printed, and every report this probe writes is public.
type StatusAgent struct {
	Name             string      `json:"name"`
	State            string      `json:"state"`
	Paused           bool        `json:"paused"`
	OffByCadence     bool        `json:"offByCadence"`
	NoCadence        bool        `json:"noCadence"`
	NeedsLogin       bool        `json:"needsLogin"`
	Cadence          string      `json:"cadence"`
	CLI              string      `json:"cli,omitempty"`
	Restarts         int         `json:"restarts"`
	StructuredStatus string      `json:"structuredStatus,omitempty"`
	StatusEvidence   string      `json:"statusEvidence,omitempty"`
	LastError        string      `json:"lastError,omitempty"`
	Conditions       []Condition `json:"conditions,omitempty"`
	WatchdogMode     string      `json:"watchdogMode,omitempty"`
}

// HubSnapshot is https://hub.tunaos.org/activity.json.
type HubSnapshot struct {
	GeneratedAt time.Time  `json:"generated_at"`
	Spokes      []HubSpoke `json:"spokes"`
	Beads       []HubBead  `json:"beads"`
	Registry    *struct {
		Hives []RegistryHive `json:"hives"`
	} `json:"registry"`
}

// HubSpoke is one Hive in the hub snapshot.
type HubSpoke struct {
	Spoke        string `json:"spoke"`
	HiveID       string `json:"hiveId"`
	GovernorMode string `json:"governorMode"`
	Agents       []struct {
		Name   string `json:"name"`
		Paused bool   `json:"paused"`
		State  string `json:"state"`
		Busy   string `json:"busy"`
	} `json:"agents"`
}

// HubBead is a recent bead (agent finding / work log entry).
type HubBead struct {
	Spoke   string    `json:"spoke"`
	Agent   string    `json:"agent"`
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Type    string    `json:"type"`
	Status  string    `json:"status"`
	Updated time.Time `json:"updated"`
}

// RegistryHive is the hub registry's per-hive record.
type RegistryHive struct {
	ID            string `json:"id"`
	Online        bool   `json:"online"`
	LastHeartbeat string `json:"lastHeartbeat"`
	RepoActivity  []struct {
		Repo   string `json:"repo"`
		Agents []struct {
			Agent    string       `json:"agent"`
			Issues   *ActivityCnt `json:"issues"`
			PRs      *ActivityCnt `json:"prs"`
			Comments *ActivityCnt `json:"comments"`
			Merges   *ActivityCnt `json:"merges"`
			Reviews  *ActivityCnt `json:"reviews"`
		} `json:"agents"`
	} `json:"repoActivity"`
}

// ActivityCnt is a count with the newest timestamp.
type ActivityCnt struct {
	Count    int        `json:"count"`
	NewestAt *time.Time `json:"newestAt,omitempty"`
}
