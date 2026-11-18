package agent

import "os/exec"

// tmuxServerStartCmd is the command ensureTmuxSession runs to create the
// agent's session, which also starts the agent's tmux server. The server
// keeps this command's environment for its whole life, in its global
// environment and in its own /proc/<pid>/environ, both readable by the agent.
// So it gets agentExecEnv, never the hive's environment.
func (m *Manager) tmuxServerStartCmd(agent *AgentProcess, agentDir string) *exec.Cmd {
	var cmd *exec.Cmd
	if agent.UID > 0 {
		suExecArgs := []string{"su-exec", m.agentExecUserSpec(agent)}
		tmuxArgs := append(m.tmuxBaseArgs(agent), newSessionCommands(agent.tmuxSession, agentDir)...)
		cmd = exec.Command(suExecArgs[0], append(suExecArgs[1:], tmuxArgs...)...)
	} else {
		base := m.tmuxBaseArgs(agent)
		tmuxArgs := append(base[1:], newSessionCommands(agent.tmuxSession, agentDir)...)
		cmd = exec.Command(base[0], tmuxArgs...)
	}
	cmd.Env = agentExecEnv()
	return cmd
}
