# About this fork

`tuna-os/hive` is a fork of [`hivecommons/hive`](https://github.com/hivecommons/hive)
(formerly `kubestellar/hive`; the old URL redirects).
It exists so that the Tuna OS organization can run the Hive agent fleet against
its own repositories and container registry. It is **not** a separate project,
and it is not a place to develop new Hive features.

Read this page before opening an issue or a pull request here: several documents
in this repository are inherited from upstream unchanged and describe the
upstream project, not this fork.

## Intent: track upstream, keep the delta near zero

The fork tracks the upstream `v4` branch. The goal is to carry **no** local
patches beyond what is unavoidably org-specific, and to upstream anything that
is not.

Recorded fork-side delta (last upstream comparison: 2026-09-24, 1,273 commits behind and 37 ahead; the 2026-09-05 sync landed in #30):

| Commit | Change | Disposition |
| --- | --- | --- |
| `4955b9f` | Publish images to `ghcr.io/${{ github.repository_owner }}` instead of the hardcoded upstream org | Generic to any fork — upstream candidate; tracked in [#2](https://github.com/tuna-os/hive/issues/2) and [#10](https://github.com/tuna-os/hive/issues/10) |
| `1433a16` | Run CI on GitHub-hosted runners instead of upstream's self-hosted fleet | Fork-specific: upstream's runner labels do not exist here |
| `2f6421c` | Point the PR Verifier at the `pr-verifier` image's current org (#29) | Generic — upstream candidate |
| `3b4fc73`, `01f9ef8` | ACMM evaluation: report "GitHub did not answer" as unknown, not as a missing file (#32) | Generic — upstream candidate |
| `77b0c5d` | Specify custom Tuna OS agent personas (#1) | Fork-specific: fleet policy |
| `5a53947` | Document fork scope, delta, and upstream routing | Fork-specific: this fork's policy |
| `14aa61a` | Add the Tuna OS observability assessment and stack guidelines | Fork-specific: fleet operations |
| `710064d`, `03782ee` | Make `needs-human` idempotent and exclude dependency bots from escalation observations | Generic — upstream candidate |
| `aff6235` | Add agent onboarding files at the repository root (#34) | Generic — upstream candidate |
| `f29a791` | Probe a configured ACMM sub-root so `src/`-layout repos are seen (#35) | Generic — upstream candidate |
| `cd58800` | Refresh fork facts after the upstream sync | Fork-specific: this fork's policy |
| `9ee77c9` | Adopt the shared Simplified Technical English check (#36) | Generic — upstream candidate |
| `4664f33`, `654dd71`, `0821731`, `ed726ae` | Add and harden the Muse Code rotation backend (#48, #50, #52, #53) | Generic feature — upstream candidate |
| `e841e7f` | Reject an unsubstituted repository-name prompt placeholder (#54) | Generic — upstream candidate |
| `1a1aef9` | Make `github-copilot` a first-class rotation backend (#14) | Generic feature — upstream candidate |
| `b34178c` | Link the Tuna OS fleet hosts from README and FORK.md (#3, #13) | Fork-specific: deployment documentation |
| `ec8ce36` | Add the hourly read-only `hive-health` probe and self-closing operations issues (#77) | Fork-specific: it watches Tuna OS deployments |
| `4cd9346` | Build the fork-only v6 image for the Talos fleet | Fork-specific: deployment image |
| `9fa426f` | Wait for contributor disconnect cleanup in the lease-restart test (#104) | Generic — upstream candidate |
| `500465f` | Require positive CI evidence before v4 merge gating (#96) | Generic — upstream candidate |
| `3ce31fd` | Protect API-proxy event logs (#75) | Generic security fix — upstream candidate |
| `96da39d` | Resolve dashboard and Discord npm audit CVEs (#130) | Generic security fix — upstream candidate |
| `90da6b0` | Link orphaned docs and document `ttyd-hive.service` (#45) | Generic documentation — upstream candidate |
| `6cb4612` | Cancel superseded CI runs with concurrency groups (#140) | Generic — upstream candidate |
| `bd70e62` | Restrict prose-workflow token permissions (#114) | Generic security hardening — upstream candidate |
| `3a66546` | Document Tuna OS compute-contribution entry points (#79) | Fork-specific: deployment/community operations |
| `1ca0af5` | Hold repo-wide CI failures for explicit escalation (#51) | Generic — upstream candidate |

The table lists commits added locally after the last recorded upstream sync and
the fork-specific commits already identified in the policy record. Verify each
generic candidate against upstream before the next sync; do not silently grow
the fork delta.

Regenerate the candidate list after every upstream sync with:

```sh
git log --oneline --no-merges hivecommons/v4..v4
```

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
| A bug or feature idea in Hive itself | Upstream: [`hivecommons/hive` issues](https://github.com/hivecommons/hive/issues) |
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
the live hosts and the health check that shows each one is up.

## Ownership

The fork is maintained by the Tuna OS organization. Decisions about the fork —
whether to sync, what delta to carry, whether to keep the fork at all — are made
here. Decisions about Hive itself are made upstream.
