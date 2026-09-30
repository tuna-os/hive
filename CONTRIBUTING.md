# Contributing to Hive

Thank you for your interest in KubeStellar Hive. This guide explains how to contribute code and documentation to this repository. To donate compute to a live hive, see [Contribute to a Hive](README.md#contribute-to-a-hive).

**New here?** Read the [contributor intro guide](docs/getting-started-contributing.md). It explains issues, local setup, tests without a cluster, and CI review.

## Where to work

- Open issues and pull requests in this repository. Use the issue templates when they are available, and link related issues from the PR body.
- Discuss design and review questions in GitHub issues and PRs so decisions remain public and searchable.
- Follow the [KubeStellar Code of Conduct](CODE_OF_CONDUCT.md) and [Hive governance](GOVERNANCE.md).
- Report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## Repository layout

- `src/` — the current Go module (`github.com/hivecommons/hive`) and the main development target for this repository.
  - `src/cmd/hive` — main Hive binary.
  - `src/cmd/hivectl`, `src/cmd/apiproxy`, `src/cmd/hive-backup` — command-line tools for operators.
  - `src/pkg/` — Go packages for agents, GitHub integration, agent schedules, policies, dashboards, hubs, backups, and runtime behavior.
  - `src/policies/` — policy prompts and rule files used by the deterministic and agent pipeline. Treat policy changes like code: review the behavior they enable, test where possible, and explain risk in the PR.
  - `src/deploy/` and `src/examples/` — deployment manifests and example configuration.
  - `src/docs/` — architecture and operator/developer reference material.
  - `src/test/` — integration and regression tests.
- `bin/` — deterministic pipeline, supervision, enforcement, deployment, and maintainer helper scripts. See [`bin/README.md`](bin/README.md) for the script-by-script index.
- `config/hive-project.yaml.example` — project metadata for the deterministic shell pipeline at the top level; see [config/README.md](config/README.md). This
  is separate from the Go runtime config in `src/hive.yaml.example`.
- `dashboard/`, `docs/`, `config/`, `systemd/`, `launchd/`, and top-level scripts — assets for the hub, dashboard, install, and operational flows.
- `Justfile` — recipes for the contributor relay; see [`docs/development.md`](docs/development.md#just-recipes).

## Branches

Use `v4` as the base branch for Hive work and PRs unless a maintainer asks otherwise. The `main` branch is not the active target for changes.

Before you start work:

```bash
git fetch origin
git switch -c <topic-branch> origin/v4
```

## Local development

See [docs/development.md](docs/development.md) for the complete setup guide for local development. The short path is:

```bash
cd src
go build ./...
go test ./...
```

The file [`src/go.mod`](src/go.mod) declares the Go version. Install that version or newer tools before you build.

## Contributor recipes

The root [`Justfile`](Justfile) defines recipes for the contributor relay. Run `just --list` to view current recipes.

| Recipe | What it does |
| --- | --- |
| `just contribute-check <backend>` | Runs the same read-only backend CLI preflight used by setup, then reports whether the machine is ready for `contribute-setup`. |
| `just contribute-setup <backend>` | Checks the Justfile version, verifies the backend CLI, signs in with GitHub, registers with the configured hub, and writes `${HOME}/.config/hive/contributor.env`. |
| `just contribute-hive [backend] [mode]` | Starts the contributor relay. The default mode is containerized; pass `local` as the mode to run natively when the local tools are installed. |
| `just contribute-status` | Queries the configured hub for status and contributor profile information. |
| `just contribute-browse` | Discovers available public hive projects. |
| `just contribute-stop` | Stops a background contributor relay if one is running. |
| `just contribute-k8s [namespace] [outfile] [image_tag]` | Emits Kubernetes manifests for a headless contributor workload. It writes to stdout or the requested file; it does not apply the manifest. |
| `just hive-api <endpoint>` | Calls a hub API endpoint, defaulting to `/status`, using the configured hive URL. |
| `just hive-api-docs` | Opens the hub API documentation in a browser. |

See [src/docs/contributor-relay.md](src/docs/contributor-relay.md) for details on the relay for contributors and Kubernetes workloads.

## Style and quality

- Format Go changes with `gofmt`.
- Prefer small, focused PRs with tests or a clear explanation when tests are not practical.
- Keep settings configurable instead of hard-coding environment-specific paths, tokens, or endpoints.
- Do not commit secrets, generated credentials, or local runtime state.
- For documentation changes, verify every command, path, and branch name you mention.

## Test policy

**A change to behavior must include a test that will fail without it.** This is a project rule, not a per-PR negotiation.

- **Bug fixes** must add a test to reproduce the bug. The test fails on the parent commit and passes on the fix. A test that passes on both commits proves nothing.
- **New functionality** must add tests for normal paths and reachable failure modes.
- **Security changes** must assert invariants. A test that merely calls a guard proves nothing. The test must fail when an engineer removes the guard.
- **Tests are in scope for review.** A test that asserts the wrong outcome is worse than no test. It reports green while behavior fails.

When a test is not practical (such as changes to cloud infrastructure or docs), state this in the PR body. Explain how you verified the change.

Static analysis runs in CI (`go vet`, `golangci-lint`, `gosec`, `govulncheck`). Fix findings instead of suppressions. When a suppression is required, explain the reason in a code comment.

## Optional git hooks

The repository includes `githooks/post-checkout`. Install it only if you want the local checkout guard:

```bash
git config core.hooksPath githooks
```

The hook runs after branch checkouts in the primary worktree. It keeps the worktree on `main`, shows guidance, and checks `main` back out. Use it for dashboard checkouts where you do feature work in separate worktrees from `git worktree add`. It does not run for file checkouts or linked worktrees, because linked worktrees use a `.git` file.

If you do not install the hook, normal Git behavior applies. If it stops a checkout, use a separate worktree or remove the `core.hooksPath` setting.

## DCO sign-off

Every commit must include a Developer Certificate of Origin sign-off. Use:

```bash
git commit -s
```

The sign-off adds a `Signed-off-by:` trailer to certify that you have the right to submit the change under the repository license. If you forget, amend the commit with `git commit --amend -s` and force-push your branch.

## Pull requests

- Target `v4` for all code and documentation contributions (the active development branch).
- Start PR titles with the repository's emoji convention, for example `📖 docs: ...`, `🐛 fix: ...`, or `✨ feature: ...`.
- Include `Fixes #<issue>` lines for issues the PR closes.
- Describe what changed, why, and how you tested it.
- Include the relevant command output or a short note such as `Not run (docs only)` when tests are not applicable.
- Add a changelog fragment in [`changelog.d/`](changelog.d/README.md) for user-visible changes (features, fixes, security, migrations, deprecations). See below.
- Refactors, tests, and dependency updates do not need fragments. Add the `no-changelog` label if the check prompts for one.
- Do not edit `CHANGELOG.md` directly. Changes to that file create merge conflicts ([#5675](https://github.com/hivecommons/hive/issues/5675)). CI compiles fragments during releases.
- Maintainers want focused PRs instead of broad edits.

## Changelog fragments

One file per PR, named `changelog.d/<category>-<pr-or-slug>.md` where the category (`added`, `changed`, `deprecated`, `fixed`, `security`) picks the CHANGELOG subsection — and, through it, the semver bump of the next release. The file's content is exactly your entry: a single `- ` bullet in the same narrative style as existing `CHANGELOG.md` entries, no headings. The complete workflow:

```bash
echo '- The relay no longer drops long tasks ([#1234](https://github.com/hivecommons/hive/issues/1234)).' > changelog.d/fixed-1234-relay-drop.md
git add changelog.d/fixed-1234-relay-drop.md
git commit -s
```

`changelog.d/README.md` has the full format, the `no-changelog` exemption, and the release-marker escape hatch. Transition note: direct `CHANGELOG.md` edits are still accepted until 2026-09-09 so in-flight PRs can land unreworked.

## Maintainer resources

Project governance lives in [GOVERNANCE.md](GOVERNANCE.md). The current owner/approver signal is also reflected in [OWNERS](OWNERS). Report security issues through [SECURITY.md](SECURITY.md), not public issues.
