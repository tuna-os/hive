package dashboard

import (
	"net"
	"strconv"

	"github.com/hivecommons/hive/crux/agent"
	cruxproxy "github.com/hivecommons/hive/crux/proxy"
)

// Agents run in the hive's container under their own UIDs and can reach this
// API on loopback. The shared dashboard token makes a request the verified
// owner (X-Hive-Internal, Authorization: Bearer, and the hub's
// X-Hive-Proxy-Auth proof), so a token that reaches an agent by any route
// would let it kick, pause or rewrite any agent and change governor settings.
// For a loopback peer, the UID owning the client socket is looked up in
// /proc/net/tcp{,6}, as the egress proxy attributes agents, and the token is
// refused when that UID is an agent's.
//
// Requests the Node proxy forwards arrive from the proxy's own (hive) UID, so
// proxy/peer_uid.js makes the same check on the browser-facing side.
//
// This fails open when the owner cannot be determined (no UID map, no
// matching socket). It is a second layer behind keeping the token out of
// agents' environment (crux/agent agentExecEnv) and off ttyd's command line
// (deploy/entrypoint.sh), and failing closed would lock the owner's tooling
// out on hives without per-agent UIDs.

// peerUIDForPort resolves the UID owning the local socket bound to port.
var peerUIDForPort = cruxproxy.LookupUIDByLocalPort

// agentUIDMapPaths are tried in order; the first readable map wins.
var agentUIDMapPaths = func() []string {
	return []string{agent.UIDMapPath, agent.PersistedUIDMapPath}
}

// sharedTokenFromAgentUser reports whether remoteAddr is a loopback peer whose
// socket is owned by one of the hive's agent users.
func sharedTokenFromAgentUser(remoteAddr string) bool {
	host, portStr, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return false
	}
	uid, err := peerUIDForPort(port)
	if err != nil || uid < 0 {
		return false
	}
	for _, path := range agentUIDMapPaths() {
		m, err := agent.LoadUIDMap(path)
		if err != nil {
			continue
		}
		return m.LookupByUID(uid) != ""
	}
	return false
}
