# Working in tuna-os/hive

Instructions for coding agents (Claude Code, Copilot, Cursor, Hive's own
agents) and for humans who want the same short version. Everything here is
verified against the tree; when the tree and this file disagree, fix this file.

## What this repository is

The `tuna-os` fork of [hivecommons/hive](https://github.com/hivecommons/hive),
deployed at [hive.tunaos.org](https://hive.tunaos.org) to run agents across
the `tuna-os` organization. Default branch is `v4`, tracking upstream `v4`.

Fork policy: keep the delta near zero and send generic changes upstream. The
one deliberate, permanent delta is CI `runs-on:` values (`ubuntu-latest` /
`ubuntu-24.04-arm` instead of upstream's `[self-hosted, hive]`), re-applied on
every sync. In practice the fork carries more than that — the current
fork-only commits, and which of them are waiting to go upstream, are listed
in [FORK.md](FORK.md). Syncs are merges of `upstream/v4` with a merge commit —
never a squash, which re-creates duplicated history.

## Layout

- `src/` — the Go module (`github.com/hivecommons/hive`, Go 1.25.6). Everything
  buildable lives here: `cmd/hive`, `pkg/`, `internal/`, `test/`.
- `src/pkg/dashboard/` — dashboard API and the single-file UI
  `static/index.html`. ACMM evaluation: `acmm_criteria.go`, `api_acmm_eval.go`.
- `src/policies/` — per-agent, per-mode prompt templates (`<agent>-<mode>.md`).
- `src/AGENT-DEFINITION.md` — the agent configuration reference.
- `src/docs/` — operator and design docs; `docs/` — repo-level guides
  (`development.md`, `troubleshooting.md`).
- `bin/` — operational shell scripts and their contract tests (`bin/test_*.sh`).
- `.github/workflows/` — 30+ workflows; `v2-tests.yml` is the test gate.

## Build, test, lint

Run from `src/`:

```bash
cd src
go build ./...
go vet ./...
go test ./...            # unit tests; src/test/ needs -tags integration and a live hive
golangci-lint run ./...  # config: src/.golangci.yml
```

Coverage is gated per package in `.github/workflows/v2-tests.yml`: default
floor 90%, with per-package floors (`dashboard` 84, `hub` 87, `agent` 89).
New code needs tests or the gate fails.

The dashboard UI is inline JavaScript. Lint it the way CI does:

```bash
(cd .github/scripts && npm ci --ignore-scripts --no-audit --no-fund)
node .github/scripts/check-inline-js.js src/pkg/dashboard/static/index.html
```

Shell scripts under `bin/` have contract tests: run `bash bin/test_<name>.sh`
for the script you changed.

## Contributing rules that CI enforces

- Every commit carries a DCO sign-off: `git commit -s`.
- PRs target `v4`. Title uses the emoji convention (`🐛 fix: …`,
  `✨ feature: …`, `📖 docs: …`).
- User-visible changes get a changelog fragment
  `changelog.d/<category>-<pr-or-slug>.md` (see `changelog.d/README.md`); do
  not edit `CHANGELOG.md`'s `Unreleased` heading directly. Test-only and
  refactor changes need none.
- `verify PR contents` (pr-verifier) is the required check; it runs the
  workflow from the base branch.
- Never skip, disable or quarantine a test to get green. Never force-push
  someone else's branch.

## Agents operating on this repository

Hive agents (scanner, quality, ci-maintainer, sec-check, architect,
strategist, guide, outreach) open issues and PRs here as
`hanthor-hive-agent[bot]` and label them by agent. Their PRs carry `hold` or
`needs-human` below full autonomy; a human clears the label. ACMM gap issues
carry `acmm` + `ai-fix-requested` and name the criterion in
`**Criterion ID:**` — the same marker the dashboard uses to avoid filing a
duplicate.

To see how this repository scores against the ACMM criteria without a running
hive, use the `acmm-evaluate` skill in `.claude/skills/`.
