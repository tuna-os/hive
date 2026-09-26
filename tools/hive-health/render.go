package main

import (
	"fmt"
	"strings"
	"time"
)

var levelIcon = map[Level]string{Pass: "PASS", Warn: "WARN", Fail: "FAIL", Skip: "skip", Info: "info"}

var levelEmoji = map[Level]string{Pass: "🟢", Warn: "🟡", Fail: "🔴", Skip: "⚪", Info: "ℹ️"}

// TextSummary is the human summary printed to stdout.
func TextSummary(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hive health %s — overall %s\n", r.GeneratedAt.UTC().Format(time.RFC3339), levelIcon[r.Level])
	for _, c := range r.Hub {
		fmt.Fprintf(&b, "  [%s] hub: %s\n", levelIcon[c.Level], c.Summary)
	}
	for _, h := range r.Hives {
		fmt.Fprintf(&b, "\n== %s (%s) — %s, tier %s", h.Name, h.URL, levelIcon[h.Level], h.AuthTier)
		if h.HiveID != "" {
			fmt.Fprintf(&b, ", id %s", h.HiveID)
		}
		b.WriteString("\n")
		for _, lv := range []Level{Fail, Warn, Pass, Info} {
			for _, c := range h.ChecksAt(lv) {
				fmt.Fprintf(&b, "  [%s] %s: %s\n", levelIcon[c.Level], c.ID, c.Summary)
				for _, e := range c.Evidence {
					fmt.Fprintf(&b, "         %s\n", e)
				}
			}
		}
		if len(h.Agents) > 0 {
			b.WriteString("  agents:\n")
			for _, a := range h.Agents {
				fmt.Fprintf(&b, "    %-14s %-4s %s\n", a.Name, levelIcon[a.Level], a.Reason)
			}
		}
		if len(h.RecentOutput) > 0 {
			b.WriteString("  recent output:\n")
			for i, o := range h.RecentOutput {
				if i >= 5 {
					break
				}
				fmt.Fprintf(&b, "    %s %-12s %s\n", o.At.UTC().Format("01-02 15:04"), orDash(o.Agent), truncate(o.Text, 110))
			}
		}
	}
	return b.String()
}

// MarkdownSummary is the GitHub job summary.
func MarkdownSummary(r Report, runURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Hive health — %s %s\n\n", levelEmoji[r.Level], strings.ToUpper(string(r.Level)))
	fmt.Fprintf(&b, "Probed %s. ", r.GeneratedAt.UTC().Format("2006-01-02 15:04 UTC"))
	b.WriteString("Public endpoints only unless a Hive's tier says `session`.\n\n")
	b.WriteString("| Hive | Verdict | Tier | Failing | Warnings |\n|---|---|---|---|---|\n")
	for _, h := range r.Hives {
		fmt.Fprintf(&b, "| [%s](%s) | %s %s | %s | %s | %s |\n", h.Name, h.URL, levelEmoji[h.Level], h.Level, h.AuthTier,
			checkIDs(h.ChecksAt(Fail)), checkIDs(h.ChecksAt(Warn)))
	}
	for _, c := range r.Hub {
		fmt.Fprintf(&b, "\n%s hub: %s\n", levelEmoji[c.Level], mdEscape(c.Summary))
	}
	for _, h := range r.Hives {
		fmt.Fprintf(&b, "\n### %s %s\n\n", levelEmoji[h.Level], h.Name)
		writeChecks(&b, h)
		b.WriteString("\n<details><summary>Agents</summary>\n\n")
		writeAgents(&b, h)
		b.WriteString("\n</details>\n")
	}
	if runURL != "" {
		fmt.Fprintf(&b, "\nFull JSON report: artifact `hive-health-report` on [this run](%s).\n", runURL)
	}
	return b.String()
}

func writeChecks(b *strings.Builder, h HiveReport) {
	b.WriteString("| | Check | Result |\n|---|---|---|\n")
	for _, lv := range []Level{Fail, Warn, Pass, Info} {
		for _, c := range h.ChecksAt(lv) {
			s := mdEscape(c.Summary)
			if len(c.Evidence) > 0 {
				ev := make([]string, len(c.Evidence))
				for i, e := range c.Evidence {
					ev[i] = mdEscape(e)
				}
				s += "<br><sub>" + strings.Join(ev, "<br>") + "</sub>"
			}
			fmt.Fprintf(b, "| %s | `%s` | %s |\n", levelEmoji[c.Level], c.ID, s)
		}
	}
}

func writeAgents(b *strings.Builder, h HiveReport) {
	b.WriteString("| Agent | | State | CLI | Cadence | Last kick | Last output | Why |\n|---|---|---|---|---|---|---|---|\n")
	for _, a := range h.Agents {
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n", a.Name, levelEmoji[a.Level], orDash(a.State),
			orDash(a.CLI), orDash(a.Cadence), ageOrDash(a.LastKickAge), ageOrDash(a.LastOutputAge), mdEscape(a.Reason))
	}
}

