#!/usr/bin/env bash
# Contract tests for bin/issue-classifier.sh.
# Run: bash bin/test_issue_classifier.sh
#
# issue-classifier.sh enriches /var/run/hive-metrics/actionable.json in place
# with complexity_tier, model_recommendation, is_tracker, lane,
# needs_architecture_review and cluster_key, plus top-level clusters and
# classification counts. Those fields decide which model and lane an agent's
# work is routed to, and nothing tested them: a regression in a lane, tracker
# or cluster rule changed routing without failing anything.
#
# Like bin/test_merge_gate.sh this EXECUTES the script. The script hardcodes
# /var/run/hive-metrics/actionable.json and /var/log/kick-agents.log with no
# env override, so the harness runs a COPY with those two paths rewritten to a
# temp dir, and asserts the rewrite landed — a refactor that renames them must
# fail here loudly, not silently run against the real paths. Classification
# rules come from HIVE_PROJECT_YAML, which the script already honours, so the
# rules under test are the fixture's, not the deployed config.
#
# Doctrine: every positive assertion sits next to a control that must NOT
# match, so a classifier that tags everything cannot pass.
# Hermetic: no network, no sleeps, never touches /var/run or /var/log.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${REPO_ROOT}/bin/issue-classifier.sh"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

# A missing harness dependency must be red, not a silent exit 0.
for dep in python3; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    echo "harness-error: $dep is required"
    exit 1
  fi
done
if ! python3 -c 'import yaml' >/dev/null 2>&1; then
  echo "harness-error: python3 PyYAML is required (the script under test imports it)"
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

RUN_DIR="${WORK}/run"                # stands in for /var/run/hive-metrics
LOG_FILE="${WORK}/kick-agents.log"   # stands in for /var/log/kick-agents.log
INPUT="${RUN_DIR}/actionable.json"
PROJECT_YAML="${WORK}/hive-project.yaml"
mkdir -p "$RUN_DIR" "${WORK}/bin"

# ── The path-rewritten copy ──────────────────────────────────────────────────
SCRIPT_COPY="${WORK}/bin/issue-classifier.sh"
sed \
  -e "s|/var/run/hive-metrics|${RUN_DIR}|g" \
  -e "s|/var/log/kick-agents.log|${LOG_FILE}|g" \
  "$SCRIPT" >"$SCRIPT_COPY"
if grep -qE '/var/run/hive-metrics|/var/log/kick-agents.log' "$SCRIPT_COPY" \
   || ! grep -q "$RUN_DIR" "$SCRIPT_COPY"; then
  echo "harness-error: path rewrite did not land — the script's hardcoded paths moved; update the sed above"
  exit 1
fi

# ── Fixture rules ────────────────────────────────────────────────────────────
cat >"$PROJECT_YAML" <<'YAMLEOF'
classification:
  complexity:
    simple:
      labels: ["typo"]
      title_patterns:
        - '\bspelling\b'
      model: "haiku"
    complex:
      labels: ["epic"]
      title_patterns:
        - '\bredesign\b'
      model: "opus"
    default_model: "sonnet"
  tracker_prefixes: ["[Nightly]", "[Tracker]"]
  lanes:
    architect:
      labels: ["architecture"]
      title_patterns:
        - '\bschema\b'
    packager:
      labels: ["rpm"]
      title_patterns:
        - '\bspec\s*file\b'
  clustering:
    reporter_window_seconds: 600
    failure_modes:
      oom: 'out of memory'
YAMLEOF

# Run the copy against an actionable.json built from the JSON array on $1.
# Prints nothing; output lands in $INPUT, stdout/exit code in $RUN_OUT/$RUN_RC.
run_classifier() {
  python3 - "$1" "$INPUT" <<'PYEOF'
import json, sys
items = json.loads(sys.argv[1])
with open(sys.argv[2], "w") as f:
    json.dump({"issues": {"items": items}}, f)
PYEOF
  RUN_OUT="$(HIVE_PROJECT_YAML="$PROJECT_YAML" bash "$SCRIPT_COPY" 2>&1)"
  RUN_RC=$?
}

