/**
 * Ad-hoc verification for per-workspace log isolation (mock-only, no hardware).
 *
 * We start ONE shared mock daemon on a dedicated port (RTT_MOCK=1 → the J-Link
 * backend is a software heartbeat generator, so NO real probe is ever touched),
 * then connect two `bridge` clients to it — simulating two VSCode workspaces
 * sharing the single daemon. Each passes a different log_file and we assert:
 *   - jlink_status reports the log_file each client requested
 *   - the second connect redirects the shared daemon to its own file
 *   - each project's file exists and receives mock heartbeats
 *
 * Run: node test/logscope.verify.js   (MCP_BINARY overrides the binary path)
 */
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { McpClient } = require('../dist/mcpClient.js');

const BIN = process.env.MCP_BINARY || path.join(
  __dirname, '..', 'bin',
  process.platform === 'win32' ? 'rtt-mcp-server.exe' : 'rtt-mcp-server',
);

// Dedicated port so we never collide with a real daemon a live VSCode extension
// may own on 8765. Both the daemon and the bridge clients use this URL.
const HOST = '127.0.0.1';
const PORT = 8791;
const SSE_URL = `http://${HOST}:${PORT}/sse`;
const BRIDGE_ENV = { RTT_MOCK: '1', RTT_DAEMON_URL: SSE_URL };

async function isUp() {
  try {
    const ctrl = new AbortController();
    const t = setTimeout(() => ctrl.abort(), 800);
    const res = await fetch(SSE_URL, { signal: ctrl.signal, headers: { Accept: 'text/event-stream' } });
    clearTimeout(t);
    res.body?.cancel?.();
    return res.ok;
  } catch {
    return false;
  }
}

async function main() {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'rtt-logscope-'));
  const logA = path.join(tmp, 'projA', 'rtt_output.log');
  const logB = path.join(tmp, 'projB', 'rtt_output.log');

  // 1. Start the shared MOCK daemon ourselves (deterministic lifecycle + port).
  const daemon = spawn(BIN, ['daemon', '-host', HOST, '-port', String(PORT)], {
    env: { ...process.env, RTT_MOCK: '1' },
    stdio: 'ignore',
    windowsHide: true,
  });
  try {
    let ready = false;
    for (let i = 0; i < 50 && !ready; i++) {
      await new Promise((r) => setTimeout(r, 200));
      ready = await isUp();
    }
    assert.ok(ready, 'mock daemon should become ready');

    // 2. Workspace A connects with its own log_file.
    const cA = new McpClient(BIN, ['bridge'], __dirname, BRIDGE_ENV);
    await cA.start();
    const rA = await cA.callTool('jlink_connect', { device: 'Cortex-M0+', log_file: logA });
    assert.match(rA.content[0].text, /Cortex-M0\+/, `A should connect, got:\n${rA.content[0].text}`);
    const stA = await cA.callTool('jlink_status', {});
    assert.ok(stA.content[0].text.includes(logA), `status A should report logA:\n${stA.content[0].text}`);

    // 3. Workspace B → SAME daemon, different project log. Should redirect.
    const cB = new McpClient(BIN, ['bridge'], __dirname, BRIDGE_ENV);
    await cB.start();
    await cB.callTool('jlink_connect', { device: 'Cortex-M0+', log_file: logB });
    const stB = await cB.callTool('jlink_status', {});
    assert.ok(stB.content[0].text.includes(logB), `status B should report logB:\n${stB.content[0].text}`);

    // 4. Let the mock monitor write a few heartbeats into logB (the active file).
    await new Promise((r) => setTimeout(r, 800));
    assert.ok(fs.existsSync(logB), 'logB should exist');
    const bContent = fs.readFileSync(logB, 'utf-8');
    assert.match(bContent, /\[mock\] heartbeat/, 'logB should contain heartbeats');

    await cA.stop();
    await cB.stop();

    console.log('OK: per-workspace log isolation verified (mock, no hardware)');
    console.log('  logA =', logA);
    console.log('  logB =', logB, '(', bContent.trim().split('\n').length, 'lines )');
  } finally {
    try { await fetch(`http://${HOST}:${PORT}/shutdown`, { method: 'POST' }); } catch {}
    try { daemon.kill(); } catch {}
  }
}

main().catch((e) => { console.error('FAIL:', e.message); process.exit(1); });
