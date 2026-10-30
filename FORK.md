# About this fork

`tuna-os/hive` is a fork of [`hivecommons/hive`](https://github.com/hivecommons/hive)
(formerly `kubestellar/hive`; the old URL redirects).
It exists so that the Tuna OS organization can run the Hive agent fleet against
its own repositories and container registry. It is **not** a separate project,
and it is not a place to develop new Hive features.

Read this page before opening an issue or a pull request here: several documents
in this repository are inherited from upstream unchanged and describe the
upstream project, not this fork.

## Intent: pilot features on upstream v6, then upstream them

`tuna/v6` is the fork's pilot line: upstream `v6` plus the features and fixes
Tuna OS is proving out. New Hive features land there first and run on the
Tuna OS hives; an upstream RFC or PR follows once a feature has shown its
numbers on a live hive, not before. Every patch on `tuna/v6` carries a
disposition (proposed upstream, reported upstream, or pilot only), and a
patch that upstream merges is dropped at the next rebase onto `v6`.

This `v4` branch tracks the upstream `v4` branch, which upstream froze on
2026-10-05. **The fleet does not run `v4`** — see "What the fleet runs" below.

Current delta against `hivecommons:v4` (after the 2026-09-05 sync in #30):

| Commit | Change | Disposition |
| --- | --- | --- |
| `4955b9f` | Publish images to `ghcr.io/${{ github.repository_owner }}` instead of the hardcoded upstream org | Generic to any fork — should be upstreamed, tracked in [#2](https://github.com/tuna-os/hive/issues/2) and [#10](https://github.com/tuna-os/hive/issues/10) |
| `1433a16` | Run CI on GitHub-hosted runners instead of upstream's self-hosted fleet | Fork-specific: upstream's runner labels do not exist here |
| `2f6421c` | Point the PR Verifier at the `pr-verifier` image's current org (#29) | Generic — upstream candidate |
| `3b4fc73`, `01f9ef8` | ACMM evaluation: report "GitHub did not answer" as unknown, not as a missing file (#32) | Generic — upstream candidate |
| `aff6235` | Agent onboarding files at the repository root (`AGENTS.md`, `CLAUDE.md`, editor and tool entrypoints) (#34) | Generic — upstream candidate |
| `f29a791` | `governor.acmm.repo_roots`: probe a configured sub-root so `src/`-layout repos are seen (#35) | Generic — upstream candidate |
| (this PR) | `tools/hive-health/` + `hive-health.yml`: hourly read-only probe of the live Tuna OS Hives, one self-closing `[operations]` issue per failing Hive | Fork-specific: it watches this org's deployments. Kept outside `src/` so it never conflicts with a sync |

Regenerate this table with `git log --oneline --no-merges hivecommons/v4..v4`.

Every entry in that table is a liability, not an asset: a local patch has to be
rebased on every sync, and it grows a merge-conflict surface as upstream moves.
A change that would benefit any fork belongs upstream. A change that is
genuinely specific to Tuna OS belongs in configuration, not in a patch.

## Staying current with upstream

Upstream `v4` is fast-moving — on 2026-09-02 it landed 96 commits in a single
day, and by 2026-09-05 this fork was 301 commits behind before #30 brought it
current. A fork that is synced by hand is behind by hundreds of commits within
a week, which means upstream bug fixes and security hardening reach the Tuna OS
fleet on no defined schedule.

Syncs so far are manual, wholesale merges of upstream `v4` (with the fork's
commits carried on top). A drift budget and an automated sync are proposed but
**not yet adopted**; the decision is tracked in
[#18](https://github.com/tuna-os/hive/issues/18). Until it lands, treat "how far behind is this fork?" as a question to answer before
relying on any upstream fix being present:

```sh
gh api repos/tuna-os/hive/compare/hivecommons:v4...tuna-os:v4 \
  --jq '{ahead: .ahead_by, behind: .behind_by}'
```

## Where to send things

| You have | Send it |
| --- | --- |
| A feature idea for Hive | Pilot it on `tuna/v6` and run it on a Tuna OS hive first; then upstream, with its live numbers |
| A bug in Hive itself | Upstream: [`hivecommons/hive` issues](https://github.com/hivecommons/hive/issues), and fix it on `tuna/v6` if the fleet needs it now |
| A change that any fork would want | Upstream, as a pull request |
| A security vulnerability in Hive code | Upstream, via private vulnerability reporting on `hivecommons/hive` |
| Something specific to how Tuna OS runs its fleet — registry, org configuration, the fork's own sync | Here |

`SECURITY.md` in this repository is upstream's text. It directs reporters to
this repository's Security tab and says reports are handled by the Hive
Maintainer Committee — those two statements do not both hold for a fork. A
report filed here reaches the fork owner only. Vulnerabilities in Hive code
should go upstream.

## Documents that describe upstream, not this fork

These files are inherited verbatim and are kept unedited so the fork stays cheap
to rebase. Read them as upstream's, and scope them with this page:

- `OWNERS` — the KubeStellar Maintainer Committee. It governs merges upstream,
  not in this fork.
- `GOVERNANCE.md` — upstream's decision-making process, including the RFC and
  supermajority rules.
- `ROADMAP.md` — upstream's release-line trajectory (v4, v5). This fork has no
  release line of its own.
- `ADOPTERS.md` — upstream's adopter roster, which lists Tuna OS as an adopter
  of the upstream project.
- `SECURITY.md` — upstream's disclosure policy; see the routing table above.
- `CONTRIBUTING.md` and `CODE_OF_CONDUCT.md` — upstream's, and they apply to
  contributions sent upstream.

## Where the fleet runs

The README's [Tuna OS deployment](README.md#tuna-os-deployment) section lists
the live hosts and the health check that shows each one is up. Since
2026-10-25 hub, school and reef run as Docker Compose services under
`/srv/hives` on the Spindle VPS, not on Kubernetes.

## What the fleet runs

The services use the upstream `ghcr.io/hivecommons/hive:v6-latest` image, but
`/srv/hives/compose.override.yaml` bind-mounts a fork-built binary over
`/usr/local/bin/hive`, so the image tag does not say what runs. On school and
reef it also mounts `/srv/hives/proxy/server.js` over the image's Node proxy:
the image's own file plus the `bb82aed` change below
(`/srv/hives/proxy/server.image.js` is the unpatched original).

| Since (UTC) | Services | Branch | Commit | Base |
| --- | --- | --- | --- | --- |
| 2026-10-30 03:31 | hub, reef, school | `tuna/v6` | `62dbda5` | same |
| 2026-10-29 22:25 | hub, reef, school | `tuna/v6` | `a60b5cf` | upstream `v6` @ 2026-10-29 (`463615a`) |
| 2026-10-29 22:05 | reef (canary) | `tuna/v6` | `fcbc71f` | same |
| 2026-10-29 16:40 | hub, school (reef until 22:05) | `tuna/fleet` (= `fix/agent-broker-no-proxy`) | `14409c3` | upstream `v5` @ 2026-10-27 (`5e8a689`) |
| 2026-10-29 12:47 | hub, reef, school | `feat/agent-proposal-gate` | `911e5c0` | same |

Delta of `tuna/v6` against upstream `v6`:

| Commit | Change | Disposition |
| --- | --- | --- |
| `eea4d3d` | `**/` path globs match with `path.Match` (`**/*luks*` fires) | Merged upstream on `v5` as hivecommons/hive#15387; drop when it reaches `v6` |
| `504b6bc`, `fcbc71f` | `project.issue_filter.agent_proposal`: hold unacknowledged hive-filed proposals at admission | Pilot; RFC hivecommons/hive#15329 |
| `e51c5c7` | Agents get the `OMP_AUTH_BROKER_URL` host in `NO_PROXY` | Upstream PR hivecommons/hive#15433 (contributor) |
| `a60b5cf` | `governor.freeze_windows`: pause listed agents on fixed days of a repeating cycle. School runs `iso-weekend` (7-day cycle from Mon 2026-11-02, days 6–7 UTC: architect, strategist, outreach) ahead of the Sunday 22:00 UTC ISO publish | Pilot |
| `bb82aed` | Node proxy admits `POST /api/gh-user-auth/start` and `/poll` and passes `hive_session` requests to the Go API, so device-flow sign-in works on direct-route spokes. Also mounted as `proxy/server.js` | Pilot; file upstream |
| `d33143c` | Hub reads spoke proof tokens from `/data/saas/spoke-tokens/<hive-id>` before trying `kubectl`, so hub-proxied single sign-on works for a Compose fleet. Staged, not in use until the hub has a GitHub OAuth app | Pilot |
| `62dbda5` | `ioscan.classifier.backend: omp`: the semantic classifier runs through omp and the auth broker (school and reef: `anthropic/claude-haiku-4-5`; agents moved from `github-copilot/gpt-6-luna` to `openai-codex/gpt-6-luna` the same night) | Pilot |

Delta of `tuna/fleet` (the `v5`-based line, retired 2026-10-29 22:25 when every service moved to `tuna/v6`) against its upstream base:

| Commits | Change | Disposition |
| --- | --- | --- |
| 4, ending `a9e21b5` | Hub-coordinated shared CLI session pool | Merged upstream as hivecommons/hive#15049; drop on the next rebase |
| `6ad4ed4` | `SendKick` re-locks before the shared-pool auto-start error return (the crash loop: 114 restarts in two days) | Fixed upstream in #15049; drop on the next rebase |
| `911e5c0` | `project.issue_filter.agent_proposal`: hold unacknowledged hive-filed proposals at admission | Proposed upstream: hivecommons/hive#15329 |
| `14409c3` | Agents get the `OMP_AUTH_BROKER_URL` host in `NO_PROXY`, so the proxy's private-address guard no longer refuses the credential broker (agent CLIs were exiting on start) | Reported upstream: hivecommons/hive#15432 |

Update this table in the same change as any binary swap. Check a running
service with `docker exec hives-<svc>-1 sha256sum /usr/local/bin/hive` and
compare with `sha256sum` of the `bin/hive-<commit>` it should mount. Tracking
issue for the rollout: tuna-os/tunaOS#4072.

## Ownership

The fork is maintained by the Tuna OS organization. Decisions about the fork —
whether to sync, what delta to carry, whether to keep the fork at all — are made
here. Decisions about Hive itself are made upstream.