# jget <python expr over `d`> — evaluate against the classified file.
jget() {
  python3 - "$INPUT" "$1" <<'PYEOF'
import json, sys
d = json.load(open(sys.argv[1]))
print(eval(sys.argv[2]))
PYEOF
}

# issue_field <number> <field> — field of the item with that number.
issue_field() {
  jget "[i for i in d['issues']['items'] if i['number']==$1][0].get('$2')"
}

assert_eq() {
  local desc="$1" want="$2" got="$3"
  if [ "$got" = "$want" ]; then pass "$desc"; else fail "$desc" "want '$want', got '$got'"; fi
}

item() { # number repo title labels-json [author] [created_at]
  python3 - "$@" <<'PYEOF'
import json, sys
n, repo, title, labels = sys.argv[1:5]
author = sys.argv[5] if len(sys.argv) > 5 else ""
created = sys.argv[6] if len(sys.argv) > 6 else ""
o = {"number": int(n), "repo": repo, "title": title, "labels": json.loads(labels)}
if author:
    o["author"] = author
if created:
    o["created_at"] = created
print(json.dumps(o))
PYEOF
}

join_items() { local IFS=,; echo "[$*]"; }

echo "=== issue-classifier.sh contract ==="

# ── complexity tier ──────────────────────────────────────────────────────────
echo "-- complexity tier --"
run_classifier "$(join_items \
  "$(item 1 o/r 'plain work' '[]')" \
  "$(item 2 o/r 'fix spelling in docs' '[]')" \
  "$(item 3 o/r 'tidy' '["typo"]')" \
  "$(item 4 o/r 'redesign the queue' '[]')" \
  "$(item 5 o/r 'tidy up' '["epic"]')" \
  "$(item 6 o/r 'fix spelling and redesign' '[]')")"
assert_eq "script exits 0 on a valid input" "0" "$RUN_RC"
assert_eq "no rule match is Medium" "Medium" "$(issue_field 1 complexity_tier)"
assert_eq "no rule match gets default_model" "sonnet" "$(issue_field 1 model_recommendation)"
assert_eq "simple title pattern is Simple" "Simple" "$(issue_field 2 complexity_tier)"
assert_eq "simple tier recommends haiku" "haiku" "$(issue_field 2 model_recommendation)"
assert_eq "simple label is Simple" "Simple" "$(issue_field 3 complexity_tier)"
assert_eq "complex title pattern is Complex" "Complex" "$(issue_field 4 complexity_tier)"
assert_eq "complex tier recommends opus" "opus" "$(issue_field 4 model_recommendation)"
assert_eq "complex label is Complex" "Complex" "$(issue_field 5 complexity_tier)"
assert_eq "Simple wins when both Simple and Complex match" "Simple" "$(issue_field 6 complexity_tier)"
assert_eq "tier_counts add up" "Complex=2 Medium=1 Simple=3" \
  "$(jget "' '.join(k+'='+str(v) for k,v in sorted(d['classification']['tier_counts'].items()))")"

# ── tracker detection ────────────────────────────────────────────────────────
echo "-- tracker detection --"
run_classifier "$(join_items \
  "$(item 10 o/r '[Nightly] failures for 2026-11-17' '[]')" \
  "$(item 11 o/r '[Tracker] release 3' '[]')" \
  "$(item 12 o/r 'about the [Nightly] job' '[]')" \
  "$(item 13 o/r 'ordinary issue' '[]')")"
assert_eq "[Nightly] prefix is a tracker" "True" "$(issue_field 10 is_tracker)"
assert_eq "second configured prefix is a tracker" "True" "$(issue_field 11 is_tracker)"
assert_eq "prefix in the middle of a title is not a tracker" "False" "$(issue_field 12 is_tracker)"
assert_eq "ordinary issue is not a tracker" "False" "$(issue_field 13 is_tracker)"
assert_eq "tracker_count counts only trackers" "2" "$(jget "d['classification']['tracker_count']")"

