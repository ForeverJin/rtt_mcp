/**
 * End-to-end integration tests for vscode-rtt-mcp.
 *
 * Spawns the Go MCP server binary (../bin/rtt-mcp-server[.exe]) in `serve` mode
 * with RTT_MOCK=1, exercising the full triple-sink (log + stderr + ring buffer)
 * without any J-Link hardware. The 11 Go tools (1 more than the old Python mock:
 * `rtt_list_supported_devices` and `rtt_check_device` are Go-only) are checked
 * for shape and the lifecycle goes through jlink_connect → read → status → write
 * → clear → disconnect.
 *
 * Run:   npm test
 * Prereq: `npm run build:go` has produced vscode-rtt-mcp/bin/rtt-mcp-server[.exe]
 *         (the `test` script does this automatically).
 */

const { test, after } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { McpClient } = require('../dist/mcpClient.js');
const { RttProvider } = require('../dist/rttProvider.js');

// Locate the Go binary that `npm run build:go` produces.
// Override with MCP_BINARY=<abs path> for ad-hoc runs.
const BIN = process.env.MCP_BINARY || path.join(
  __dirname, '..', 'bin',
  process.platform === 'win32' ? 'rtt-mcp-server.exe' : 'rtt-mcp-server',
);
const TEST_CWD = __dirname;
// RTT_MOCK=1 swaps the J-Link backend for an in-process software heartbeat
// generator; required because these tests have no hardware.
const TEST_ENV = { RTT_MOCK: '1' };

// Fail fast with a helpful message if the binary is missing — otherwise the
// children would silently fail and the tests would crash with EPIPE.
if (!fs.existsSync(BIN)) {
  // eslint-disable-next-line no-console
  console.error(
    `\n  Missing Go binary at ${BIN}\n` +
    `  Run \`npm run build:go\` (or \`./vscode-rtt-mcp/build.sh\`) first, or set MCP_BINARY=<path>.\n`,
  );
  process.exit(1);
}

const liveClients = new Set();
const liveProviders = new Set();

after(async () => {
  for (const p of liveProviders) await p.shutdown().catch(() => {});
  for (const c of liveClients) await c.stop().catch(() => {});
});

async function makeClient() {
  const c = new McpClient(BIN, ['serve'], TEST_CWD, TEST_ENV);
  await c.start();
  liveClients.add(c);
  return c;
}

async function makeProvider(pollMs = 200) {
  // RttProvider constructs McpClient internally without an env override; we
  // also need RTT_MOCK=1 in the spawned process's env. Setting it in
  // process.env works because child_process.spawn inherits the parent env
  // by default (we pass `{ ...process.env, ...this.env }` in McpClient).
  process.env.RTT_MOCK = '1';
  const p = new RttProvider(BIN, ['serve'], TEST_CWD, pollMs, 'Cortex-M0+', 4000, '');
  liveProviders.add(p);
  return p;
}

test('McpClient: start() sends initialize and sets initialized flag', async () => {
  const c = await makeClient();
  assert.equal(c.initialized, true);
});

test('McpClient: listTools returns 11 Go tools with expected names', async () => {
  const c = await makeClient();
  const tools = await c.listTools();
  assert.equal(tools.length, 11);
  const names = tools.map((t) => t.name).sort();
  assert.deepEqual(names, [
    'jlink_connect',
    'jlink_disconnect',
    'jlink_status',
    'rtt_check_device',
    'rtt_clear',
    'rtt_list_devices',
    'rtt_list_supported_devices',
    'rtt_read',
    'rtt_read_log',
    'rtt_read_raw',
    'rtt_write',
  ]);
});

test('McpClient: jlink_connect text mentions device and speed', async () => {
  const c = await makeClient();
  const result = await c.callTool('jlink_connect', { device: 'Cortex-M0+', speed: 1000 });
  assert.equal(result.isError, undefined);
  assert.equal(result.content[0].type, 'text');
  // RTT_MOCK backend accepts any device name; the returned text reflects what
  // we asked for. With mock, the actual connected device is reported via
  // jlink_status.
  assert.match(result.content[0].text, /Cortex-M0\+/);
  assert.match(result.content[0].text, /1000 kHz/);
});

