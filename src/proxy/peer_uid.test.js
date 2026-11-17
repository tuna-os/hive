// The dashboard token is the owner credential. peer_uid.js refuses it from a
// loopback peer whose socket is owned by one of the hive's agent users.
import { strict as assert } from 'assert';
import fs from 'fs';
import net from 'net';
import os from 'os';
import path from 'path';
import { socketOwnerUID, agentUIDs, peerIsAgentUser } from './peer_uid.js';

const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'peer-uid-'));
const header = '  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n';
const tcp = path.join(dir, 'tcp');
// 0100007F:C350 is 127.0.0.1:50000, owned by uid 2005.
fs.writeFileSync(tcp, header +
  '   0: 0100007F:0BB9 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 1 1 0 100 0 0 10 0\n' +
  '   1: 0100007F:C350 0100007F:0BB9 01 00000000:00000000 00:00000000 00000000  2005        0 2 1 0 100 0 0 10 0\n');
const map = path.join(dir, 'uid-map.json');
fs.writeFileSync(map, JSON.stringify({ agents: { guide: 2005, scanner: 2006 }, proxy_uid: 1001, base_uid: 2001 }));

assert.equal(socketOwnerUID(50000, [tcp]), 2005);
assert.equal(socketOwnerUID(3001, [tcp]), 1001);
assert.equal(socketOwnerUID(40000, [tcp]), -1);
assert.deepEqual([...agentUIDs([path.join(dir, 'missing'), map])].sort(), [2005, 2006]);
assert.equal(agentUIDs([path.join(dir, 'missing')]).size, 0);

const opts = { tables: [tcp], maps: [map] };
assert.equal(peerIsAgentUser({ remoteAddress: '127.0.0.1', remotePort: 50000 }, opts), true, 'agent uid on loopback is refused');
assert.equal(peerIsAgentUser({ remoteAddress: '::ffff:127.0.0.1', remotePort: 50000 }, opts), true, 'IPv4-mapped loopback too');
assert.equal(peerIsAgentUser({ remoteAddress: '10.0.0.5', remotePort: 50000 }, opts), false, 'non-loopback peers are not looked up');
assert.equal(peerIsAgentUser({ remoteAddress: '127.0.0.1', remotePort: 40000 }, opts), false, 'unknown owner fails open');
assert.equal(peerIsAgentUser({ remoteAddress: '127.0.0.1', remotePort: 50000 }, { tables: [tcp], maps: [path.join(dir, 'missing')] }), false, 'no UID map fails open');

// Against the real kernel table where there is one: our own connection is
// attributed to our own uid.
if (fs.existsSync('/proc/net/tcp') && typeof process.getuid === 'function') {
  const self = path.join(dir, 'self-map.json');
  fs.writeFileSync(self, JSON.stringify({ agents: { me: process.getuid() } }));
  const server = net.createServer();
  await new Promise(r => server.listen(0, '127.0.0.1', r));
  const seen = new Promise(r => server.once('connection', r));
  const client = net.connect(server.address().port, '127.0.0.1');
  const sock = await seen;
  assert.equal(peerIsAgentUser(sock, { maps: [self] }), true, 'a real loopback peer is attributed to its uid');
  assert.equal(peerIsAgentUser(sock, { maps: [map] }), false, 'a uid outside the map is not refused');
  client.destroy();
  server.close();
}

fs.rmSync(dir, { recursive: true, force: true });
console.log('peer_uid: all assertions passed');
