package main

import (
	"strings"
	"testing"
)

func TestRedactScrubsSecrets(t *testing.T) {
	for _, in := range []string{
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789 leaked",
		"github_pat_11ABCDEFG0123456789_abcdefghijklmnop",
		"key sk-proj-abcdefghijklmnopqrstu",
		"Authorization: Bearer abc.def.ghijklmnop",
		"curl -H 'X-Hive-Internal: 9f8e7d6c5b4a39281706'",
		"Cookie hive_session=0123456789abcdef",
		"AKIAABCDEFGHIJKLMNOP",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"blob QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9wcXJzdHV2d3h5eg==",
	} {
		out := redact(in, 0)
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("not redacted: %q -> %q", in, out)
		}
	}
}

func TestRedactKeepsOrdinaryText(t *testing.T) {
	for _, in := range []string{
		"blocked: crash-looping (9 restarts in 24h): login token refreshed",
		"Agent \"operations\" is alive but not producing: no production evidence for 6h1m0s",
		"main commit 253d595 lost verify-smoke verdicts; see 0123456789abcdef0123456789abcdef01234567",
		"pass basic authentication tests",
	} {
		if out := redact(in, 0); out != in {
			t.Errorf("changed: %q -> %q", in, out)
		}
	}
}

func TestRedactTruncatesAndCollapsesWhitespace(t *testing.T) {
	if got := redact("  a\n\n b  ", 0); got != "a b" {
		t.Fatalf("%q", got)
	}
	if got := redact(strings.Repeat("é", 10), 4); got != "éééé…" {
		t.Fatalf("%q", got)
	}
}

func TestMdEscape(t *testing.T) {
	got := mdEscape("a|b @me #12 <x> `c`\nd")
	for _, bad := range []string{"@me", "#12", "<x>", "`", "\n"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q still contains %q", got, bad)
		}
	}
	if !strings.Contains(got, `a\|b`) {
		t.Errorf("pipe not escaped: %q", got)
	}
}
