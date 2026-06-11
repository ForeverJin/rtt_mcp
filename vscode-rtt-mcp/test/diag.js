/**
 * Standalone diagnostic for "MCP request 'initialize' timed out".
 *
 * Replicates the exact spawn path the extension uses and prints
 * stderr/stdout in real time. Run from the project root:
 *
 *   node test/diag.js
 *   node test/diag.js "/path/to/python"
 *   node test/diag.js "/path/to/python" "/path/to/cwd"
 */

const { spawn } = require('node:child_process');
const path = require('node:path');

const python = process.argv[2] || 'python';
const cwd = process.argv[3] || path.join(__dirname, '..', 'mcp-rtt-server');
const args = ['-u', '-m', 'mcp_rtt_server.server'];

console.log('=== diag.js ===');
console.log('python:', python);
console.log('cwd   :', cwd);
console.log('args  :', args.join(' '));
console.log('================\n');

const proc = spawn(python, args, {
  cwd,
  stdio: ['pipe', 'pipe', 'pipe'],
  windowsHide: true,
});

let buf = '';
let reqId = 0;
let initialized = false;

proc.stdout.on('data', (chunk) => {
  process.stdout.write('[STDOUT] ' + chunk);
  buf += chunk.toString('utf-8');
  let idx;
  while ((idx = buf.indexOf('\n')) >= 0) {
    const line = buf.slice(0, idx).replace(/\r$/, '').trim();
    buf = buf.slice(idx + 1);
    if (line) console.log('\n>>> LINE-DELIMITED RESPONSE:', line, '\n');
  }
});

proc.stderr.on('data', (chunk) => {
  process.stderr.write('[STDERR] ' + chunk);
});

proc.on('exit', (code, signal) => {
  console.log(`\n[EXIT] code=${code} signal=${signal}`);
  process.exit(code ?? 1);
});

proc.on('error', (err) => {
  console.error('[SPAWN ERROR]', err.message);
  process.exit(2);
});

setTimeout(() => {
  if (initialized) {
    console.log('\n[OK] initialize completed within 2s - server is healthy');
    proc.kill();
    process.exit(0);
  }
  console.log('\n[FAIL] no initialize response in 2s - sending manually then exiting');

  const initReq = JSON.stringify({
    jsonrpc: '2.0', id: ++reqId, method: 'initialize',
    params: { protocolVersion: '2024-11-05', capabilities: {}, clientInfo: { name: 'diag', version: '0.1.0' } }
  });
  const payload = initReq + '\n';
  console.log('[SEND]', payload.trim());
  proc.stdin.write(payload);
}, 2000);

setTimeout(() => {
  console.log('\n[TIMEOUT 5s] killing process');
  proc.kill();
  process.exit(3);
}, 5000);
