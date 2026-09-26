# hive-health

A read-only health probe for the Tuna OS Hives (fork-specific; see
[`FORK.md`](../../FORK.md)). It asks each Hive's web API how it is doing,
classifies the answers into pass / warn / fail checks, and hands the result to
[`.github/workflows/hive-health.yml`](../../.github/workflows/hive-health.yml),
which keeps one GitHub issue open per failing Hive.

```sh
cd tools/hive-health
go run .                   # all Hives in targets.json
go run . -only school      # one Hive
go run . -json r.json      # also write the JSON report
go test ./...              # classification tests against recorded responses
```

It is a standalone Go module (standard library only) so it adds nothing to
`src/`'s dependency graph or coverage gates, and stays out of upstream syncs.

## What it reads

| Endpoint | Auth | Used for |
| --- | --- | --- |
| `GET /api/health` | public | reachability |
| `GET /api/health/deep` | public | readiness, GitHub auth, governor mode and queue, per-agent state / last kick / pause, stall detection, hub heartbeat |
| `GET /api/contribute/status`, `/api/contribute/activity` | public | served build SHA, contributor activity |
| `https://hub.tunaos.org/activity.json` | public | recent beads per agent, per-agent GitHub output (the "is it producing" signal) |
| `GET /api/status` | **session** | per-agent cadence (so cadence-paused lanes are known, not guessed), `needsLogin`, watchdog conditions, `BLOCKED` evidence (crash loops), system alerts, held PRs, GitHub rate limit |

The public tier is useful on its own: it catches an unreachable Hive, a dead
governor, agents that stopped, stale kicks and a Hive that stopped producing.
What it cannot see is *why* an agent is idle: an agent sitting on a login
screen looks healthy from `/api/health/deep`. That needs the session tier.

The public tier also cannot see cadence. An agent that has not been kicked
since the Hive restarted, for over 24 h, while its peers were, is reported as
**presumed cadence-paused** (skipped, not failed). The session tier replaces
the guess with the configured cadence.

## Checks

Every Hive gets: `reachability`, `deep-health`, `governor`, `governor-kicks`
(newest kick to any active agent), `backlog` (and `backlog-spike` against the
previous run's report), `production` (newest output in the hub snapshot), and
per-agent checks `agent/<name>/{state,kick,output,needs-login,blocked,not-ready}`.
Session tier adds `alert/<id>` and `github-rate-limit`.

Deliberately-off agents are never evaluated: operator-paused, disabled in
config, cadence `paused` in the current mode, or on-demand. Watchdog "alive
but not producing" alerts about those agents are dropped, since the watchdog
raises them for cadence-paused lanes too. Restarts that the watchdog calls a
crash loop but that were all caused by an operator action (the rotation
CronJobs) are `info`, not a failure.

Thresholds live in `config.go` (`DefaultThresholds`) and can be overridden
per run in `targets.json` under `"thresholds"`.

## The issue

Only `fail` opens an issue; warnings are reported in the job summary but never
open or hold one. The issue:

- is found again by a hidden `<!-- hive-health:target=<name> -->` marker;
- is titled `[operations] Hive health: …` and labelled `hive-health` +
  `agent/operations` — both route it to a Hive's operations lane
  (`src/pkg/classify`: title-prefix and label routing), and `tuna-os/hive` is
  in the repo list of both school and reef;
- has its body rewritten every run, gets a comment only when the set of
  failing checks changes, and is closed when the Hive stops failing;
- carries the evidence, the agent table, recent output, and per-failure
  diagnosis steps, split into what an agent may do (diagnose, comment, open a
  PR) and what needs a human (anything that changes the running Hive).

It is never labelled `hold` (that hides it from every agent) or
`ai-fix-requested` (that hands it to Copilot via `ai-fix.yml`); adding
`ai-fix-requested` by hand is the human-approved way to escalate.

## Optional: the session tier

The probe never needs a credential. To enable the session tier for a Hive:

1. Add a **read-role** GitHub account to that Hive's
   `dashboard.authorized_users`, e.g. `hanthor-hive-probe:read` (any entry
   after the first defaults to `read`; the first is the owner). Do not use an
   owner or merger account. If the Hive heartbeats to a hub, grant it through
   the hub's Manage Access instead: the heartbeat re-syncs the list.
2. Sign in to the Hive as that account (GitHub device flow) and copy the
   `hive_session` cookie value.
3. Store it as an Actions secret: `HIVE_HEALTH_SESSION_SCHOOL`,
   `HIVE_HEALTH_SESSION_REEF` or `HIVE_HEALTH_SESSION_REILLY` (names are set
   per Hive in `targets.json`).

Sessions last 30 days. When one expires the probe reports `auth-tier: warn`
and falls back to the public tier; refresh the secret.

Why a session and not a token: every token path the Hive accepts
(`X-Hive-Internal`, the dashboard Bearer token) is owner-equivalent, and a
direct-route Hive disables the Bearer path anyway. A read-role session is the
only credential the Hive itself scopes to read-only (non-GET requests are
refused for role `read`). **Never put `HIVE_DASHBOARD_TOKEN` in an Actions
secret.**

The probe only sends the cookie on `GET /api/status` to the configured host,
refuses cross-host redirects, never reads the pane tail (`liveSummary`),
redacts everything it writes (report, summary, issues are all public), and
scrubs the configured session value from every output.

Operators with cluster access can get the same view without a secret:

```sh
kubectl -n hive exec deploy/hive -- curl -sS -H "X-Hive-Internal: $TOKEN" \
  http://127.0.0.1:3002/api/status > school-status.json
go run . -only school -status-file school=school-status.json   # from tools/hive-health
```
