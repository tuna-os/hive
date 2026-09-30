package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// selfHealingAlerts are dashboard system alerts that clear on their own (a
// provider quota window resets); they warn instead of fail.
var selfHealingAlerts = map[string]bool{
	"provider-budget-exceeded": true,
}

const noKickMessage = "no kick message recorded"

// Evaluate classifies one Hive. It is pure: everything it needs is in raw,
// prev (the same Hive in the previous run's report, may be nil) and now.
func Evaluate(raw Raw, prev *HiveReport, th Thresholds, now time.Time) HiveReport {
	h := evaluate(raw, prev, th, now)
	h.finalize()
	return h
}

func evaluate(raw Raw, prev *HiveReport, th Thresholds, now time.Time) HiveReport {
	t := raw.Target
	h := HiveReport{Name: t.Name, URL: t.URL, Namespace: t.Namespace, AuthTier: "public"}

	// 1. Reachability.
	if raw.Health.OK() && strings.Contains(string(raw.Health.Body), "ok") {
		h.add(Check{ID: "reachability", Kind: "reachability", Level: Pass,
			Summary: fmt.Sprintf("GET /api/health answered 200 in %s", raw.Health.Latency.Round(time.Millisecond))})
	} else {
		h.add(Check{ID: "reachability", Kind: "reachability", Level: Fail,
			Summary:  "the Hive's public health endpoint did not answer 200",
			Evidence: []string{raw.Health.Describe()}})
	}

	// 2. Deep health is the backbone of the public tier.
	d := raw.Deep
	if d == nil {
		h.add(Check{ID: "deep-health", Kind: "deep-health", Level: Fail,
			Summary:  "GET /api/health/deep is unavailable, so agent and governor state cannot be read",
			Evidence: []string{raw.DeepFetch.Describe()}})
		return h
	}
	var cfg DeepConfig
	if d.check("config", &cfg) {
		h.HiveID = cfg.HiveID
	}
	switch d.Status {
	case "ok":
		h.add(Check{ID: "deep-health", Kind: "deep-health", Level: Pass, Summary: "deep health reports ok"})
	case "critical":
		h.add(Check{ID: "deep-health", Kind: "deep-health", Level: Fail,
			Summary: fmt.Sprintf("deep health reports critical (%d failing checks)", d.Fails)})
	default:
		h.add(Check{ID: "deep-health", Kind: "deep-health", Level: Warn,
			Summary: fmt.Sprintf("deep health reports %q (%d failing checks)", d.Status, d.Fails)})
	}
	if raw.Contribute != nil {
		h.ServedSHA = raw.Contribute.ServedSHA
	}

	var ready DeepCheck
	if d.check("ready", &ready) && ready.Status == "fail" {
		h.add(Check{ID: "ready", Kind: "ready", Level: Fail,
			Summary: "the Hive has no status yet (still booting, or the first GitHub enumeration is failing)", Evidence: nonEmpty(ready.Detail)})
	}
	var gha DeepCheck
	if d.check("github_auth", &gha) {
		switch {
		case gha.Status == "fail":
			h.add(Check{ID: "github-auth", Kind: "github-auth", Level: Fail,
				Summary: "the Hive cannot authenticate to GitHub", Evidence: nonEmpty(redact(gha.Detail, 300))})
		case strings.Contains(gha.Detail, "missing"):
			h.add(Check{ID: "github-auth", Kind: "github-auth", Level: Warn,
				Summary: "GitHub auth works but a capability is missing", Evidence: nonEmpty(redact(gha.Detail, 300))})
		}
	}

	// 3. Governor.
	var gov DeepGovernor
	hasGov := d.check("governor", &gov) && gov.Mode != ""
	if hasGov {
		h.add(Check{ID: "governor", Kind: "governor", Level: Pass,
			Summary: fmt.Sprintf("governor running in %s mode", gov.Mode)})
	} else {
		h.add(Check{ID: "governor", Kind: "governor", Level: Fail,
			Summary: "deep health has no governor state: the governor is not running"})
	}

	// 4. Auth tier.
	st := raw.Status
	switch raw.StatusSource {
	case "session":
		if st != nil && len(st.Agents) > 0 {
			h.AuthTier = "session"
		} else {
			h.AuthTier = "session-rejected"
			ev := []string{}
			if raw.StatusFetch != nil {
				ev = append(ev, raw.StatusFetch.Describe())
			}
			h.add(Check{ID: "auth-tier", Kind: "auth-tier", Level: Warn,
				Summary:  "a dashboard session is configured for this Hive but /api/status did not accept it (expired?); fell back to public checks",
				Evidence: ev})
			st = nil
		}
	case "status-file":
		if st != nil && len(st.Agents) > 0 {
			h.AuthTier = "status-file"
		} else {
			st = nil
		}
	}

	// 5. Hub snapshot: recent output per agent.
	lastOut, outputs, hubNote := hubOutput(raw.Hub, h.HiveID)
	if raw.Activity != nil {
		for _, a := range raw.Activity.Activity {
			if a.Task == "" {
				continue
			}
			outputs = append(outputs, OutputItem{At: a.Timestamp, Source: "contributor-activity",
				Text: redact(fmt.Sprintf("%s %s: %s", a.Username, a.Action, a.Task), 280)})
		}
	}
	sort.SliceStable(outputs, func(i, j int) bool { return outputs[i].At.After(outputs[j].At) })
	if len(outputs) > 12 {
		outputs = outputs[:12]
	}
	h.RecentOutput = outputs

	// 6. Agents.
	deepAgents := d.Agents()
	statusAgents := map[string]StatusAgent{}
	cadences := map[string]CadenceRow{}
	if st != nil {
		for _, a := range st.Agents {
			statusAgents[a.Name] = a
		}
		for _, c := range st.CadenceMatrix {
			cadences[c.Agent] = c
		}
	}
	names := map[string]bool{}
	for n := range deepAgents {
		names[n] = true
	}
	for n := range statusAgents {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	kickedSinceStart := 0
	for _, da := range deepAgents {
		if da.State == "running" && !da.Paused && da.LastPromptLen > 0 {
			kickedSinceStart++
		}
	}

	pausedAgents := map[string]bool{}
	var newestActiveKick *time.Time
	activeCount := 0
	for _, name := range sorted {
		da, hasDeep := deepAgents[name]
		sa, hasStatus := statusAgents[name]
		ag, checks := evalAgent(name, da, hasDeep, sa, hasStatus, cadences[name], kickedSinceStart, lastOut[name], gov.Mode, th, now)
		if ag.PausedBy != "" {
			pausedAgents[name] = true
		} else {
			activeCount++
			if ag.LastKick != nil && (newestActiveKick == nil || ag.LastKick.After(*newestActiveKick)) {
				k := *ag.LastKick
				newestActiveKick = &k
			}
		}
		h.Agents = append(h.Agents, ag)
		for _, c := range checks {
			h.add(c)
		}
	}

	// 7. Is the governor actually kicking anyone?
	if hasGov {
		switch {
		case activeCount == 0:
			h.add(Check{ID: "governor-kicks", Kind: "governor-stalled", Level: Warn,
				Summary: "every agent is paused or cadence-paused; the Hive is doing no work"})
		case newestActiveKick == nil:
			h.add(Check{ID: "governor-kicks", Kind: "governor-stalled", Level: Fail,
				Summary: "no active agent has ever been kicked"})
		default:
			age := now.Sub(*newestActiveKick)
			lv := Pass
			if age > th.GovernorFail.Duration {
				lv = Fail
			} else if age > th.GovernorWarn.Duration {
				lv = Warn
			}
			h.add(Check{ID: "governor-kicks", Kind: "governor-stalled", Level: lv,
				Summary: fmt.Sprintf("newest kick to an active agent was %s ago", humanAge(age))})
		}
	}

	// 8. Deep-health stall detection, minus agents that are deliberately off
	// (a cadence-paused agent has an old kick and an empty buffer by design).
	var stall DeepCheck
	if d.check("stall_detection", &stall) && stall.Status == "warn" {
		var real []string
		for _, a := range stall.Agents {
			if !pausedAgents[a] {
				real = append(real, a)
			}
		}
		if len(real) > 0 {
			h.add(Check{ID: "stall-detection", Kind: "stall", Level: Warn,
				Summary: "agents kicked but produced no output for 30+ minutes: " + strings.Join(real, ", ")})
		}
	}
	var hb DeepHeartbeat
	if d.check("hub_heartbeat", &hb) && hb.Status != "pass" && hb.Status != "" {
		h.add(Check{ID: "hub-heartbeat", Kind: "hub-heartbeat", Level: Warn,
			Summary: "the hub has not accepted a heartbeat recently", Evidence: nonEmpty(redact(hb.Detail, 200))})
	}
	var tok DeepCheck
	if d.check("tokens", &tok) && tok.Status == "warn" {
		h.add(Check{ID: "tokens", Kind: "tokens", Level: Warn, Summary: "zero tokens consumed: agents may not be working"})
	}

	// 9. Queue / backlog.
	var q DeepQueue
	d.check("queue", &q)
	qs := &QueueSnapshot{Mode: gov.Mode, Issues: gov.Issues, PRs: gov.PRs, Hold: gov.Hold, Actionable: q.Actionable}
	if st != nil {
		hp := st.Hold.PRs
		qs.HoldPRs = &hp
	}
	h.Queue = qs
	h.add(backlogCheck(qs, prev, th))

	// 10. System alerts (authenticated tier).
	if st != nil {
		if core := st.GHRateLimits.Core; core.Limit > 0 && core.Remaining*20 < core.Limit {
			h.add(Check{ID: "github-rate-limit", Kind: "github-rate-limit", Level: Warn,
				Summary: fmt.Sprintf("GitHub API budget nearly exhausted: %d of %d left, resets %s", core.Remaining, core.Limit, orUnknown(core.Reset))})
		}
		for _, a := range st.SystemAlerts {
			if c, ok := alertCheck(a, pausedAgents); ok {
				h.add(c)
			}
		}
	}

	// 11. Is the Hive producing anything at all?
	if c, ok := productionCheck(lastOut, hubNote, raw.Hub, th, now); ok {
		h.add(c)
	}
	return h
}

func evalAgent(name string, da DeepAgent, hasDeep bool, sa StatusAgent, hasStatus bool, cad CadenceRow,
	kickedSinceStart int, lastOut *time.Time, mode string, th Thresholds, now time.Time) (AgentReport, []Check) {
	ag := AgentReport{Name: name, State: da.State}
	if hasStatus {
		if ag.State == "" {
			ag.State = sa.State
		}
		ag.CLI = sa.CLI
		ag.Cadence = sa.Cadence
		ag.NeedsLogin = sa.NeedsLogin
		ag.StatusEvidence = redact(sa.StatusEvidence, 200)
	}
	if hasDeep && da.LastKick != "" {
		if k, err := time.Parse(time.RFC3339, da.LastKick); err == nil {
			ag.LastKick = &k
			ag.LastKickAge = humanAge(now.Sub(k))
		}
	}
	if lastOut != nil {
		o := *lastOut
		ag.LastOutput = &o
		ag.LastOutputAge = humanAge(now.Sub(o))
	}

	// Deliberately off?
	switch {
	case da.Paused || (hasStatus && sa.Paused):
		ag.PausedBy, ag.Level, ag.Reason = "pause", Skip, "paused by an operator"
	case da.Disabled:
		ag.PausedBy, ag.Level, ag.Reason = "config", Skip, "disabled in config"
	case hasStatus && (sa.OffByCadence || cadenceIsPaused(sa.Cadence)):
		ag.PausedBy, ag.Level = "cadence", Skip
		ag.Reason = "cadence is `paused` in " + orUnknown(strings.ToLower(mode)) + " mode"
		if allModesPaused(cad) {
			ag.Reason = "cadence is `paused` in every governor mode (deliberately off)"
		}
	case hasStatus && strings.EqualFold(strings.TrimSpace(sa.Cadence), "on demand"):
		ag.PausedBy, ag.Level, ag.Reason = "on-demand", Skip, "on-demand agent (never kicked by the governor)"
	case !hasStatus && hasDeep && da.State == "running" && da.Detail == noKickMessage && kickedSinceStart > 0 &&
		(ag.LastKick == nil || now.Sub(*ag.LastKick) >= th.PresumedPausedAge.Duration):
		ag.PausedBy, ag.Level = "cadence-presumed", Skip
		ag.Reason = fmt.Sprintf("presumed cadence-paused: not kicked since the Hive restarted while %d peers were (confirm with the authenticated tier)", kickedSinceStart)
	}
	if ag.PausedBy != "" {
		return ag, nil
	}

	var checks []Check
	id := func(aspect string) string { return "agent/" + name + "/" + aspect }

	state := ag.State
	if state != "" && state != "running" {
		checks = append(checks, Check{ID: id("state"), Kind: "agent-down", Level: Fail,
			Summary: fmt.Sprintf("%s is %s (not paused, not disabled)", name, state)})
	}

	if hasStatus && sa.NeedsLogin {
		since := conditionSince(sa.Conditions, "Authenticated", "False")
		if since == nil {
			since = conditionSince(sa.Conditions, "Ready", "False")
		}
		lv := Fail
		sum := fmt.Sprintf("%s (%s) is sitting on a login/credential screen", name, orUnknown(sa.CLI))
		if since != nil {
			if now.Sub(*since) < th.LoginGrace.Duration {
				lv = Warn
			}
			sum += " since " + since.UTC().Format(time.RFC3339) + " (" + humanAge(now.Sub(*since)) + ")"
		}
		ev := conditionEvidence(sa.Conditions)
		if sa.WatchdogMode != "" {
			ev = append(ev, "watchdog mode: "+sa.WatchdogMode+" (observe = it reports but does not restart the agent)")
		}
		checks = append(checks, Check{ID: id("needs-login"), Kind: "needs-login", Level: lv, Summary: sum, Evidence: ev})
	}

	if hasStatus && strings.EqualFold(sa.StructuredStatus, "BLOCKED") {
		ev := redact(sa.StatusEvidence, 300)
		low := strings.ToLower(ev)
		c := Check{ID: id("blocked"), Evidence: nonEmpty(ev)}
		switch {
		case strings.Contains(low, "crash-looping") && strings.HasSuffix(low, ": operator"):
			// The watchdog labels these crash-looping, but every restart came
			// from an operator action (the rotation CronJobs restart agents on
			// a schedule). Context, not a verdict.
			c.Kind, c.Level = "crash-loop-operator", Info
			c.Summary = name + " restarted repeatedly by operator actions (rotation), not by crashes"
		case strings.Contains(low, "crash-looping"):
			c.Kind, c.Level = "crash-loop", Fail
			c.Summary = name + " is crash-looping"
		case strings.Contains(low, "inference ("):
			c.Kind, c.Level = "provider-blocked", Warn
			c.Summary = name + " is blocked on its inference provider (quota or outage); usually clears when the window resets"
		default:
			c.Kind, c.Level = "blocked", Fail
			c.Summary = name + " reports BLOCKED"
		}
		checks = append(checks, c)
	}

	if hasStatus && !sa.NeedsLogin {
		for _, cond := range sa.Conditions {
			if cond.Type == "Ready" && cond.Status == "False" && !cond.LastTransitionTime.IsZero() &&
				now.Sub(cond.LastTransitionTime) > th.NotReadyWarn.Duration {
				checks = append(checks, Check{ID: id("not-ready"), Kind: "not-ready", Level: Warn,
					Summary:  fmt.Sprintf("%s has not been Ready for %s", name, humanAge(now.Sub(cond.LastTransitionTime))),
					Evidence: nonEmpty(redact(cond.Reason+": "+cond.Message, 200))})
			}
		}
	}

	if hasDeep && da.Status == "warn" && da.Detail != "" && da.Detail != noKickMessage {
		checks = append(checks, Check{ID: id("deep-warn"), Kind: "kick-refused", Level: Warn,
			Summary: name + ": " + redact(da.Detail, 200)})
	}
	if hasDeep && da.Status == "fail" && state == "running" {
		checks = append(checks, Check{ID: id("deep-fail"), Kind: "agent-down", Level: Fail,
			Summary: name + " fails its deep health check"})
	}

	// Kick freshness vs cadence.
	cadence, known := parseCadence(ag.Cadence)
	warnAt, failAt := th.UnknownCadenceWarn.Duration, th.UnknownCadenceFail.Duration
	basis := "cadence unknown on the public tier"
	if known {
		warnAt = max(time.Duration(float64(cadence)*th.KickWarnFactor)+th.KickSlack.Duration, th.KickWarnFloor.Duration)
		failAt = max(time.Duration(float64(cadence)*th.KickFailFactor)+th.KickSlack.Duration, th.KickFailFloor.Duration)
		basis = "cadence " + ag.Cadence
	}
	if ag.LastKick == nil {
		checks = append(checks, Check{ID: id("kick"), Kind: "kick-stale", Level: Warn,
			Summary: name + " has never been kicked (" + basis + ")"})
	} else {
		age := now.Sub(*ag.LastKick)
		switch {
		case age > failAt:
			checks = append(checks, Check{ID: id("kick"), Kind: "kick-stale", Level: Fail,
				Summary: fmt.Sprintf("%s last kicked %s ago (%s; fail threshold %s)", name, humanAge(age), basis, humanAge(failAt))})
		case age > warnAt:
			checks = append(checks, Check{ID: id("kick"), Kind: "kick-stale", Level: Warn,
				Summary: fmt.Sprintf("%s last kicked %s ago (%s; warn threshold %s)", name, humanAge(age), basis, humanAge(warnAt))})
		}
	}

	// Output freshness vs cadence: only when the hub had output data for this
	// agent at all, and never stricter than OutputWarn.
	if known && ag.LastOutput != nil {
		limit := 12 * cadence
		if limit < th.OutputWarn.Duration {
			limit = th.OutputWarn.Duration
		}
		if age := now.Sub(*ag.LastOutput); age > limit {
			checks = append(checks, Check{ID: id("output"), Kind: "no-output", Level: Warn,
				Summary: fmt.Sprintf("%s is kicked but its newest output is %s old (cadence %s)", name, humanAge(age), ag.Cadence)})
		}
	}

	ag.Level = Pass
	for _, c := range checks {
		if c.Level.rank() > ag.Level.rank() {
			ag.Level, ag.Reason = c.Level, c.Summary
		}
	}
	if ag.Reason == "" {
		ag.Reason = "healthy"
		if ag.LastKick != nil {
			ag.Reason = "kicked " + ag.LastKickAge + " ago (" + basis + ")"
		}
	}
	return ag, checks
}

// backlogCheck reports the queue and flags spikes against the previous run.
func backlogCheck(q *QueueSnapshot, prev *HiveReport, th Thresholds) Check {
	c := Check{ID: "backlog", Kind: "backlog", Level: Info,
		Summary: fmt.Sprintf("queue: %d actionable issues, %d PRs, %d on hold", q.Actionable, q.PRs, q.Hold)}
	if q.HoldPRs != nil {
		c.Summary += fmt.Sprintf(" (%d of the held items are PRs waiting on a human)", *q.HoldPRs)
	}
	if prev == nil || prev.Queue == nil {
		return c
	}
	p := prev.Queue
	var spikes []string
	spike := func(label string, before, after int) {
		delta := after - before
		if delta >= th.SpikeMin && float64(delta) >= th.SpikeRatio*float64(before) {
			spikes = append(spikes, fmt.Sprintf("%s %d -> %d (+%d)", label, before, after, delta))
		}
	}
	spike("on hold", p.Hold, q.Hold)
	spike("actionable issues", p.Actionable, q.Actionable)
	spike("open PRs", p.PRs, q.PRs)
	if p.HoldPRs != nil && q.HoldPRs != nil {
		spike("held PRs", *p.HoldPRs, *q.HoldPRs)
	}
	if len(spikes) > 0 {
		c.ID, c.Kind, c.Level = "backlog-spike", "backlog-spike", Warn
		c.Summary = "backlog spiked since the previous probe run"
		c.Evidence = spikes
	}
	return c
}

// alertCheck maps a dashboard system alert. Watchdog alerts about an agent
// that is deliberately off are dropped: the watchdog's "alive but not
// producing" fires for cadence-paused agents too.
func alertCheck(a SystemAlert, paused map[string]bool) (Check, bool) {
	if agent := watchdogAlertAgent(a.ID); agent != "" && paused[agent] {
		return Check{}, false
	}
	c := Check{ID: "alert/" + a.ID, Kind: "system-alert", Summary: redact(a.Message, 300)}
	switch strings.ToLower(a.Severity) {
	case "error", "critical":
		c.Level = Fail
		if selfHealingAlerts[a.ID] {
			c.Level = Warn
		}
	case "warning", "warn":
		c.Level = Warn
	default:
		c.Level = Info
	}
	return c, true
}

// watchdogAlertAgent extracts the agent from "watchdog-<kind>-<agent>".
func watchdogAlertAgent(id string) string {
	rest, ok := strings.CutPrefix(id, "watchdog-")
	if !ok {
		return ""
	}
	_, agent, ok := strings.Cut(rest, "-")
	if !ok {
		return ""
	}
	return agent
}

// hubOutput extracts per-agent newest output and recent output items for the
// Hive with the given id from the hub snapshot.
func hubOutput(hub *HubSnapshot, hiveID string) (map[string]*time.Time, []OutputItem, string) {
	last := map[string]*time.Time{}
	if hub == nil {
		return last, nil, "hub snapshot unavailable"
	}
	bump := func(agent string, t *time.Time) {
		if t == nil || t.IsZero() || agent == "" {
			return
		}
		if cur := last[agent]; cur == nil || t.After(*cur) {
			v := *t
			last[agent] = &v
		}
	}
	spoke := ""
	for _, s := range hub.Spokes {
		if hiveID != "" && s.HiveID == hiveID {
			spoke = s.Spoke
		}
	}
	var items []OutputItem
	if spoke != "" {
		for _, b := range hub.Beads {
			if b.Spoke != spoke {
				continue
			}
			u := b.Updated
			bump(b.Agent, &u)
			items = append(items, OutputItem{At: b.Updated, Agent: b.Agent, Source: "bead/" + orUnknown(b.Type),
				Text: redact(b.Title, 280)})
		}
	}
	inRegistry := false
	if hub.Registry != nil {
		for _, rh := range hub.Registry.Hives {
			if rh.ID != hiveID || hiveID == "" {
				continue
			}
			inRegistry = true
			for _, ra := range rh.RepoActivity {
				for _, a := range ra.Agents {
					for _, cnt := range []*ActivityCnt{a.Issues, a.PRs, a.Comments, a.Merges, a.Reviews} {
						if cnt != nil && cnt.Count > 0 {
							bump(a.Agent, cnt.NewestAt)
						}
					}
				}
			}
		}
	}
	note := ""
	switch {
	case spoke == "" && !inRegistry:
		note = "Hive not listed in the hub snapshot"
	case len(last) == 0:
		note = "no beads or GitHub activity for this Hive in the hub snapshot window"
	}
	return last, items, note
}

func productionCheck(last map[string]*time.Time, note string, hub *HubSnapshot, th Thresholds, now time.Time) (Check, bool) {
	if hub == nil {
		return Check{}, false
	}
	var newest *time.Time
	for _, t := range last {
		if newest == nil || t.After(*newest) {
			newest = t
		}
	}
	if newest == nil {
		return Check{ID: "production", Kind: "not-producing", Level: Info, Summary: note}, true
	}
	age := now.Sub(*newest)
	c := Check{ID: "production", Kind: "not-producing", Level: Pass,
		Summary: fmt.Sprintf("newest agent output (bead / issue / PR / comment) is %s old", humanAge(age))}
	if age > th.OutputFail.Duration {
		c.Level = Fail
	} else if age > th.OutputWarn.Duration {
		c.Level = Warn
	}
	return c, true
}

// HubChecks evaluates the hub snapshot itself.
func HubChecks(hub *HubSnapshot, f Fetched, th Thresholds, now time.Time) []Check {
	if f.URL == "" {
		return nil
	}
	if hub == nil {
		return []Check{{ID: "hub-snapshot", Kind: "hub-snapshot", Level: Warn,
			Summary: "hub snapshot unavailable; per-agent output data is missing from this run", Evidence: []string{f.Describe()}}}
	}
	age := now.Sub(hub.GeneratedAt)
	c := Check{ID: "hub-snapshot", Kind: "hub-snapshot", Level: Pass,
		Summary: fmt.Sprintf("hub snapshot generated %s ago", humanAge(age))}
	if age > th.HubSnapshotStale.Duration {
		c.Level = Warn
		c.Summary += " (stale: the hive-activity CronJob may not be running)"
	}
	return []Check{c}
}

func parseCadence(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" || cadenceIsPaused(s) {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

func cadenceIsPaused(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "paused", "pause", "off":
		return true
	}
	return false
}

func allModesPaused(c CadenceRow) bool {
	if c.Agent == "" {
		return false
	}
	for _, v := range c.Modes() {
		if !cadenceIsPaused(v) {
			return false
		}
	}
	return true
}

func conditionSince(conds []Condition, typ, status string) *time.Time {
	for _, c := range conds {
		if c.Type == typ && c.Status == status && !c.LastTransitionTime.IsZero() {
			t := c.LastTransitionTime
			return &t
		}
	}
	return nil
}

func conditionEvidence(conds []Condition) []string {
	var out []string
	for _, c := range conds {
		if c.Status == "True" {
			continue
		}
		out = append(out, redact(fmt.Sprintf("%s=%s (%s): %s", c.Type, c.Status, c.Reason, c.Message), 200))
	}
	return out
}

func nonEmpty(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return []string{s}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
