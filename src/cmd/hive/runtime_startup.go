package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

type startupAgentManager interface {
	Start(context.Context, string) error
}

type startupAuditLog interface {
	AuditLog(user, action, detail, agent string)
}

// startPersistentAgents owns the agent-launch phase of daemon startup. It is
// synchronous so the composition root decides whether to run it in the
// background, while tests can exercise cancellation without starting a daemon.
func startPersistentAgents(
	ctx context.Context,
	agents map[string]config.AgentConfig,
	onDemandFromPack map[string]bool,
	manager startupAgentManager,
	audit startupAuditLog,
	logger *slog.Logger,
	stagger time.Duration,
) {
	started := 0
	for name, agentConfig := range agents {
		if agentConfig.OnDemand || onDemandFromPack[name] {
			logger.Info("skipping on-demand agent at startup", "name", name)
			continue
		}
		if started > 0 {
			logger.Info("staggering agent launch", "name", name, "delay_sec", int(stagger/time.Second))
			select {
			case <-time.After(stagger):
			case <-ctx.Done():
				logger.Info("aborting staggered agent launch: shutting down")
				return
			}
		}
		// A cancellation before the first launch must not start a fresh process.
		if ctx.Err() != nil {
			logger.Info("aborting staggered agent launch: shutting down")
			return
		}
		logger.Info("audit: starting agent", "name", name, "trigger", "startup")
		if err := manager.Start(ctx, name); err != nil {
			logger.Warn("failed to start agent", "name", name, "error", err)
		} else {
			detail := "trigger=startup"
			if agentConfig.Paused {
				detail = "trigger=startup; restored paused (persisted)"
			}
			audit.AuditLog("system", "agent_start", detail, name)
		}
		started++
	}
}
