package main

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Everything the probe emits (JSON report, job summary, issue bodies) is
// public: tuna-os/hive is a public repository. Text that originates from a
// Hive (bead titles, alert messages, status evidence) passes through redact
// before it is stored, as defence in depth against an agent having echoed a
// credential into something the API serves.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`(?i)\b(token|authorization|x-hive-internal|hive_session|password|secret|api[_-]?key)(\s*[:=]\s*)["']?[^\s"',;]{8,}`),
	regexp.MustCompile(`(?i)\b(bearer)(\s+)[A-Za-z0-9._~+/=-]{8,}`),
	// Long opaque blobs (base64 / hex) of 48+ chars. Git SHAs (40) survive.
	regexp.MustCompile(`\b[A-Za-z0-9+/_-]{48,}={0,2}`),
}

// redact scrubs likely secrets and bounds the length.
func redact(s string, max int) string {
	s = strings.TrimSpace(s)
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			// Keep the keyword of a key=value match so the text still reads.
			if sub := re.FindStringSubmatch(m); len(sub) > 2 && sub[1] != "" && re.NumSubexp() >= 2 {
				return sub[1] + sub[2] + "[REDACTED]"
			}
			return "[REDACTED]"
		})
	}
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && utf8.RuneCountInString(s) > max {
		r := []rune(s)
		s = string(r[:max]) + "…"
	}
	return s
}

// mdEscape makes text safe inside a Markdown table cell and stops it from
// pinging people or cross-linking issues (@user, #123 stay literal).
func mdEscape(s string) string {
	r := strings.NewReplacer(
		"|", `\|`,
		"\n", " ",
		"@", "@​",
		"#", "#​",
		"<", "&lt;",
		">", "&gt;",
		"`", "'",
	)
	return r.Replace(s)
}
