# Local development

This guide describes the local development workflow for the Hive Go codebase on `v4`.

## Prerequisites

- Git and GitHub CLI (`gh`) for normal issue and PR workflows.
- Go `1.25.6`, as declared by [`src/go.mod`](../src/go.mod).
- Docker or Podman if you use container relay or deployment paths.
- `tmux` for local agent/contributor workflows that attach CLIs to terminal sessions.
- `just` if you use the repository's helper recipes (`brew install just` on macOS, or install from the `just` project for your platform).
- Optional agent CLIs for local tests: Claude Code, GitHub Copilot, Gemini, Bob, Goose, Codex, Pi, or Antigravity.

## Clone and branch

```bash
git clone https://github.com/hivecommons/hive.git
cd hive
git switch -c <topic-branch> origin/v4
```

Use `v4` as the PR base for ordinary Hive development. Rebase or recreate your branch from a fresh `origin/v4` before you open or update a PR.

## Build

Build every package in the Go module:

```bash
cd src
go build ./...
```

To build only the main Hive binary during quick iteration:

```bash
cd src
go build ./cmd/hive
```

## Test

Run the module tests from `src/`:

```bash
cd src
go test ./...
```

The `src/test/` package holds the inception e2e/regression suite. Those tests talk to a **live hive over the network**. They use the `integration` build tag. The command above compiles the package but runs none of them. A plain `go test ./...` will not run this suite. To run the suite:

```bash
cd src
HIVE_URL=http://<host>:<port> HIVE_TOKEN=<token> go test -tags integration ./test/...
```

The suite skips (exit 0) when `HIVE_URL` is unset or the endpoint fails to answer. A run without those variables means "skipped", not "verified". Run it when you touch inception code; the normal `go test ./pkg/...` loop is enough otherwise. See `src/test/doc.go` for the package's own description.

If an environment dependency stops a full run, document the package and error in your PR. Run the individual package tests that match your change.

Useful narrower loops:

```bash
cd src
go test ./pkg/...
go test ./cmd/hive
```

### The plain command above is weaker than the gate

`go test ./...` is not what CI runs. Every shard in
[`.github/workflows/v2-tests.yml`](../.github/workflows/v2-tests.yml) — the
workflow that publishes the required `test` check — uses the same three flags:

```bash
cd src
go test ./pkg/hub     -short -race -count=1 -run '<1/3 slice>'   # test (hub i/3)
go test ./pkg/agent   -short -race -count=1 -run '<1/5 slice>'   # test (agent i/5)
go test $PKGS         -short -race -count=1                      # test (rest i/3)
```

The workflow shards tests by time. Packages `pkg/hub` and `pkg/agent` run in separate jobs by test function. Other packages divide into balanced buckets. Every shard runs with `-v` and writes slowest tests to the step summary.

Check that summary before local profiles when tests run slow. Scheduled runs add `-shuffle=on`. The PR gate will disable shuffle, so test order failures create issues instead of red PRs. Shards cover all of `./pkg/...` and `./cmd/...`. The difference from your local loop is the set of flags, not the test files. To run tests locally like the gate:

```bash
cd src
go test ./pkg/... ./cmd/... -short -race -count=1
```

What each flag changes:

- **`-race`** catches bugs that slip past normal runs. A data race or lock order mistake will pass non-race tests and fail under load in production. For example, concurrent writes to a WebSocket created deadlocks in [`src/pkg/dashboard/contribute_ws.go`](../src/pkg/dashboard/contribute_ws.go). Before you push changes, run the race build on packages you touched.
- **`-short`** sets `testing.Short()`. Tests with `if testing.Short() { t.Skip(...) }` do not run in CI. A slow test without this guard slows down every shard. But a test behind a `Short` guard never runs in the gate. A green check says nothing about that test. Guard slow setup only. Do not guard assertions that prove a fix.
- **`-count=1`** disables the test cache. Without it, an unchanged package returns cached results instead of fresh runs. You want fresh runs when you debug failures or flakes.

Two flags that appear in CI but are *not* part of the PR gate:

