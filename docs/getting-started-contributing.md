# Getting started as a first-time contributor

This is the path to make your first code or documentation contribution to Hive. It ties together the reference docs and answers Hive questions. To learn the mechanics (branches, DCO, PR format), read [`CONTRIBUTING.md`](../CONTRIBUTING.md). For build and test commands, read [`docs/development.md`](development.md).

## 1. Find something to work on

- Browse the [issue tracker](https://github.com/hivecommons/hive/issues). Issues
  labeled `documentation` and `help wanted` are good entry points; many
  `[guide]` doc-gap issues are small and self-contained.
- Documentation fixes are the fastest way to land a first PR and learn the review process.
- Comment on the issue before you start complex tasks so contributors do not duplicate work.

## 2. Set up a local environment

For most changes you do **not** need a cluster.

- **Go code:** you need Go (see [`docs/development.md`](development.md) for the
  version and `make`/`just` targets). `cd src && go build ./...` compiles the
  binary; that is enough to iterate on most code.
- **Docs:** no toolchain needed — edit Markdown and preview locally.
- **Full system:** run Docker Compose from the repository [Quick Start](../README.md#quick-start-docker-compose) (`docker compose -f src/docker-compose.yaml up -d`). You only need Kubernetes for deployment-specific work.

## 3. Test a change without a cluster

- **Go:** `cd src && go test ./...` runs the unit suite. Most tests are
  hermetic. Tests that need tools such as `kubectl` will skip when tools are absent. See the Test section of [`docs/development.md`](development.md).
- **Shell scripts:** `*.test.sh` / `*.test.js` files under
  [`bin/`](../bin/README.md) are runnable directly with `bash`/`node`.
- **The proxy:** `cd src/proxy && npm test`.
- Do not run local build/lint as a merge gate — CI is the gate (see step 6).

## 4. Key concepts before touching agent policy

If your change touches how agents behave, understand these first:

- **Deterministic pipeline vs. agents.** Scripts in [`bin/`](../bin/README.md) filter, classify, and gate work before an LLM runs. Agents make judgment calls only.
- **ACMM levels.** The [matrix of ACMM policies](../src/docs/acmm-policy-matrix.md) sets what each agent can do at each maturity level (advisory to auto-merge).
- **Agent configuration.** [`agent-configuration.md`](../src/docs/agent-configuration.md)
  is the field-by-field reference; prompt and policy templates live under the
  policies directory.
- **The architecture.** Read [`src/docs/architecture.md`](../src/docs/architecture.md) before you change the governor loop or guardrails.

## 5. How the Hive dev bot interacts with your PR

Hive maintains its own repository with a hive — so a bot can interact with your contribution:

- The hive agents can comment on or triage issues and open PRs.
- Automated review comments or bot PRs that reference your issue show the hive at work on its backlog. Coordinate in the issue thread. Human maintainers make merge decisions on community PRs.
- The bot will neutralize raw mentions. This prevents notifications to everyone on updates. You do not need to take special action.

## 6. Review and CI: what to expect

- Open your PR against the **`v4`** branch (the active development branch).
- CI runs build, tests, a coverage check, and container image builds. Some
  checks (Playwright, `tide`) do not block PRs. The **required** checks are the
  build/test/coverage/docker ones.
- Coverage occasionally flakes; a maintainer will re-run it. A red `PR Verifier`
  status is a known repo-wide quirk, not your change.
- A maintainer reviews and merges when CI passes. Timelines vary. Add a comment to the issue or PR thread if maintainers have not replied for days.

## Next steps

- [`CONTRIBUTING.md`](../CONTRIBUTING.md) — branches, DCO sign-off, PR format.
- [`docs/development.md`](development.md) — build, test, lint, and `just` recipes.
- [`src/docs/README.md`](../src/docs/README.md) — the full documentation index.
- [ClankeR contributor relay](../src/docs/contributor-relay.md) — contribute
  compute to a live hive from your machine.
