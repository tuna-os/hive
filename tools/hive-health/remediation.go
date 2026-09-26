package main

import "strings"

// Advice is what the issue tells whoever picks it up. Agent steps must stay
// within what a Hive agent can safely do from its session: read, diagnose,
// comment, open a PR. Human steps are the ones that change a running Hive.
type Advice struct {
	Diagnose []string
	Human    []string
}

func adviceFor(kind string, h HiveReport) Advice {
	ns := h.Namespace
	if ns == "" {
		ns = "<namespace>"
	}
	kubectl := "`kubectl -n " + ns
	switch kind {
	case "reachability", "deep-health":
		return Advice{
			Diagnose: []string{
				"Re-run the probe for this Hive only: `cd tools/hive-health && go run . -only " + h.Name + "` (from a checkout of tuna-os/hive).",
				"Check whether the failure is DNS/Cloudflare (`curl -sv " + h.URL + "/api/health`) or the Hive itself (a 5xx or a timeout from the origin).",
				"If another Hive is healthy, compare: a shared failure points at the cluster or Cloudflare, not at this Hive.",
			},
			Human: []string{
				kubectl + " get pods` and " + kubectl + " logs deploy/hive --tail=200` for a crash loop or OOM.",
				"`kubectl get nodes` / `kubectl describe node` for DiskPressure or a NotReady control-plane node.",
			},
		}
	case "ready":
		return Advice{
			Diagnose: []string{"A Hive that stays not-ready past boot usually cannot enumerate GitHub: check the github-auth check and the GitHub API status page."},
			Human:    []string{kubectl + " logs deploy/hive --tail=200 | grep -i enumerat`."},
		}
	case "github-auth":
		return Advice{
			Diagnose: []string{"Quote the github_auth detail from `/api/health/deep` and say which credential path the Hive uses (GitHub App vs PAT)."},
			Human:    []string{"Check the GitHub App installation for the org and the key/secret mounted into the pod (`hive-secrets`)."},
		}
	case "governor", "governor-stalled":
		return Advice{
			Diagnose: []string{
				"List which agents are due and which are paused (the Agents table). If every active agent's kick is old, the governor loop itself is stuck.",
				"Look for a recent config change (packs, cadence edits) that paused lanes unintentionally.",
			},
			Human: []string{kubectl + " logs deploy/hive --since=1h | grep -E 'governor eval|kicking agent'` — evals without kicks mean every due agent is paused or refused."},
		}
	case "needs-login":
		return Advice{
			Diagnose: []string{
				"The agent's CLI is showing a login/credential screen, so every kick is wasted. Identify the backend (CLI column) and whether other agents on the same backend are also affected.",
				"If the Hive rotates backends (hive-rotate), propose moving this agent to a backend with working credentials as a PR to the rotation config, not by changing the running Hive.",
			},
			Human: []string{"Re-authenticate that CLI's credentials (shared auth refresh) or pin the agent to a working backend from the dashboard, then confirm `needsLogin` clears in `/api/status`."},
		}
	case "crash-loop", "blocked", "agent-down":
		return Advice{
			Diagnose: []string{
				"Quote the status evidence and conditions for the agent. Distinguish a crash (process exits) from restarts triggered by rotation or credential refresh.",
				"Search this repo and upstream hivecommons/hive for the error text before proposing a fix.",
			},
			Human: []string{kubectl + " logs deploy/hive --since=2h | grep <agent>` for the restart reason, then fix the cause before resuming; resuming a crash-looping agent just loops again."},
		}
	case "crash-loop-operator":
		return Advice{
			Diagnose: []string{"Restarts attributed to `operator` are usually the rotation CronJob. Check whether the rotation interval is shorter than the agent's work cycle."},
			Human:    []string{"Consider lengthening the rotation interval for this Hive's CronJob if agents never finish a task between rotations."},
		}
	case "provider-blocked", "system-alert":
		return Advice{
			Diagnose: []string{"Provider quota alerts clear when the quota window resets. If it persists across several runs, identify which agents share the exhausted provider."},
			Human:    []string{"Move affected agents to another provider or raise the quota."},
		}
	case "kick-stale", "no-output", "stall", "not-producing", "not-ready", "kick-refused":
		return Advice{
			Diagnose: []string{
				"Compare the agent's last kick with its cadence (Agents table). A stale kick with the governor otherwise healthy means the agent is being skipped: check for a refused kick or a pause.",
				"A recent kick with no output means the agent session is wedged (hung egress call, stuck prompt). Check the Hive's egress/proxy health.",
			},
			Human: []string{kubectl + " exec deploy/hive -- tmux capture-pane -pt hive-<agent> | tail -40` to see what the agent is actually doing."},
		}
	case "hub-heartbeat", "hub-snapshot":
		return Advice{
			Diagnose: []string{"A stale heartbeat or hub snapshot is a connectivity problem between the Hive and the hub; restarting the Hive will not fix it."},
			Human:    []string{"`kubectl -n hive get cronjob hive-activity` and the hub deployment in `hive-hub`."},
		}
	case "github-rate-limit":
		return Advice{
			Diagnose: []string{"When the budget is gone the Hive cannot enumerate issues, so the governor stops kicking. Identify which lanes or sweeps spend the most calls (automerge sweeps, per-repo enumeration across many repos)."},
			Human:    []string{"Reduce the repo list or sweep frequency, or split repos across Hives so one GitHub App installation is not shared by every sweep."},
		}
	case "backlog-spike":
		return Advice{
			Diagnose: []string{"Explain the jump: a burst of new agent-filed issues, a repo added to the Hive, or PRs piling up on `hold` waiting for a human."},
			Human:    []string{"Review the held PRs, or lower the rate of agent-filed issues if the backlog is noise."},
		}
	case "auth-tier":
		return Advice{
			Human: []string{"The configured read-role session expired or was revoked. Log in to " + h.URL + " as the read-role probe account and update the Actions secret (see tools/hive-health/README.md)."},
		}
	}
	return Advice{}
}

// adviceKinds returns the distinct kinds of the non-pass checks, fails first.
func adviceKinds(h HiveReport) []string {
	seen := map[string]bool{}
	var out []string
	for _, lv := range []Level{Fail, Warn} {
		for _, c := range h.ChecksAt(lv) {
			if !seen[c.Kind] {
				seen[c.Kind] = true
				out = append(out, c.Kind)
			}
		}
	}
	return out
}

func kindTitle(kind string) string {
	return strings.ReplaceAll(kind, "-", " ")
}
