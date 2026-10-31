package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestKioskSoundDefault executes the kiosk-only audio preference and URL override.
func TestKioskSoundDefault(t *testing.T) {
	html := indexHTML(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — kiosk sound default was NOT executed by this run")
	}
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsFunc(t, html, "stageTrackerKioskSoundDefault"),
		"assert.strictEqual(stageTrackerKioskSoundDefault(true, '?kiosk=1'), true, 'kiosk default is on');",
		"assert.strictEqual(stageTrackerKioskSoundDefault(true, '?kiosk=1&sound=off'), false, 'sound=off silences');",
		"assert.strictEqual(stageTrackerKioskSoundDefault(true, '?kiosk=1&sound=on'), true, 'any other value is on');",
		"assert.strictEqual(stageTrackerKioskSoundDefault(true, ''), true, 'no query → on');",
		"assert.strictEqual(stageTrackerKioskSoundDefault(false, '?sound=on'), false, 'never outside kiosk mode');",
	}, "\n")
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node kiosk sound default fixture failed: %v\n%s", err, out)
	}
}