# ── lane assignment ──────────────────────────────────────────────────────────
echo "-- lane assignment --"
run_classifier "$(join_items \
  "$(item 20 o/r 'nothing special' '[]')" \
  "$(item 21 o/r 'rework the schema' '[]')" \
  "$(item 22 o/r 'x' '["rpm"]')" \
  "$(item 23 o/r 'fix the spec file' '[]')" \
  "$(item 24 o/r 'schema and spec file' '[]')")"
assert_eq "no lane rule match defaults to scanner" "scanner" "$(issue_field 20 lane)"
assert_eq "title pattern selects architect lane" "architect" "$(issue_field 21 lane)"
assert_eq "architect lane sets needs_architecture_review" "True" "$(issue_field 21 needs_architecture_review)"
assert_eq "label selects packager lane" "packager" "$(issue_field 22 lane)"
assert_eq "non-architect lane leaves needs_architecture_review false" "False" "$(issue_field 22 needs_architecture_review)"
assert_eq "title pattern selects packager lane" "packager" "$(issue_field 23 lane)"
assert_eq "first matching lane in config order wins" "architect" "$(issue_field 24 lane)"
assert_eq "default lane leaves needs_architecture_review false" "False" "$(issue_field 20 needs_architecture_review)"
assert_eq "lane_counts tally each lane" "architect=2 packager=2 scanner=1" \
  "$(jget "' '.join(k+'='+str(v) for k,v in sorted(d['classification']['lane_counts'].items()))")"

# ── cluster key ──────────────────────────────────────────────────────────────
echo "-- cluster key --"
LONG_PREFIX="$(printf 'a%.0s' $(seq 1 45))"
run_classifier "$(join_items \
  "$(item 30 o/r 'Build Cache: stale entries' '[]')" \
  "$(item 31 o/r 'no colon here' '["area/Net","kind/bug"]')" \
  "$(item 32 o/r 'no colon here either' '["kind/Bug"]')" \
  "$(item 33 o/r "${LONG_PREFIX}: too long to be a component" '["area/api"]')" \
  "$(item 34 o/r 'no colon, no area' '["other"]')")"
assert_eq "title prefix before first colon is lower-cased and hyphenated" "build-cache" "$(issue_field 30 cluster_key)"
assert_eq "area label is the key when the title has no colon" "area/net" "$(issue_field 31 cluster_key)"
assert_eq "kind label is the key when there is no area label" "kind/bug" "$(issue_field 32 cluster_key)"
assert_eq "a 40+ char prefix is ignored in favour of the area label" "area/api" "$(issue_field 33 cluster_key)"
assert_eq "no prefix and no area/kind label gives null" "None" "$(issue_field 34 cluster_key)"

# ── clusters ─────────────────────────────────────────────────────────────────
echo "-- clusters --"
run_classifier "$(join_items \
  "$(item 40 o/r 'Cache: one' '[]')" \
  "$(item 41 o/r 'Cache: two' '[]')" \
  "$(item 42 o/r 'Lonely: only one' '[]')" \
  "$(item 43 o/r 'Worker: crashed, out of memory' '[]')" \
  "$(item 44 o/r 'builder died: Out Of Memory' '[]')")"
assert_eq "two issues sharing a prefix form a cluster" "2" \
  "$(jget "next(c['count'] for c in d['clusters'] if c['key']=='cache')")"
assert_eq "a single-issue prefix forms no cluster" "False" \
  "$(jget "any(c['key']=='lonely' for c in d['clusters'])")"
assert_eq "failure_modes regex clusters issues across prefixes (case-insensitive)" "2" \
  "$(jget "next(c['count'] for c in d['clusters'] if c['key']=='failure-oom')")"
assert_eq "cluster_count matches the clusters list" "$(jget "len(d['clusters'])")" \
  "$(jget "d['classification']['cluster_count']")"

