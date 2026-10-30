package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

// revertSheriffInterval spaces revert sheriff passes. It matches the
// red-main sweep, whose tracker the sheriff reads.
const revertSheriffInterval = 10 * time.Minute

// runRevertSheriffSweepIfDue runs the revert sheriff for the repos
// revert_sheriff opts in (tuna-os pilot, tuna-os/tunaOS#4072). It must run
// after runRedMainSweepIfDue in the same tick.
func runRevertSheriffSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil || cfg == nil || !cfg.RevertSheriff.Enabled() {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < revertSheriffInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	rs := cfg.RevertSheriff
	mode := rs.ModeOrDefault()
	result, err := ghClient.SweepRevertSheriff(ctx, github.RevertSheriffOptions{
		Watches:        rs.Watches,
		Mode:           mode,
		Hold:           rs.HoldOrDefault(),
		MaxPerDay:      rs.MaxPerDayOrDefault(),
		Lookback:       rs.LookbackOrDefault(),
		ConfirmTimeout: rs.ConfirmTimeoutOrDefault(),
		Now:            now,
		Audit: func(ev github.RevertSheriffEvent) {
			logger.Info("revert sheriff", "mode", mode, "repo", ev.Repo, "check", ev.Check, "action", ev.Action,
				"pr", ev.PR, "culprit", ev.Culprit, "revert_pr", ev.RevertPR, "reason", ev.Reason)
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("mode=%s, check=%s, action=%s, pr=%d, culprit=%s, revert_pr=%d, reason=%s",
				mode, ev.Check, ev.Action, ev.PR, ev.Culprit, ev.RevertPR, ev.Reason)
			dashSrv.AuditLogRecord("system", "revert-sheriff", detail, "", ev.Repo, ev.PR)
		},
	})
	if err != nil {
		logger.Warn("revert sheriff sweep failed", "error", err)
		return
	}
	if result.Red > 0 {
		logger.Info("revert sheriff sweep complete", "repos", result.Repos, "red", result.Red, "events", result.Events)
	}
}
