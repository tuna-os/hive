package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

type recordingStarter struct {
	started []string
	fail    map[string]bool
}

func (s *recordingStarter) Start(_ context.Context, name string) error {
	s.started = append(s.started, name)
	if s.fail[name] {
		return errors.New("start failed")
	}
	return nil
}

type auditEntry struct {
	detail string
	agent  string
}

type recordingStartupAudit struct {
	entries []auditEntry
}

func (a *recordingStartupAudit) AuditLog(_, _, detail, agent string) {
	a.entries = append(a.entries, auditEntry{detail: detail, agent: agent})
}

func startupTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestStartPersistentAgentsFiltersAndAudits(t *testing.T) {
	manager := &recordingStarter{fail: map[string]bool{"failed": true}}
	audit := &recordingStartupAudit{}
	agents := map[string]config.AgentConfig{
		"ondemand": {OnDemand: true},
		"packed":   {},
		"paused":   {Paused: true},
		"failed":   {},
	}

	startPersistentAgents(context.Background(), agents, map[string]bool{"packed": true}, manager, audit, startupTestLogger(), 0)

	if len(manager.started) != 2 {
		t.Fatalf("started %d agents, want 2: %v", len(manager.started), manager.started)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("recorded %d successful starts, want 1: %v", len(audit.entries), audit.entries)
	}
	if audit.entries[0].agent != "paused" || audit.entries[0].detail != "trigger=startup; restored paused (persisted)" {
		t.Fatalf("paused audit = %+v", audit.entries[0])
	}
}

func TestStartPersistentAgentsHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manager := &recordingStarter{}

	startPersistentAgents(ctx, map[string]config.AgentConfig{"worker": {}}, nil, manager, &recordingStartupAudit{}, startupTestLogger(), time.Hour)

	if len(manager.started) != 0 {
		t.Fatalf("started agents after cancellation: %v", manager.started)
	}
}
