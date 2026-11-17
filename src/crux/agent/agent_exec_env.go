package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// agentExecEnv is the environment of every process the hive starts as an
// agent's user (su-exec), above all the agent's tmux server.
//
// It is built from a list of allowed names, not by removing known secrets
// from the hive's own environment. The hive process holds the dashboard
// token, which the dashboard API accepts as the owner, and the hub secret,
// the terminal key and the full App installation token. A process started
// with exec.Command and no Env gets all of that. Its /proc/<pid>/environ
// belongs to the agent's user, and a tmux server also copies it into its
// global environment (`show-environment -g`). The session-level removal in
// applySessionEnv hides a value from new panes only, so it cannot be the
// boundary. A removal list also has to name every new secret.
//
// What an agent is meant to have is set afterwards per session by
// agentEnvPairs. This list keeps only what a pane needs from the container
// and nothing that grants access to the hive itself.
func agentExecEnv() []string {
	return filterAgentExecEnv(os.Environ())
}

// filterAgentExecEnv keeps the entries of env that agentExecEnvAllowed
// accepts.
func filterAgentExecEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if ok && agentExecEnvAllowed(k) {
			out = append(out, kv)
		}
	}
	return out
}

// agentExecEnvNames are passed through by name.
var agentExecEnvNames = map[string]bool{
	// Process basics. su-exec sets HOME for a named user; the rest is what a
	// shell, a CLI and tmux itself expect.
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"TERM": true, "LANG": true, "LANGUAGE": true, "TZ": true, "TMUX_TMPDIR": true,
	// Egress proxy and its CA, as agentSubprocessEnv passes them.
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
	"NODE_OPTIONS": true,
	// Credentials that belong to the agent's backend CLI, not to the hive:
	// model API keys a CLI reads from its environment, and the operator's
	// credential broker for omp (see agentNoProxy). None of them is accepted
	// by the hive's own API.
	"OPENAI_API_KEY": true, "CODEX_API_KEY": true, "ANTHROPIC_API_KEY": true,
	"GEMINI_API_KEY": true, "GOOGLE_API_KEY": true,
	"OMP_AUTH_BROKER_URL": true, "OMP_AUTH_BROKER_TOKEN": true,
}

// agentExecEnvSecretParts mark a HIVE_* name as a credential. Matching is on
// the upper-cased name, so HIVE_DASHBOARD_TOKEN, HIVE_HUB_SECRET,
// HIVE_TERMINAL_KEY, HIVE_TTYD_CREDENTIAL, HIVE_SESSION_KEY and
// HIVE_MIRROR_HUB_HEARTBEAT_KEY are all dropped. A false positive costs a
// setting in the pane; a false negative leaks a credential, so the parts are
// broad.
var agentExecEnvSecretParts = []string{
	"TOKEN", "SECRET", "KEY", "PASSWORD", "PASSWD", "PASS", "CREDENTIAL",
	"PRIVATE", "COOKIE", "AUTH", "DSN",
}

// agentExecEnvAllowed reports whether the variable k may reach a process
// running as an agent's user.
func agentExecEnvAllowed(k string) bool {
	if agentExecEnvNames[k] || strings.HasPrefix(k, "LC_") {
		return true
	}
	if !strings.HasPrefix(k, "HIVE_") {
		return false
	}
	upper := strings.ToUpper(k)
	for _, p := range agentExecEnvSecretParts {
		if strings.Contains(upper, p) {
			return false
		}
	}
	return true
}

// suExec is exec.Command("su-exec", args...) with agentExecEnv: the process
// runs as the agent's user, so the agent can read its environment.
func suExec(args ...string) *exec.Cmd {
	cmd := exec.Command("su-exec", args...)
	cmd.Env = agentExecEnv()
	return cmd
}

// suExecContext is suExec bound to ctx.
func suExecContext(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "su-exec", args...)
	cmd.Env = agentExecEnv()
	return cmd
}
