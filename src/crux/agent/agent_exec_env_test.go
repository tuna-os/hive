package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// hiveSecretSentinels are hive credentials an agent must never see: with the
// dashboard token, X-Hive-Internal is the verified owner on the dashboard API.
var hiveSecretSentinels = []string{
	"HIVE_DASHBOARD_TOKEN", "DASHBOARD_AUTH_TOKEN", "HIVE_HUB_SECRET",
	"HIVE_TERMINAL_KEY", "HIVE_TTYD_CREDENTIAL", "HIVE_SESSION_KEY",
	"HIVE_MIRROR_HUB_HEARTBEAT_KEY", "HIVE_GITHUB_TOKEN", "GH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN", "LINEAR_CLIENT_SECRET",
}

func setSecretSentinels(t *testing.T) {
	t.Helper()
	for _, k := range hiveSecretSentinels {
		t.Setenv(k, "sentinel-"+k)
	}
}

func assertNoSentinel(t *testing.T, what string, env []string) {
	t.Helper()
	for _, kv := range env {
		if strings.Contains(kv, "sentinel-") {
			k, _, _ := strings.Cut(kv, "=")
			t.Errorf("%s passes %s to the agent's user", what, k)
		}
	}
}

// The agent's tmux server keeps the environment of the command that started
// it in its global environment and in its own /proc/<pid>/environ, both
// readable by the agent. Neither the server start nor any tmux client may
// inherit the hive's environment.
func TestTmuxServerStartsWithoutHiveSecrets(t *testing.T) {
	setSecretSentinels(t)
	t.Setenv("HIVE_SOME_SETTING", "kept")
	m := NewManager(map[string]config.AgentConfig{
		"guide": {Backend: "claude", Mode: "ISSUES_AND_PRS"},
	}, discardLogger(), ProjectContext{ACMMLevel: 3})
	for _, uid := range []int{0, 2001} {
		agent := &AgentProcess{Name: "guide", UID: uid, tmuxSocket: "hive-guide", tmuxSession: "hive-guide"}
		for _, cmd := range []*exec.Cmd{
			m.tmuxServerStartCmd(agent, t.TempDir()),
			m.tmuxCmd(agent, "has-session", "-t", agent.tmuxSession),
		} {
			if cmd.Env == nil {
				t.Fatalf("uid %d: %v inherits the hive environment (Env is nil)", uid, cmd.Args)
			}
			assertNoSentinel(t, fmt.Sprintf("uid %d: %v", uid, cmd.Args), cmd.Env)
			if !containsEnv(cmd.Env, "HIVE_SOME_SETTING=kept") || !containsEnv(cmd.Env, "PATH="+os.Getenv("PATH")) {
				t.Errorf("uid %d: %v dropped a non-secret setting the pane needs", uid, cmd.Args)
			}
		}
	}
}

func TestSuExecCommandsGetAgentExecEnv(t *testing.T) {
	setSecretSentinels(t)
	for _, cmd := range []*exec.Cmd{suExec("hive-guide", "true"), suExecContext(t.Context(), "hive-guide", "true")} {
		if cmd.Env == nil {
			t.Fatalf("%v inherits the hive environment", cmd.Args)
		}
		assertNoSentinel(t, fmt.Sprint(cmd.Args), cmd.Env)
	}
}

func TestAgentExecEnvAllowed(t *testing.T) {
	for k, want := range map[string]bool{
		"PATH": true, "LANG": true, "LC_ALL": true, "TZ": true, "NODE_OPTIONS": true,
		"HIVE_API_PORT": true, "HIVE_CONFIG": true, "HIVE_REPO": true,
		"OMP_AUTH_BROKER_URL": true, "OPENAI_API_KEY": true,
		"HIVE_DASHBOARD_TOKEN": false, "DASHBOARD_AUTH_TOKEN": false,
		"HIVE_HUB_SECRET": false, "HIVE_TERMINAL_KEY": false,
		"HIVE_TTYD_CREDENTIAL": false, "HIVE_SESSION_KEY": false,
		"HIVE_MIRROR_HUB_HEARTBEAT_KEY": false, "HIVE_GITHUB_TOKEN": false,
		"HIVE_REGISTRATION_TOKEN": false, "GH_TOKEN": false, "GITHUB_TOKEN": false,
		"CLAUDE_CODE_OAUTH_TOKEN": false, "LINEAR_API_KEY": false,
		"GH_APP_KEY_FILE": false, "SOME_UNKNOWN_VAR": false,
	} {
		if got := agentExecEnvAllowed(k); got != want {
			t.Errorf("agentExecEnvAllowed(%q) = %v, want %v", k, got, want)
		}
	}
}

// With real tmux: a server started through tmuxServerStartCmd must not list
// a hive secret in its GLOBAL environment. The pane-level test in
// tmux_inherited_env_test.go cannot see this, because the session removal
// hides the global value from panes while the server still holds it.
func TestTmuxServerGlobalEnvHasNoHiveSecrets(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	setSecretSentinels(t)
	m := NewManager(map[string]config.AgentConfig{
		"guide": {Backend: "claude", Mode: "ISSUES_AND_PRS"},
	}, discardLogger(), ProjectContext{ACMMLevel: 3})
	socket := fmt.Sprintf("hglobal%d", os.Getpid())
	agent := &AgentProcess{Name: "guide", UID: 0, tmuxSocket: socket, tmuxSession: "hive-global"}
	if out, err := m.tmuxServerStartCmd(agent, t.TempDir()).CombinedOutput(); err != nil {
		t.Fatalf("starting tmux server: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	out, err := exec.Command("tmux", "-L", socket, "show-environment", "-g").CombinedOutput()
	if err != nil {
		t.Fatalf("show-environment -g: %v: %s", err, out)
	}
	assertNoSentinel(t, "the tmux server's global environment", strings.Split(string(out), "\n"))
}

func containsEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