# Reporter-window clustering: same author within the window clusters; outside
# the window (600s in the fixture) it does not.
run_classifier "$(join_items \
  "$(item 50 o/r 'first thing' '[]' alice 2026-11-17T10:00:00Z)" \
  "$(item 51 o/r 'second thing' '[]' alice 2026-11-17T10:05:00Z)" \
  "$(item 52 o/r 'late thing' '[]' alice 2026-11-17T12:00:00Z)" \
  "$(item 53 o/r 'other author thing' '[]' bob 2026-11-17T10:01:00Z)")"
assert_eq "same-author issues inside the window share a reporter key" "reporter-alice-50" "$(issue_field 50 cluster_key)"
assert_eq "second issue in the window gets the same key" "reporter-alice-50" "$(issue_field 51 cluster_key)"
assert_eq "an issue outside the window is not clustered" "None" "$(issue_field 52 cluster_key)"
assert_eq "a different author is not clustered with it" "None" "$(issue_field 53 cluster_key)"

# ── output envelope, failure paths ───────────────────────────────────────────
echo "-- envelope and failure paths --"
run_classifier "$(join_items "$(item 60 o/r 'a: b' '[]')")"
assert_eq "summary line reports the counts" "Classified 1 issues: 0S/1M/0C, 0 clusters, lanes={'scanner': 1}" "$RUN_OUT"
assert_eq "classification.config_source records the yaml used" "$PROJECT_YAML" "$(jget "d['classification']['config_source']")"
if grep -q 'DONE — classification complete' "$LOG_FILE" 2>/dev/null; then
  pass "completion is logged to the (rewritten) log path"
else
  fail "completion is logged to the (rewritten) log path" "log: $(cat "$LOG_FILE" 2>/dev/null)"
fi

rm -f "$INPUT"
RUN_OUT="$(HIVE_PROJECT_YAML="$PROJECT_YAML" bash "$SCRIPT_COPY" 2>&1)"
RUN_RC=$?
assert_eq "missing input exits 0 (enumeration is optional)" "0" "$RUN_RC"
if [ ! -e "$INPUT" ] && grep -q 'ERROR: .* not found — skipping classification' "$LOG_FILE"; then
  pass "missing input is logged and no file is created"
else
  fail "missing input is logged and no file is created"
fi

# When HIVE_PROJECT_YAML points at nothing, the script falls back to the first
# examples/**/hive-project.yaml next to the install. A file with no
# classification section must leave the built-in defaults in force (default
# tracker prefixes, the auto-qa simple label) rather than failing the kick.
# The copy sits in ${WORK}/bin, so the fallback looks in ${WORK}/examples.
mkdir -p "${WORK}/examples/demo"
echo 'unrelated: true' >"${WORK}/examples/demo/hive-project.yaml"
python3 - "$INPUT" <<'PYEOF'
import json, sys
json.dump({"issues": {"items": [
    {"number": 70, "repo": "o/r", "title": "[Auto-QA] sweep", "labels": ["auto-qa"]},
    {"number": 71, "repo": "o/r", "title": "[Tracker] not a default prefix", "labels": []}]}},
    open(sys.argv[1], "w"))
PYEOF
RUN_OUT="$(HIVE_PROJECT_YAML="${WORK}/does-not-exist.yaml" bash "$SCRIPT_COPY" 2>&1)"
RUN_RC=$?
assert_eq "absent rules file falls back to examples/ and exits 0" "0" "$RUN_RC"
assert_eq "fallback records the examples file as config_source" "${WORK}/examples/demo/hive-project.yaml" \
  "$(jget "d['classification']['config_source']")"
assert_eq "built-in default tracker prefix applies" "True" "$(issue_field 70 is_tracker)"
assert_eq "a prefix outside the defaults is not a tracker" "False" "$(issue_field 71 is_tracker)"
assert_eq "built-in default simple label applies" "Simple" "$(issue_field 70 complexity_tier)"

echo ""
echo "Passed: ${PASS}  Failed: ${FAIL}"
[ "$FAIL" -eq 0 ]
