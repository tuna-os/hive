package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRevertSheriffConfigDefaultsAndOptIn(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("revert_sheriff:\n  repos: [Tuna-OS/TunaOS]\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	rs := cfg.RevertSheriff
	if !rs.Enabled() || !rs.Watches("tuna-os/tunaos") || rs.Watches("tuna-os/corral") {
		t.Fatalf("opt-in by repo broken: %+v", rs)
	}
	if rs.ModeOrDefault() != RevertSheriffShadow || !rs.HoldOrDefault() || rs.MaxPerDayOrDefault() != 2 ||
		rs.LookbackOrDefault() != 10 || rs.ConfirmTimeoutOrDefault() != 3*time.Hour {
		t.Fatalf("defaults wrong: mode=%s hold=%v max=%d lookback=%d timeout=%s",
			rs.ModeOrDefault(), rs.HoldOrDefault(), rs.MaxPerDayOrDefault(), rs.LookbackOrDefault(), rs.ConfirmTimeoutOrDefault())
	}

	var off *RevertSheriffConfig
	if off.Enabled() || off.Watches("tuna-os/tunaos") || off.Validate() != nil {
		t.Fatal("absent block must be off and valid")
	}

	noCap := &RevertSheriffConfig{MaxPerDay: -1}
	if noCap.MaxPerDayOrDefault() != 0 {
		t.Fatalf("max_per_day -1 must mean no cap, got %d", noCap.MaxPerDayOrDefault())
	}
}

func TestRevertSheriffConfigValidate(t *testing.T) {
	bad := map[string]*RevertSheriffConfig{
		"mode":     {Repos: []string{"o/r"}, Mode: "merge"},
		"repo":     {Repos: []string{"just-a-name"}},
		"lookback": {Repos: []string{"o/r"}, Lookback: 31},
		"timeout":  {Repos: []string{"o/r"}, ConfirmTimeout: "soon"},
	}
	for name, c := range bad {
		if c.Validate() == nil {
			t.Errorf("%s: invalid config accepted", name)
		}
	}
	good := &RevertSheriffConfig{Repos: []string{"o/r"}, Mode: "Propose", Lookback: 30, ConfirmTimeout: "90m"}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}