// IssueMarker is the hidden de-duplication key in a health issue body.
func IssueMarker(name string) string { return "<!-- hive-health:target=" + name + " -->" }

func fingerprintMarker(fp string) string { return "<!-- hive-health:fingerprint=" + fp + " -->" }

// IssueTitle is stable per Hive so the issue is easy to find. The
// "[operations]" prefix routes it to the operations lane (pkg/classify
// title-prefix routing).
func IssueTitle(h HiveReport) string {
	return fmt.Sprintf("[operations] Hive health: %s (%s) is failing its health probe", h.Name, hostOf(h.URL))
}

// IssueBody renders the body of the per-Hive health issue.
func IssueBody(h HiveReport, generated time.Time, runURL string) string {
	var b strings.Builder
	b.WriteString(IssueMarker(h.Name) + "\n" + fingerprintMarker(h.Fingerprint) + "\n")
	fmt.Fprintf(&b, "**%s %s** — probed %s", levelEmoji[h.Level], strings.ToUpper(string(h.Level)), generated.UTC().Format("2006-01-02 15:04 UTC"))
	if runURL != "" {
		fmt.Fprintf(&b, " by [this workflow run](%s)", runURL)
	}
	b.WriteString(".\n\n")
	fmt.Fprintf(&b, "Hive: %s", h.URL)
	if h.HiveID != "" {
		fmt.Fprintf(&b, " · id `%s`", h.HiveID)
	}
	if h.ServedSHA != "" {
		fmt.Fprintf(&b, " · build `%s`", h.ServedSHA)
	}
	fmt.Fprintf(&b, " · evidence tier `%s`\n\n", h.AuthTier)
	b.WriteString("> Maintained by `.github/workflows/hive-health.yml`. The body is rewritten on every hourly run; " +
		"a comment is added only when the set of failing checks changes. The issue closes itself when the Hive stops failing. " +
		"Comment here rather than editing the body.\n\n")

	b.WriteString("## What is failing\n\n")
	writeChecks(&b, HiveReport{Checks: append(h.ChecksAt(Fail), h.ChecksAt(Warn)...)})

	b.WriteString("\n## Agents\n\n")
	writeAgents(&b, h)
	b.WriteString("\nAgents marked ⚪ are deliberately off (operator pause, disabled, or a cadence of `paused`) and are not evaluated.\n")

	if h.Queue != nil {
		q := h.Queue
		fmt.Fprintf(&b, "\n## Queue\n\nGovernor mode `%s` · %d actionable issues · %d PRs · %d on hold", orDash(q.Mode), q.Actionable, q.PRs, q.Hold)
		if q.HoldPRs != nil {
			fmt.Fprintf(&b, " (%d held PRs)", *q.HoldPRs)
		}
		b.WriteString("\n")
	}

	if len(h.RecentOutput) > 0 {
		b.WriteString("\n## Recent output readable via the API\n\n| When (UTC) | Agent | Source | Text |\n|---|---|---|---|\n")
		for _, o := range h.RecentOutput {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", o.At.UTC().Format("01-02 15:04"), orDash(o.Agent), o.Source, mdEscape(truncate(o.Text, 200)))
		}
	}

	b.WriteString("\n## For the Hive agent picking this up\n\n")
	b.WriteString("You are diagnosing a Hive from the outside. Stay inside these limits:\n\n" +
		"- **Diagnose, don't operate.** Do not restart pods, pause/resume agents, edit Hive config, or rotate credentials from an agent session. The probe is read-only by design and so is this lane.\n" +
		"- **Propose, then stop.** Put your diagnosis in a comment here. If a code or manifest change fixes it, open a PR (this repo, or the deployment manifests in `hanthor/dotfiles` under `talos-k8s/hive/`) and link it; a human reviews and applies it.\n" +
		"- **Evidence first.** Quote the check IDs and evidence above; say what you could not verify (the public tier cannot see pane contents or login state unless the tier is `session`).\n" +
		"- **Redaction.** This repository is public. Never paste tokens, cookies, or raw terminal output into the issue.\n\n")
	for _, kind := range adviceKinds(h) {
		a := adviceFor(kind, h)
		if len(a.Diagnose) == 0 && len(a.Human) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n", kindTitle(kind))
		for _, s := range a.Diagnose {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range a.Human {
			fmt.Fprintf(&b, "- 👤 needs a human: %s\n", s)
		}
		b.WriteString("\n")
	}
	b.WriteString("Re-check any time: `cd tools/hive-health && go run . -only " + h.Name + "` from a checkout of this repository.\n")
	return b.String()
}

func checkIDs(cs []Check) string {
	if len(cs) == 0 {
		return "—"
	}
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = "`" + c.ID + "`"
	}
	return strings.Join(ids, ", ")
}

func hostOf(u string) string {
	return Target{URL: u}.Host()
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func ageOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s + " ago"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