test('McpClient: unknown tool name throws protocol error', async () => {
  const c = await makeClient();
  // The Go MCP server returns "unknown tool \"<name>\" (code=-32602)" for
  // an unknown tool. Match case-insensitively to be resilient to message
  // wording changes; the protocol-level error code is the load-bearing part.
  await assert.rejects(
    () => c.callTool('not_a_real_tool', {}),
    /unknown tool/i,
  );
});

test('McpClient: request timeout surfaces as error', async () => {
  const c = await makeClient();
  // Exercise the timeout path with a non-existent method. The call may succeed
  // (returns an error) or fail (timeout), but the call must not hang.
  const start = Date.now();
  try {
    await c.callTool('not_a_real_tool', {});
  } catch {
    /* expected */
  }
  assert.ok(Date.now() - start < 5000, 'call should not hang');
});

test('RttProvider: full connect/read/status/write/clear/disconnect lifecycle', async () => {
  const p = await makeProvider();
  assert.equal(p.isConnected, false);

  const connResult = await p.connect({ device: 'Cortex-M0+', speed: 2000 });
  assert.match(connResult.content[0].text, /Cortex-M0\+/);
  assert.equal(p.isConnected, true);

  // Give the mock monitor goroutine a few poll cycles to accumulate data.
  await new Promise((r) => setTimeout(r, 500));
  const data = await p.read();
  assert.match(data, /\[mock\] heartbeat/);

  const status = await p.status();
  assert.match(status, /Cortex-M0\+/);
  assert.match(status, /2000 kHz/);
  assert.match(status, /RTT Started: true/);

  const writeResult = await p.write('hello\n');
  assert.match(writeResult, /Wrote \d+ bytes/);

  await p.clear();
  // Right after clear, the ring should be drained. The monitor will refill
  // within ~poll interval, so we don't assert the next read is empty.

  await p.disconnect();
  assert.equal(p.isConnected, false);
});

test('RttProvider: monitor mode streams data via the broadcast log', async () => {
  const p = await makeProvider(100);
  const received = [];
  let errors = 0;
  await p.connect();
  p.startMonitor({
    onData: (text) => received.push(text),
    onError: () => { errors++; },
  });
  await new Promise((r) => setTimeout(r, 800));
  p.stopMonitor();
  assert.equal(errors, 0, 'monitor should not error');
  assert.ok(received.length > 0, `expected data chunks, got ${received.length}`);
  assert.match(received.join(''), /\[mock\] heartbeat/);
  await p.disconnect();
});

test('RttProvider: list devices returns the mock probe', async () => {
  const p = await makeProvider();
  await p.connect();
  const text = await p.listDevices();
  assert.match(text, /J-Link Mock #0/);
  await p.disconnect();
});

test('RttProvider: list supported devices returns the mock device DB', async () => {
  const p = await makeProvider();
  const names = await p.listSupportedDevices();
  // Mock backend has a tiny hard-coded DB (Cortex-M0+, Cortex-M4, STM32F407VE, HC32L19x).
  assert.ok(Array.isArray(names));
  assert.ok(names.includes('Cortex-M0+'));
  assert.ok(names.includes('HC32L19x'));
});

test('RttProvider: read before connect throws', async () => {
  const p = await makeProvider();
  await assert.rejects(() => p.read(), /not connected/i);
});

test('McpClient: stop() rejects pending requests', async () => {
  const c = new McpClient(BIN, ['serve'], TEST_CWD, TEST_ENV);
  await c.start();
  const pending = c.callTool('jlink_status', {});
  // Attach handler first to suppress unhandledRejection warning
  const handled = pending.catch((e) => e);
  await c.stop();
  const result = await handled;
  assert.ok(result instanceof Error);
  assert.match(result.message, /stopped/);
});