- **`-timeout 600s`** runs only in the hourly coverage workflow ([`.github/workflows/coverage-hourly.yml`](../.github/workflows/coverage-hourly.yml)). The PR shards use the default 10-minute timeout for Go binaries.
- **`-coverprofile`** generates coverage data on each shard. CI scores coverage, but only failed tests will block merge gates.

Neither the PR gate nor the cron runs `./test/...`. Both run `./pkg/...` and `./cmd/...`. The integration suite also needs the `integration` tag and a live hive.

## Format and lint expectations

Run `gofmt` on Go files you edit:

```bash
gofmt -w path/to/file.go
```

The v4 CI workflow runs `go vet ./...` after it builds the Hive binary. Run that check locally:

```bash
cd src
go vet ./...
```


## If CI says "NOTICE is out of date"

`NOTICE` lists Go modules in the binary. Scripts generate this file. Changes to `src/go.mod` or `src/go.sum` make it stale and fail the `notice-drift` check in `.github/workflows/go-security-analysis.yml`.

Regenerate and commit it:

```bash
bash src/scripts/generate-notice.sh   # writes NOTICE at the repo root
```

Three things that will otherwise cost you a CI round trip:

- **Commit output verbatim.** The check is byte-exact. Do not reformat text or strip trailing whitespace. Some licenses contain trailing spaces. Edits create diffs against CI output.
- **The generator needs Go from the module.** The file `src/go.mod` pins a version. If you run an older Go toolchain, `go-licenses` fails to resolve packages and aborts.
- **Red checks may come from base branches.** If a dependency merge missed an update to `NOTICE`, `v4` becomes stale. Open PRs inherit this failure. Check whether `v4` is clean before you debug your change.

A `FORBIDDEN` result means the module graph contains an unapproved license. Remove or replace the dependency.

The root Justfile does not define a `lint` recipe. Use `go vet ./...` to check code locally, with `gofmt`, `go build`, and `go test`.

## Running Hive locally

The quickest operator path remains Docker Compose from the root README:

```bash
cp src/hive.yaml.example src/hive.yaml
# src/.env, NOT ./.env — `-f src/docker-compose.yaml` makes `src/` the project
# directory, so that is the `.env` Compose reads. A root `.env` is ignored.
echo "HIVE_GITHUB_TOKEN=ghp_..." > src/.env
# REQUIRED: the dashboard's auth proxy refuses to start without it.
printf 'HIVE_DASHBOARD_TOKEN=%s\n' "$(openssl rand -hex 32)" >> src/.env
docker compose -f src/docker-compose.yaml up -d
```

For source-level debugging, build with `go build ./cmd/hive` from `src/` and run the generated binary with a local `src/hive.yaml`. Keep real tokens in your shell or local ignored `.env` files, never in commits.

## Just recipes

The root [`Justfile`](../Justfile) is the discoverable entry point for contributor relay automation. List the public recipes with:

```bash
just --list
```

Current recipes focus on the **contribute** workflow:

- `just contribute-check <backend>` — read-only preflight for an agent backend CLI.
- `just contribute-setup <backend>` — one-time setup for GitHub auth, hub registration, and backend readiness.
- `just contribute-hive [backend] [mode]` — start work on a hive with a container (or local mode on request).
- `just contribute-status`, `just contribute-browse`, and `just contribute-stop` — inspect, discover, or stop contributor relay activity.
- `just contribute-k8s [namespace] [outfile] [image_tag]` — generate Kubernetes manifests for headless workloads. It prints or writes manifests without apply actions.
- `just hive-api <endpoint>` and `just hive-api-docs` — check API endpoints on the hub for the configured hive.

Tasks not shown in `just --list` are internal. Use Go, Docker Compose, and Kubernetes commands from the README and `src/docs/`.

## Before opening a PR

1. Rebase on the latest `origin/v4`.
2. Run the build and tests that match your change.
3. Commit with DCO sign-off: `git commit -s`.
4. Open a PR against `v4` with an emoji title, verification notes, and `Fixes #...` lines to close issues.
