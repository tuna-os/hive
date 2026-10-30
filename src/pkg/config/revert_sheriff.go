package config

import (
	"fmt"
	"strings"
	"time"
)

// Revert sheriff modes.
const (
	// RevertSheriffShadow finds and confirms culprits and logs the revert
	// it would open; it writes nothing to the forge except the confirming
	// re-run.
	RevertSheriffShadow = "shadow"
	// RevertSheriffPropose opens the revert PR.
	RevertSheriffPropose = "propose"
)

// RevertSheriffConfig opts repos into the revert sheriff (tuna-os pilot,
// tuna-os/tunaOS#4072). When a merge turns a required check red on a repo's
// default branch, the sheriff finds the merge, re-runs the failing job once to
// rule out a flake, and, in propose mode, opens a revert PR. Nil or no repos
// means off.
type RevertSheriffConfig struct {
	// Repos lists the owner/repo names the sheriff watches.
	Repos []string `yaml:"repos,omitempty" json:"repos,omitempty"`
	// Mode is "shadow" (default) or "propose".
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Hold labels each revert PR `hold`, so a person releases it. Default
	// true.
	Hold *bool `yaml:"hold,omitempty" json:"hold,omitempty"`
	// MaxPerDay caps the reverts proposed per repo in 24 hours. Default 2;
	// a negative value means no cap.
	MaxPerDay int `yaml:"max_per_day,omitempty" json:"max_per_day,omitempty"`
	// Lookback is how many default-branch commits the sheriff reads to find
	// the last green one. Default 10, at most 30.
	Lookback int `yaml:"lookback,omitempty" json:"lookback,omitempty"`
	// ConfirmTimeout is how long the sheriff waits for the confirming re-run
	// (a Go duration). Default 3h.
	ConfirmTimeout string `yaml:"confirm_timeout,omitempty" json:"confirm_timeout,omitempty"`
}

const (
	defaultRevertSheriffMaxPerDay      = 2
	defaultRevertSheriffLookback       = 10
	maxRevertSheriffLookback           = 30
	defaultRevertSheriffConfirmTimeout = 3 * time.Hour
)

// Watches reports whether the sheriff watches repo.
func (c *RevertSheriffConfig) Watches(repo string) bool {
	if c == nil {
		return false
	}
	repo = strings.TrimSpace(repo)
	for _, r := range c.Repos {
		if strings.EqualFold(strings.TrimSpace(r), repo) {
			return true
		}
	}
	return false
}

// Enabled reports whether any repo is watched.
func (c *RevertSheriffConfig) Enabled() bool {
	return c != nil && len(c.Repos) > 0
}

// ModeOrDefault returns the mode, "shadow" when unset.
func (c *RevertSheriffConfig) ModeOrDefault() string {
	if c == nil || strings.TrimSpace(c.Mode) == "" {
		return RevertSheriffShadow
	}
	return strings.ToLower(strings.TrimSpace(c.Mode))
}

// HoldOrDefault reports whether revert PRs get the hold label.
func (c *RevertSheriffConfig) HoldOrDefault() bool {
	if c == nil || c.Hold == nil {
		return true
	}
	return *c.Hold
}

// MaxPerDayOrDefault returns the daily cap; 0 means no cap.
func (c *RevertSheriffConfig) MaxPerDayOrDefault() int {
	switch {
	case c == nil || c.MaxPerDay == 0:
		return defaultRevertSheriffMaxPerDay
	case c.MaxPerDay < 0:
		return 0
	}
	return c.MaxPerDay
}

// LookbackOrDefault returns how many commits to read.
func (c *RevertSheriffConfig) LookbackOrDefault() int {
	if c == nil || c.Lookback <= 0 {
		return defaultRevertSheriffLookback
	}
	if c.Lookback > maxRevertSheriffLookback {
		return maxRevertSheriffLookback
	}
	return c.Lookback
}

// ConfirmTimeoutOrDefault returns how long to wait for the re-run.
func (c *RevertSheriffConfig) ConfirmTimeoutOrDefault() time.Duration {
	if c == nil || strings.TrimSpace(c.ConfirmTimeout) == "" {
		return defaultRevertSheriffConfirmTimeout
	}
	d, err := time.ParseDuration(strings.TrimSpace(c.ConfirmTimeout))
	if err != nil || d <= 0 {
		return defaultRevertSheriffConfirmTimeout
	}
	return d
}

// Validate rejects a mode, repo or timeout the sheriff cannot use.
func (c *RevertSheriffConfig) Validate() error {
	if c == nil {
		return nil
	}
	switch c.ModeOrDefault() {
	case RevertSheriffShadow, RevertSheriffPropose:
	default:
		return fmt.Errorf("revert_sheriff.mode: %q is not %q or %q", c.Mode, RevertSheriffShadow, RevertSheriffPropose)
	}
	for _, r := range c.Repos {
		owner, name, ok := strings.Cut(strings.TrimSpace(r), "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return fmt.Errorf("revert_sheriff.repos: %q is not owner/repo", r)
		}
	}
	if c.Lookback < 0 || c.Lookback > maxRevertSheriffLookback {
		return fmt.Errorf("revert_sheriff.lookback: %d is not between 0 and %d", c.Lookback, maxRevertSheriffLookback)
	}
	if s := strings.TrimSpace(c.ConfirmTimeout); s != "" {
		if d, err := time.ParseDuration(s); err != nil || d <= 0 {
			return fmt.Errorf("revert_sheriff.confirm_timeout: %q is not a positive duration", c.ConfirmTimeout)
		}
	}
	return nil
}
