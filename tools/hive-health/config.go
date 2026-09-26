package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Target is one Hive to probe.
type Target struct {
	// Name is the short, stable key for this Hive. It is the de-duplication
	// key for its health issue, so renaming it opens a fresh issue.
	Name string `json:"name"`
	// URL is the public base URL of the Hive dashboard (https only).
	URL string `json:"url"`
	// Namespace is the Kubernetes namespace the Hive runs in. It is only used
	// to write runbook commands for a human into the issue body; the probe
	// never talks to Kubernetes.
	Namespace string `json:"namespace,omitempty"`
	// SessionEnv names an environment variable that MAY hold a hive_session
	// cookie value for a read-role dashboard account. When the variable is
	// unset or empty the probe uses public endpoints only.
	SessionEnv string `json:"session_env,omitempty"`
}

// Config is the targets file.
type Config struct {
	HubSnapshotURL string     `json:"hub_snapshot_url,omitempty"`
	Hives          []Target   `json:"hives"`
	Thresholds     Thresholds `json:"thresholds"`
}

// Duration is a time.Duration that reads and writes as a Go duration string.
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Thresholds tunes classification. Every field has a default (see
// DefaultThresholds); a targets file only needs to name what it overrides.
type Thresholds struct {
	// A kick is stale when its age exceeds factor x cadence + KickSlack.
	KickWarnFactor float64  `json:"kick_warn_factor,omitempty"`
	KickFailFactor float64  `json:"kick_fail_factor,omitempty"`
	KickSlack      Duration `json:"kick_slack,omitempty"`
	// Floors: the governor only kicks an idle agent, and one turn can run for
	// half an hour, so a 1m cadence does not mean a kick every minute.
	KickWarnFloor Duration `json:"kick_warn_floor,omitempty"`
	KickFailFloor Duration `json:"kick_fail_floor,omitempty"`
	// Used when the agent's cadence is unknown (public tier: /api/health/deep
	// does not expose cadence).
	UnknownCadenceWarn Duration `json:"unknown_cadence_warn,omitempty"`
	UnknownCadenceFail Duration `json:"unknown_cadence_fail,omitempty"`
	// An agent not kicked since the Hive restarted, for at least this long,
	// while its peers were kicked, is presumed to be cadence-paused.
	PresumedPausedAge Duration `json:"presumed_paused_age,omitempty"`
	// The newest kick across all active agents: the governor is not kicking
	// anything when it is older than these.
	GovernorWarn Duration `json:"governor_warn,omitempty"`
	GovernorFail Duration `json:"governor_fail,omitempty"`
	// The newest output (bead, issue, PR, comment) the Hive produced.
	OutputWarn Duration `json:"output_warn,omitempty"`
	OutputFail Duration `json:"output_fail,omitempty"`
	// How long an agent may sit on a login screen / not-ready before it counts.
	LoginGrace   Duration `json:"login_grace,omitempty"`
	NotReadyWarn Duration `json:"not_ready_warn,omitempty"`
	// The hub snapshot is regenerated every 5 minutes.
	HubSnapshotStale Duration `json:"hub_snapshot_stale,omitempty"`
	// A backlog counter (hold, actionable) that grows by at least SpikeMin
	// AND by at least SpikeRatio (fraction) since the previous run is a spike.
	SpikeMin   int     `json:"spike_min,omitempty"`
	SpikeRatio float64 `json:"spike_ratio,omitempty"`
}

// DefaultThresholds are tuned against the live Tuna OS Hives (governor
// eval_interval 5m, cadences 1m..4h).
func DefaultThresholds() Thresholds {
	return Thresholds{
		KickWarnFactor:     3,
		KickFailFactor:     6,
		KickSlack:          Duration{15 * time.Minute},
		KickWarnFloor:      Duration{45 * time.Minute},
		KickFailFloor:      Duration{2 * time.Hour},
		UnknownCadenceWarn: Duration{12 * time.Hour},
		UnknownCadenceFail: Duration{36 * time.Hour},
		PresumedPausedAge:  Duration{24 * time.Hour},
		GovernorWarn:       Duration{30 * time.Minute},
		GovernorFail:       Duration{2 * time.Hour},
		OutputWarn:         Duration{24 * time.Hour},
		OutputFail:         Duration{72 * time.Hour},
		LoginGrace:         Duration{15 * time.Minute},
		NotReadyWarn:       Duration{30 * time.Minute},
		HubSnapshotStale:   Duration{30 * time.Minute},
		SpikeMin:           25,
		SpikeRatio:         0.25,
	}
}

func (t Thresholds) withDefaults() Thresholds {
	d := DefaultThresholds()
	if t.KickWarnFactor == 0 {
		t.KickWarnFactor = d.KickWarnFactor
	}
	if t.KickFailFactor == 0 {
		t.KickFailFactor = d.KickFailFactor
	}
	fill := func(v *Duration, def Duration) {
		if v.Duration == 0 {
			*v = def
		}
	}
	fill(&t.KickSlack, d.KickSlack)
	fill(&t.KickWarnFloor, d.KickWarnFloor)
	fill(&t.KickFailFloor, d.KickFailFloor)
	fill(&t.UnknownCadenceWarn, d.UnknownCadenceWarn)
	fill(&t.UnknownCadenceFail, d.UnknownCadenceFail)
	fill(&t.PresumedPausedAge, d.PresumedPausedAge)
	fill(&t.GovernorWarn, d.GovernorWarn)
	fill(&t.GovernorFail, d.GovernorFail)
	fill(&t.OutputWarn, d.OutputWarn)
	fill(&t.OutputFail, d.OutputFail)
	fill(&t.LoginGrace, d.LoginGrace)
	fill(&t.NotReadyWarn, d.NotReadyWarn)
	fill(&t.HubSnapshotStale, d.HubSnapshotStale)
	if t.SpikeMin == 0 {
		t.SpikeMin = d.SpikeMin
	}
	if t.SpikeRatio == 0 {
		t.SpikeRatio = d.SpikeRatio
	}
	return t
}

// LoadConfig reads and validates a targets file.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return ParseConfig(b)
}

// ParseConfig parses and validates targets JSON.
func ParseConfig(b []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parsing targets: %w", err)
	}
	if len(c.Hives) == 0 {
		return Config{}, fmt.Errorf("targets: no hives configured")
	}
	seen := map[string]bool{}
	for i, h := range c.Hives {
		if h.Name == "" {
			return Config{}, fmt.Errorf("targets: hive %d has no name", i)
		}
		if seen[h.Name] {
			return Config{}, fmt.Errorf("targets: duplicate hive name %q", h.Name)
		}
		seen[h.Name] = true
		u, err := url.Parse(h.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return Config{}, fmt.Errorf("targets: hive %q: url must be an https URL, got %q", h.Name, h.URL)
		}
		c.Hives[i].URL = strings.TrimRight(h.URL, "/")
	}
	if c.HubSnapshotURL != "" {
		u, err := url.Parse(c.HubSnapshotURL)
		if err != nil || u.Scheme != "https" {
			return Config{}, fmt.Errorf("targets: hub_snapshot_url must be https, got %q", c.HubSnapshotURL)
		}
	}
	c.Thresholds = c.Thresholds.withDefaults()
	return c, nil
}

// Host returns the host part of the target URL.
func (t Target) Host() string {
	u, err := url.Parse(t.URL)
	if err != nil {
		return t.URL
	}
	return u.Host
}
