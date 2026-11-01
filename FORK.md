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
The Matrix pilot also mounts its commit-matched dashboard HTML over
`/opt/hive/proxy/public/index.html`: replacing only the Go binary leaves the
public browser UI on the old image's page.

Current pins: Reef and School `e0c044e15`; Hub remains on `230f745`.
The operator scoped this rollout to passive Code activity on the two spokes.

| Since (UTC) | Services | Branch | Commit | Base |
| --- | --- | --- | --- | --- |
| 2026-11-01 02:44 | school | `feature/matrix-cascade-diffs` | `e0c044e15` | existing v6/Matrix pilot plus readable diffs and opt-in workspace snapshots; final verification-buffer reset |
| 2026-11-01 02:43 | reef (canary first) | `feature/matrix-cascade-diffs` | `e0c044e15` | same; matched binary and Node-served HTML |
| 2026-10-31 22:36 | school | `deploy/upstream-v6-matrix` | `637bae44f` | same v6/Matrix base and repository-name fix as Reef |
| 2026-10-31 21:56 | reef (repo-name fix canary) | `deploy/upstream-v6-matrix` | `637bae44f` | same v6/Matrix base, plus qualified repository names for patch observers |
| 2026-10-31 18:23 | school | `deploy/upstream-v6-matrix` | `71ce0ef56` | upstream `v6` `6f6a5ca63`, plus Matrix ports from `v5` |
| 2026-10-31 18:08 | reef (canary) | `deploy/upstream-v6-matrix` | `71ce0ef56` | upstream `v6` `6f6a5ca63`, plus Matrix ports from `v5` ([#172](https://github.com/tuna-os/hive/pull/172)) |
| 2026-10-30 20:30 | hub, reef, school (reef canary from 20:25) | `tuna/v6` | `230f745` | upstream `v6` `463615a` |
| 2026-10-30 03:31 | hub, reef, school | `tuna/v6` | `62dbda5` | same |
| 2026-10-29 22:25 | hub, reef, school | `tuna/v6` | `a60b5cf` | upstream `v6` @ 2026-10-29 (`463615a`) |
| 2026-10-29 22:05 | reef (canary) | `tuna/v6` | `fcbc71f` | same |
| 2026-10-29 16:40 | hub, school (reef until 22:05) | `tuna/fleet` (= `fix/agent-broker-no-proxy`) | `14409c3` | upstream `v5` @ 2026-10-27 (`5e8a689`) |
| 2026-10-29 12:47 | hub, reef, school | `feat/agent-proposal-gate` | `911e5c0` | same |

Pilot-line patches and upstream dispositions:

| Commit | Change | Disposition |
| --- | --- | --- |
| `eea4d3d` | `**/` path globs match with `path.Match` (`**/*luks*` fires) | Merged upstream as hivecommons/hive#15387; included in the `6f6a5ca63` refresh |
| `504b6bc`, `fcbc71f` | `project.issue_filter.agent_proposal`: hold unacknowledged hive-filed proposals at admission | Pilot; RFC hivecommons/hive#15329 |
| `e51c5c7` | Agents get the `OMP_AUTH_BROKER_URL` host in `NO_PROXY` | Merged upstream as hivecommons/hive#15649; refresh adopts upstream's hardened helper and removes the duplicate fork helper |
| `a60b5cf` | `governor.freeze_windows`: pause listed agents on fixed days of a repeating cycle. School runs `iso-weekend` (7-day cycle from Mon 2026-11-02, days 6–7 UTC: architect, strategist, outreach) ahead of the Sunday 22:00 UTC ISO publish | Pilot |
| `bb82aed` | Node proxy admits `POST /api/gh-user-auth/start` and `/poll` and passes `hive_session` requests to the Go API, so device-flow sign-in works on direct-route spokes. Also mounted as `proxy/server.js` | Reported upstream: hivecommons/hive#15872 |
| `d33143c` | Hub reads spoke proof tokens from `/data/saas/spoke-tokens/<hive-id>` before trying `kubectl`, so hub-proxied single sign-on works for a Compose fleet. Staged, not in use until the hub has a GitHub OAuth app | Pilot |
| `62dbda5` | `ioscan.classifier.backend: omp`: the semantic classifier runs through omp and the auth broker (school and reef: `anthropic/claude-haiku-4-5`; agents moved from `github-copilot/gpt-6-luna` to `openai-codex/gpt-6-luna` the same night) | Proposed upstream: hivecommons/hive#15873 |
| `230f745` | `revert_sheriff`: when a merge turns a required check red on the default branch, find the merge, re-run its failing job once, and (propose mode) open a held `hive/revert` + `fixes-main` revert PR ([tuna-os/hive#170](https://github.com/tuna-os/hive/pull/170)). School runs it in **shadow** mode for `tunaos`, `tunaos-packages`, `corral`, `tacklebox` | Pilot (SDLC HIVE-8 / TUNA-4) |
| `71ce0ef56` | Ambient Matrix pilot: Rain, Cascade, Ghost coder and Terminal effects from upstream #15945, #15946, #15967, #15982, #15977, #16020, #15985 and #16046, adapted to v6. Also gates sheriff CI recovery through upstream's shared `ci_rerun` writeback policy | Upstream feature ports, fork integration [#172](https://github.com/tuna-os/hive/pull/172); opt-in per spoke, no Twitch broadcast |
| `637bae44f` | Matrix observations use resolved `owner/name`, not a short `project.repos` display name. Short names otherwise fail the exact Matrix allowlist and silently discard every patch | Fork fix with regression case; CI passed, deployed to Reef and School |
| `e0c044e15` | Passive Code activity: full green additions/red removals, separate in-progress/pushed provenance, managed and isolated-agent-owned temporary workspace snapshots, whole-block privacy/bounds, pause and explicit replay | Fork [#174](https://github.com/tuna-os/hive/pull/174); exact-source CI passed; deployed to Reef and School with separate workspace consent. Proposed upstream as held [RFC #16137](https://github.com/hivecommons/hive/issues/16137), with screenshots; not accepted upstream |

For `71ce0ef56`, the ARM64 binary SHA-256 is
`9f25d95df53ea24e03f267f7b5c097b1686b91318323e82122f77facb781e0ca`;
matching Node-served dashboard HTML:
`0ae9f9575e0f4b5cf44e431b387ac870b10473f486d0db622a8ff5b9175b9444`.
[Full Go CI](https://github.com/tuna-os/hive/actions/runs/41227099240)
passed 27 jobs (three skipped; no coverage verdict), and
[both architecture builds](https://github.com/tuna-os/hive/actions/runs/41227085681)
passed their version smoke. PR #172 remains unmerged: the inherited DCO
squash-attribution policy does not recognize the imported upstream v6 history.
That successful test run covers `71ce0ef56`; it does not certify later commits.
The pilot is separate from merge approval and does not bypass merge policy.

The `637bae44f` ARM64 binary SHA-256 is
`b930b03e3d4e2d88baf9dc963cae995859d2ea5c659ef407e10b5b355b4ae5c6`.
It reuses the byte-identical `dashboard-71ce0ef56.html` mount.
[Its binary build](https://github.com/tuna-os/hive/actions/runs/41233789674)
passed; [its full-suite run](https://github.com/tuna-os/hive/actions/runs/41233790530)
passed 27 jobs, with three skipped, on the exact deployed commit.
Both spokes run the matching binary; Hub was not restarted.
Both original feeds returned `seq: 0` despite School completing a 254-PR
sentinel pass: short configured repository names could not match the qualified
Matrix allowlist. After the fix, live captures were observed at 23:11 UTC
(School) and 23:18 UTC (Reef); both later returned `seq: 1200` and 1,200 lines.
Reef's first cold-start reconciliation had exhausted its App quota and skipped
optional sweeps; capture recovered after quota reset without bypassing a guard.

The real browser surfaces rendered Rain and Ghost on Reef, Cascade and Terminal
on School. Reef's P-key pause/resume changed the visible status as expected.
A sample from each feed matched a genuine added line, after the source's
whitespace trimming, in public [tuna-os/Tavern#323](https://github.com/tuna-os/Tavern/pull/323),
`src/url_security.py`, head `ac9349dba541c1c9297a4282565ce577ceabd7a9`.
The same PR can appear in both feeds because their watched repositories overlap;
these are per-spoke counts, not a deduplicated hub total.
Authorized responses were HTTP 200 with `Cache-Control: no-store`; anonymous
requests, including a forged `X-Hive-Role: owner`, returned 401 on both spokes.
This proves buffered PR-patch capture/playback, not live editor keystrokes.

The `e0c044e159d7c0c0fa45edd07897aea3e196c9cc` Code activity pilot replaces
Cascade's scattered lines with a readable diff reader. The Activity Monitor's
**Code activity** link opens `/?kiosk=1&view=matrix&mx.effect=cascade`.
In-progress snapshots compare a managed workspace with HEAD; pushed checkpoints
retain their real PR links. Observation time and workspace ownership are not
claims of typing time or model authorship. Replay is explicit and off by default.

[Exact-source full CI](https://github.com/tuna-os/hive/actions/runs/41263978136)
passed 27 jobs, with three skipped and no coverage verdict.
[Both stamped architecture builds](https://github.com/tuna-os/hive/actions/runs/41263978117)
passed. The ARM64 binary SHA-256 is
`a2b755acffbf76a6b33b72175b266df6757d883c3e8d21add727a853fdb2deb4`;
matching `dashboard-e0c044e15.html` SHA-256 is
`403a5a9befcc98def6fd2f711a8c408af6828c49ee00f2680cc422819b908688`.
Both running mounts and the ARM64 version output matched these artifacts.

Controlled, explicitly non-agent-authored edits in genuine public Tavern clones
proved native capture on Reef's managed directory and School's isolated-agent
temporary directory. Each feed advanced from 12 to 24 real changed lines after
a second filesystem edit. Native browsers showed complete red/green paragraphs,
blanks, indentation, repeated lines, correct provenance and newly-received labels.
The fake-credential region was omitted whole. Pause held the native scroll;
390px reduced-motion rendering wrapped without horizontal overflow. The actual
dashboard link opened the reader. Authorized APIs returned 200 with `no-store`;
anonymous and forged-role requests remained 401 on both spokes.

Verification fixtures were removed and their transient buffers cleared with
same-container restarts. Both clean APIs then returned `source: "code-activity"`,
`workspaces: true`, `latest: 0`, without verification rows or rendered cards.
No synthetic checkpoint was injected. Published rendering was exercised with
the actual public Tavern #323 diff; a fresh native pushed checkpoint after this
rollout was not forced or claimed. The existing sentinel author/change detection,
polling and quota gates still control that source.

Stopped snapshots are `/srv/hives/backups/reef-code-activity-20261101T022412Z/`
and `school-code-activity-20261101T023300Z/`: data, both configs, the override,
and temporary files. School's two temporary Git subtrees were archived separately
and restored before the replacement container started, with numeric ownership and
permissions preserved. Both spokes deliberately enable `dashboard.matrix.workspaces`
in seed and runtime; public allowlists and additional exclusions are unchanged.
Hub's `2026-10-30T20:30:55.719198892Z` start time is unchanged.


Matrix is opt-in in each spoke's seed **and** runtime config, with explicit
public-repository allowlists and additional config/deployment-path exclusions.
The original Matrix builds initialized after reconciliation's sentinel sweep and
could return 404 while cold. The Code activity build initializes an enabled source
on an authenticated read; no eligible data is a 200 empty feed, not invented motion.
Anonymous public Matrix requests returned 401/403; kiosk is not an auth bypass.
Data, both configs and the Compose override were backed up before each restart:
Reef `20261031T180544Z`, School `20261031T182146Z`, under `/srv/hives/backups/`.
The Reef fix canary has a fresh consistent snapshot and config/override backups
at stamp `20261031T215350Z`; its immediate previous binary is `71ce0ef56`.
School's fix snapshot/config/override backup stamp is `20261031T223247Z`,
also rolling back to `71ce0ef56` without a data-format downgrade.
For rollback, restore only the affected spoke's previous binary, matching HTML
and both configs; preserve other services' pins. The Code activity pilot's previous
pair is `hive-637bae44f` with `dashboard-71ce0ef56.html`. Restore temporary Git
archives before starting a recreated container. Do not restore a stale data
snapshot over newer agent work unless downgrade compatibility requires it.

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
