package hivelite

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CommentMarker heads a rendered advisory so a reader (or the Hive App, if
// it ever publishes one) can recognise it. hive-lite itself never posts.
const CommentMarker = "<!-- hive-lite:triage -->"

func jsonObject(key, value string) (string, error) {
	b, err := json.Marshal(map[string]string{key: value})
	return string(b), err
}

// RenderComment formats the advisory report for a result (Markdown, printed
// to stdout or a step summary; hive-lite does not post it). model names the
// provider/model that served the session (for transparency).
func RenderComment(r Result, model string) string {
	var b strings.Builder
	b.WriteString(CommentMarker + "\n")
	verb := "Triage"
	if r.Outcome.Tool == "task.abort" {
		verb = "Triage aborted"
	}
	fmt.Fprintf(&b, "### 🐝 hive-lite advisory: %s\n\n", verb)
	how := "explicit task.complete"
	if r.Implicit {
		how = "prose answer accepted in the finish phase"
	}
	fmt.Fprintf(&b, "<sub>Free-tier model `%s`, %d turns, %d of %d citations verified against code the session read (%s). Advisory only: hive-lite holds no write credentials and this is not a review decision.</sub>\n\n",
		model, r.Turns, r.Verified, len(r.Outcome.Citations), how)
	body := r.Outcome.Summary
	if r.Outcome.Tool == "task.abort" {
		body = r.Outcome.Reason
		if body == "" {
			body = r.Outcome.Summary
		}
	}
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n")
	var unverified []string
	for _, c := range r.Outcome.Citations {
		if !c.Verified {
			unverified = append(unverified, fmt.Sprintf("`%s:%d`", c.Path, c.Line))
		}
	}
	if len(unverified) > 0 {
		fmt.Fprintf(&b, "\n> ⚠️ Not read by the session, so unverified: %s\n", strings.Join(unverified, ", "))
	}
	return b.String()
}
