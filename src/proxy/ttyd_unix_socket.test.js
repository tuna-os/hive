// By default deploy/entrypoint.sh starts ttyd on a unix socket
// (HIVE_TTYD_SOCKET) in a directory only the hive's user can enter, and with
// no credential: on loopback TCP any local user, agents included, could
// connect, so ttyd needed a password, and its password (the dashboard token)
// sat in /proc/<pid>/cmdline for every user to read.
//
// Pins, through the real proxy against a mock ttyd on a unix socket:
//   1. /terminal reaches ttyd over HIVE_TTYD_SOCKET, not over TCP
//   2. no Authorization reaches ttyd there, even with HIVE_TTYD_CREDENTIAL set
//   3. the websocket upgrade takes the same socket
import { createServer, request as httpRequest } from 'http';
import { strict as assert } from 'assert';
import { spawn } from 'child_process';
import { fileURLToPath } from 'url';
import fs from 'fs';
import os from 'os';
import path from 'path';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19031;
const GO_PORT = 19032;
// A TCP port nothing listens on: reaching ttyd at all proves the socket was used.
const UNUSED_TTYD_PORT = 19033;

let goServer, ttydServer, proxyProcess, sockDir;
let lastAuthorization = 'unset';
let upgrades = 0;

function setupMockGoBackend() {
  const server = createServer((req, res) => {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end('{}');
  });
  return new Promise(resolve => server.listen(GO_PORT, () => resolve(server)));
}

function setupMockTtyd(socketPath) {
  const server = createServer((req, res) => {
    lastAuthorization = req.headers.authorization;
    res.writeHead(200, { 'Content-Type': 'text/html' });
    res.end('ttyd-over-socket');
  });
  server.on('upgrade', (req, socket) => {
    upgrades += 1;
    lastAuthorization = req.headers.authorization;
    socket.end('HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n');
  });
  return new Promise(resolve => server.listen(socketPath, () => resolve(server)));
}

function startProxy(extraEnv) {
  return new Promise((resolve, reject) => {
    const proc = spawn('node', ['server.js'], {
      cwd: __dirname,
      env: {
        ...process.env,
        HIVE_PROXY_PORT: String(PROXY_PORT),
        HIVE_API_PORT: String(GO_PORT),
        HIVE_TTYD_PORT: String(UNUSED_TTYD_PORT),
        HIVE_DASHBOARD_TOKEN: '',
        HIVE_TTYD_CREDENTIAL: '',
        HIVE_STATIC_DIR: __dirname,
        NODE_ENV: 'test',
        ...extraEnv,
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let started = false;
    proc.stdout.on('data', (d) => {
      if (!started && d.toString().includes('hive-proxy')) {
        started = true;
        resolve(proc);
      }
    });
    proc.on('error', reject);
    setTimeout(() => { if (!started) reject(new Error('proxy start timeout')); }, 10000);
  });
}

function get() {
  return new Promise((resolve, reject) => {
    const req = httpRequest({ host: '127.0.0.1', port: PROXY_PORT, path: '/terminal/?arg=hive-scanner' }, (res) => {
      let body = '';
      res.on('data', d => { body += d; });
      res.on('end', () => resolve({ status: res.statusCode, body }));
    });
    req.on('error', reject);
    req.end();
  });
}

function upgrade() {
  return new Promise((resolve, reject) => {
    const req = httpRequest({
      host: '127.0.0.1',
      port: PROXY_PORT,
      path: '/terminal/ws?arg=hive-scanner',
      headers: {
        Connection: 'Upgrade',
        Upgrade: 'websocket',
        'Sec-WebSocket-Version': '13',
        'Sec-WebSocket-Key': Buffer.from('0123456789abcdef').toString('base64'),
      },
    });
    req.on('upgrade', (res, socket) => { socket.destroy(); resolve(res.statusCode); });
    req.on('response', (res) => resolve(res.statusCode));
    req.on('error', reject);
    req.end();
  });
}

async function main() {
  sockDir = fs.mkdtempSync(path.join(os.tmpdir(), 'hive-ttyd-'));
  const socketPath = path.join(sockDir, 'ttyd.sock');
  goServer = await setupMockGoBackend();
  ttydServer = await setupMockTtyd(socketPath);

  proxyProcess = await startProxy({ HIVE_TTYD_SOCKET: socketPath, HIVE_TTYD_CREDENTIAL: 'ops:override-pass' });
  const r = await get();
  assert.equal(r.status, 200, `GET over the socket got ${r.status}`);
  assert.equal(r.body, 'ttyd-over-socket', 'the request must reach the ttyd on HIVE_TTYD_SOCKET');
  assert.equal(lastAuthorization, undefined, 'no credential is presented to a ttyd on the private socket');

  lastAuthorization = 'unset';
  const status = await upgrade();
  assert.equal(status, 101, `websocket upgrade over the socket got ${status}`);
  assert.equal(upgrades, 1, 'the upgrade must reach the ttyd on HIVE_TTYD_SOCKET');
  assert.equal(lastAuthorization, undefined, 'no credential on the upgrade either');

  console.log('ttyd_unix_socket: all assertions passed');
}

function cleanup() {
  if (proxyProcess) proxyProcess.kill();
  if (goServer) goServer.close();
  if (ttydServer) ttydServer.close();
  if (sockDir) fs.rmSync(sockDir, { recursive: true, force: true });
}

main()
  .then(() => { cleanup(); process.exit(0); })
  .catch((err) => { console.error(err); cleanup(); process.exit(1); });
