// peer_uid.js — refuse the shared dashboard token from an agent's user.
//
// Agents run in this container under their own UIDs and reach loopback. The
// dashboard token makes a request the verified owner, so a token that reaches
// an agent by any route would let it act as the owner through this proxy. For
// a loopback peer, look up the UID that owns the client socket in
// /proc/net/tcp{,6} and refuse the token when that UID is an agent's (the
// hive's UID map, the same file the Go egress proxy attributes agents with).
// pkg/dashboard/shared_token_peer.go makes the same check on the Go API.
//
// Fails open when the owner cannot be determined (no /proc, no UID map, no
// matching socket): this is a second layer behind keeping the token out of
// agents' environment and off ttyd's command line, and failing closed would
// lock the owner's own tooling out on hives without per-agent UIDs.
import fs from 'fs';

export const PROC_NET_TABLES = ['/proc/net/tcp', '/proc/net/tcp6'];
export const UID_MAP_PATHS = ['/var/run/hive/uid-map.json', '/data/.hive/uid-map.json'];

// socketOwnerUID returns the UID owning the local socket bound to port, or -1.
export function socketOwnerUID(port, tables = PROC_NET_TABLES) {
  const hexPort = Number(port).toString(16).toUpperCase().padStart(4, '0');
  for (const table of tables) {
    let text;
    try {
      text = fs.readFileSync(table, 'utf8');
    } catch {
      continue;
    }
    for (const line of text.split('\n').slice(1)) {
      const fields = line.trim().split(/\s+/);
      if (fields.length < 8) continue;
      const local = fields[1];
      const port = local.slice(local.lastIndexOf(':') + 1).toUpperCase();
      if (port !== hexPort) continue;
      const uid = Number.parseInt(fields[7], 10);
      if (Number.isInteger(uid)) return uid;
    }
  }
  return -1;
}

// agentUIDs returns the set of agent UIDs from the first readable UID map.
export function agentUIDs(paths = UID_MAP_PATHS) {
  for (const p of paths) {
    try {
      const m = JSON.parse(fs.readFileSync(p, 'utf8'));
      return new Set(Object.values((m && m.agents) || {}).filter(Number.isInteger));
    } catch {
      // next path
    }
  }
  return new Set();
}

function isLoopback(addr) {
  return addr === '::1' || /^(::ffff:)?127\./.test(addr || '');
}

// peerIsAgentUser reports whether socket's peer is a local process running as
// one of the hive's agent users.
export function peerIsAgentUser(socket, { tables = PROC_NET_TABLES, maps = UID_MAP_PATHS } = {}) {
  if (!socket || !isLoopback(socket.remoteAddress) || !socket.remotePort) return false;
  const uid = socketOwnerUID(socket.remotePort, tables);
  if (uid < 0) return false;
  return agentUIDs(maps).has(uid);
}
