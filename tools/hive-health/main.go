// Command hive-health probes the Tuna OS Hives through their web API and
// classifies what it sees into pass / warn / fail checks.
//
//	hive-health [probe flags]        probe every Hive in the targets file
//	hive-health plan-issues [flags]  turn a report into issue-sync actions
//
// It only ever issues HTTP GETs. It never needs a credential: the optional
// read-role session (see README.md) only enriches the report.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv, NewHTTPGetter(20*time.Second), time.Now))
}

type multiFlag map[string]string

func (m multiFlag) String() string { return "" }
func (m multiFlag) Set(v string) error {
	k, p, ok := strings.Cut(v, "=")
	if !ok || k == "" || p == "" {
		return errors.New("want name=path")
	}
	m[k] = p
	return nil
}

func run(args []string, stdout, stderr io.Writer, env func(string) string, get Getter, now func() time.Time) int {
	if len(args) > 0 && args[0] == "plan-issues" {
		return runPlan(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("hive-health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	targets := fs.String("targets", defaultTargets(), "targets JSON file")
	only := fs.String("only", "", "comma-separated hive names to probe (default: all)")
	jsonOut := fs.String("json", "", "write the JSON report here")
	mdOut := fs.String("markdown", "", "write a Markdown summary here (e.g. $GITHUB_STEP_SUMMARY)")
	prevPath := fs.String("previous", "", "previous JSON report, for backlog spike detection (missing file is fine)")
	runURL := fs.String("run-url", "", "link to this CI run, for the Markdown summary")
	failOnFail := fs.Bool("fail-on-unhealthy", false, "exit 2 when any Hive fails")
	noHub := fs.Bool("no-hub", false, "skip the hub snapshot")
	statusFiles := multiFlag{}
	fs.Var(statusFiles, "status-file", "name=path: use a locally fetched /api/status JSON for a hive (operator use; repeatable)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	cfg, err := LoadConfig(*targets)
	if err != nil {
		fmt.Fprintln(stderr, "hive-health:", err)
		return 1
	}
	if *only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(*only, ",") {
			want[strings.TrimSpace(n)] = true
		}
		var sel []Target
		for _, h := range cfg.Hives {
			if want[h.Name] {
				sel = append(sel, h)
				delete(want, h.Name)
			}
		}
		if len(want) > 0 {
			fmt.Fprintln(stderr, "hive-health: unknown hive(s) in -only:", keys(want))
			return 1
		}
		cfg.Hives = sel
	}
	prev := loadPrevious(*prevPath, stderr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	col := &Collector{Get: get, Env: env, StatusFiles: statusFiles}
	hubURL := cfg.HubSnapshotURL
	if *noHub {
		hubURL = ""
	}
	hub, hubFetch := col.FetchHub(ctx, hubURL)

	start := now()
	report := Report{GeneratedAt: start.UTC()}
	report.Hub = HubChecks(hub, hubFetch, cfg.Thresholds, start)
	lv := Pass
	for _, c := range report.Hub {
		lv = worst(lv, c.Level)
	}
	for _, t := range cfg.Hives {
		raw := col.Collect(ctx, t, hub)
		var p *HiveReport
		if prev != nil {
			for i := range prev.Hives {
				if prev.Hives[i].Name == t.Name {
					p = &prev.Hives[i]
				}
			}
		}
		h := Evaluate(raw, p, cfg.Thresholds, now())
		report.Hives = append(report.Hives, h)
		lv = worst(lv, h.Level)
	}
	report.Level = lv

	// Last line of defence: no configured session value may appear anywhere
	// in what is written out.
	var secrets []string
	for _, t := range cfg.Hives {
		if t.SessionEnv != "" {
			if v := strings.TrimSpace(env(t.SessionEnv)); v != "" {
				secrets = append(secrets, v)
			}
		}
	}
	scrub := func(s string) string {
		for _, v := range secrets {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
		return s
	}

	js, _ := json.MarshalIndent(report, "", "  ")
	if *jsonOut != "" {
		if err := os.WriteFile(*jsonOut, []byte(scrub(string(js))+"\n"), 0o644); err != nil {
			fmt.Fprintln(stderr, "hive-health:", err)
			return 1
		}
	}
	if *mdOut != "" {
		f, err := os.OpenFile(*mdOut, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(stderr, "hive-health:", err)
			return 1
		}
		_, _ = f.WriteString(scrub(MarkdownSummary(report, *runURL)))
		f.Close()
	}
	fmt.Fprint(stdout, scrub(TextSummary(report)))
	if *failOnFail && report.Level == Fail {
		return 2
	}
	return 0
}

func runPlan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plan-issues", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reportPath := fs.String("report", "", "JSON report from a probe run")
	openPath := fs.String("open-issues", "", "JSON from `gh issue list --label hive-health --state open --json number,title,body`")
	outDir := fs.String("out", "", "directory to write plan.json and body files into")
	runURL := fs.String("run-url", "", "link to the CI run")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *reportPath == "" || *openPath == "" || *outDir == "" {
		fmt.Fprintln(stderr, "plan-issues: -report, -open-issues and -out are required")
		return 1
	}
	var r Report
	if err := readJSON(*reportPath, &r); err != nil {
		fmt.Fprintln(stderr, "plan-issues:", err)
		return 1
	}
	var open []OpenIssue
	if err := readJSON(*openPath, &open); err != nil {
		fmt.Fprintln(stderr, "plan-issues:", err)
		return 1
	}
	actions := PlanIssues(r, open, *runURL)
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(stderr, "plan-issues:", err)
		return 1
	}
	// Bodies go to files so the shell step never interpolates them.
	type fileAction struct {
		Op          string `json:"op"`
		Target      string `json:"target"`
		Number      int    `json:"number,omitempty"`
		Title       string `json:"title,omitempty"`
		Labels      string `json:"labels,omitempty"`
		BodyFile    string `json:"body_file,omitempty"`
		CommentFile string `json:"comment_file,omitempty"`
	}
	var out []fileAction
	for i, a := range actions {
		fa := fileAction{Op: a.Op, Target: a.Target, Number: a.Number, Title: a.Title, Labels: strings.Join(a.Labels, ",")}
		if a.Body != "" {
			fa.BodyFile = filepath.Join(*outDir, fmt.Sprintf("%02d-%s-body.md", i, a.Target))
			if err := os.WriteFile(fa.BodyFile, []byte(a.Body), 0o644); err != nil {
				fmt.Fprintln(stderr, "plan-issues:", err)
				return 1
			}
		}
		if a.Comment != "" {
			fa.CommentFile = filepath.Join(*outDir, fmt.Sprintf("%02d-%s-comment.md", i, a.Target))
			if err := os.WriteFile(fa.CommentFile, []byte(a.Comment), 0o644); err != nil {
				fmt.Fprintln(stderr, "plan-issues:", err)
				return 1
			}
		}
		out = append(out, fa)
		fmt.Fprintf(stdout, "%s %s #%d\n", a.Op, a.Target, a.Number)
	}
	if out == nil {
		out = []fileAction{}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "plan.json"), b, 0o644); err != nil {
		fmt.Fprintln(stderr, "plan-issues:", err)
		return 1
	}
	return 0
}

func readJSON(path string, into any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

func loadPrevious(path string, stderr io.Writer) *Report {
	if path == "" {
		return nil
	}
	var r Report
	if err := readJSON(path, &r); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stderr, "hive-health: ignoring previous report:", err)
		}
		return nil
	}
	return &r
}

// defaultTargets finds targets.json next to the source when run with `go run`
// from the repo root or from this directory.
func defaultTargets() string {
	for _, p := range []string{"targets.json", filepath.Join("tools", "hive-health", "targets.json")} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "targets.json"
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
