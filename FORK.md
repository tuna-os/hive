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

Measured on 2026-09-24 with the repository's own compare endpoint, the fork
is **37 commits ahead** of `hivecommons:v4` (fork-only commits plus the sync
merges that carry them) and **1273 commits behind** it. The last upstream sync
is still #30 from 2026-09-05. Every fork-only commit since the fork was
created, oldest first:

| Commit | Change | Disposition |
| --- | --- | --- |
| `4955b9f` | Publish images to `ghcr.io/${{ github.repository_owner }}` instead of the hardcoded upstream org | Generic to any fork — should be upstreamed, tracked in [#2](https://github.com/tuna-os/hive/issues/2) and [#10](https://github.com/tuna-os/hive/issues/10) |
| `1433a16` | Run CI on GitHub-hosted runners instead of upstream's self-hosted fleet | Fork-specific: upstream's runner labels do not exist here |
| `2f6421c` | Point the PR Verifier at the `pr-verifier` image's current org (#29) | Generic — upstream candidate |
| `3b4fc73`, `01f9ef8` | ACMM evaluation: report "GitHub did not answer" as unknown, not as a missing file (#32) | Generic — upstream candidate |
| `aff6235` | Agent onboarding files at the repository root (`AGENTS.md`, `CLAUDE.md`, editor and tool entrypoints) (#34) | Generic — upstream candidate |
| `f29a791` | `governor.acmm.repo_roots`: probe a configured sub-root so `src/`-layout repos are seen (#35) | Generic — upstream candidate |
| `9ee77c9` | Adopt the org-wide Simplified Technical English check (`ste.yml`, `.ste-budget`, `release-lines.yml`) (#36) | Fork-specific: wires this repository into Tuna OS shared CI |
| `4664f33` | Add the Muse Code (`muse`) contributor backend — 14 files, ~400 lines (#48) | Generic — upstream candidate. **A new Hive feature developed here**, which the policy above says not to do; until it is upstreamed it is the second-largest patch the fork carries |
| `654dd71` | Document that muse's model catalog is caller-dependent (#50) | Generic — travels with #48 |
| `0821731` | Launch muse through the shared-`$HOME` umask 007 wrapper (#52) | Generic — travels with #48 |
| `ed726ae` | Give muse a launch contract so it does not start bare and hang (#53) | Generic — travels with #48 |
| `e841e7f` | Refuse an unsubstituted prompt placeholder as a repository name (#54) | Generic — upstream candidate, independent of muse |
| `1a1aef9` | Make `github-copilot` a first-class rotation backend: prober, token fallbacks, pack-rung validation — 11 files, ~600 lines (#14) | Generic — upstream candidate. **A new Hive feature developed here**; the largest patch the fork carries |
| `b34178c` | Link the Tuna OS fleet hosts from README and this page (#13) | Fork-specific |

Regenerate this table with `git log --oneline --no-merges hivecommons/v4..v4`
(after `git remote add hivecommons https://github.com/hivecommons/hive` and a
fetch). Two things stand out in the current table:

- **Eleven of the fourteen rows are generic.** None of them has an upstream pull
  request recorded against it. Until they are sent upstream, each one is a
  patch that has to survive every sync — and the two largest (#14, #48) are
  feature work, which is exactly what this page says the fork is not for. The
  upstreaming of the delta is tracked in [#2](https://github.com/tuna-os/hive/issues/2)
  and, for the publisher genericization, [#10](https://github.com/tuna-os/hive/issues/10).
- **The delta has doubled since the last sync.** The 2026-09-05 sync carried
  seven fork-only commits; there are now fifteen. A larger delta makes the
  next sync's conflict surface larger, and the next sync is already the
  biggest one the fork has faced (see below).

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
[#18](https://github.com/tuna-os/hive/issues/18). The cost of not deciding
is visible in the numbers: on 2026-09-24, nineteen days after the last sync,
the fork was 1273 commits behind — four times the gap that #30 closed, and
the largest the fork has ever been. There is still no workflow in
`.github/workflows/` that syncs from upstream, no tag, and no release, so
"which upstream commit is the fleet running?" has no recorded answer.

| Date | Behind `hivecommons:v4` | Ahead | Event |
| --- | --- | --- | --- |
| 2026-09-02 | 301 | 2 | First sync (#9) |
| 2026-09-02 | 357 | 2 | Same day, after upstream landed 96 commits |
| 2026-09-05 | 0 | 7 | Wholesale sync (#30) brought the fork current |
| 2026-09-24 | 1273 | 37 | No sync since #30 |

Until a contract lands, treat "how far behind is this fork?" as a question to
answer before relying on any upstream fix being present:

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
