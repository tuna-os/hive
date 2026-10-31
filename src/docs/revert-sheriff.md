# Revert sheriff (tuna-os pilot)

The revert sheriff proposes reverting the merge that broke a required check on
a repository's default branch. It is a `tuna-os/hive` pilot
([tuna-os/tunaOS#4072](https://github.com/tuna-os/tunaOS/issues/4072),
`docs/SDLC-ROADMAP.md` HIVE-8 / TUNA-4). It is off unless a repository is
listed.

```yaml
revert_sheriff:
  repos: [tuna-os/tunaos]   # opt-in; no repos means off
  mode: shadow              # shadow (default) or propose
  hold: true                # label revert PRs `hold` (default true)
  max_per_day: 2            # per repo; -1 means no cap
  lookback: 10              # default-branch commits read, at most 30
  confirm_timeout: 3h       # wait for the confirming re-run
```

## What it does

It runs every 10 minutes, right after the red-main sweep, and only for listed
repositories whose default branch that sweep saw with a required check red.
For each red check:

1. **Find the culprit.** It follows first parents from the tip and reads the
   check's latest run on each commit. The culprit is the oldest red commit
   before the first green one. Commits whose run skipped itself (path filters)
   are passed over. A commit with no completed result (none, pending,
   cancelled) makes the break unattributable. The sheriff waits for the tip to
   move and tries again.
2. **Map it to a PR.** The culprit must be the merge commit of a merged pull
   request; otherwise nothing happens.
3. **Rule out a flake.** It re-runs the failing GitHub Actions job on the
   culprit once. A green re-run ends the case (`flaky`). A check that is not an
   Actions job cannot be re-run, and the case ends (`gave-up`). If the re-run
   does not finish within `confirm_timeout`, the case also ends (`gave-up`).
4. **Gate.** A confirmed culprit is not reverted when:
   - a sheriff revert for it is already open;
   - it is itself a revert;
   - it touches `auto_merge.human_merge_paths` for the repository;
   - the daily cap is reached.
5. **Act.**
   - **shadow:** it logs `would-revert`, and the confirming re-run is its only
     write.
   - **propose:** it asks GitHub to revert the PR (the `revertPullRequest`
     mutation, the same operation as the **Revert** button) and labels the
     revert `hive/revert` and `fixes-main`, so the red-main breaker ranks it as
     a fix, plus `hold` when `hold` is true. It also comments on the culprit
     with a link.

Both the confirming rerun and the held revert proposal consult the shared
writeback gate under `ci_rerun`, using the culprit PR's author, labels and
creation time. Operator-paused or writeback-off repositories stay untouched.
For foreign PRs, the normal ACMM and review opt-in rules still apply; listing
a repository under `revert_sheriff.repos` does not bypass them. The gate is
checked again before proposing a revert, so disabling writes while a rerun is
in flight prevents the proposal and its follow-up labels/comment.

Every decision is written to the audit log as `revert-sheriff`, with
`action=` one of:

- `unattributable`, `no-pr`
- `rerun`, `flaky`, `gave-up`
- `refused`, `would-revert`, `reverted`, `failed`

A case ends when the check goes green on the tip.

## Limits

- Only check runs are read, not commit statuses.
- State is in memory. After a restart, a still-red check is found again and
  its job re-run again, which only delays a revert.
- GitHub authors the revert commit, so it has no `Signed-off-by` trailer.
  Repositories that require DCO need a person to sign off or merge it.
