/**
 * VSCode extension entry point.
 *
 * Wires up commands, status bar, and output channel around RttProvider.
 * The status bar item is the primary entry point - click it for a quick menu.
 */

import { ChildProcess, spawn } from 'child_process';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import * as vscode from 'vscode';
import { RttProvider } from './rttProvider';

const RTT_OUTPUT_NAME = 'RTT';

/** Format current time as [HH:MM:SS.mmm] for log output. */
function ts(): string {
  const d = new Date();
  const h = String(d.getHours()).padStart(2, '0');
  const m = String(d.getMinutes()).padStart(2, '0');
  const s = String(d.getSeconds()).padStart(2, '0');
  const ms = String(d.getMilliseconds()).padStart(3, '0');
  return `${h}:${m}:${s}.${ms}`;
}

let provider: RttProvider;
let statusBar: vscode.StatusBarItem;
let output: vscode.OutputChannel;
let disposables: vscode.Disposable[] = [];

// Captured in activate so the helpers below can resolve the bundled binary and
// the log path without threading the context through every call site.
let extensionCtx: vscode.ExtensionContext;
let logFileDefault: string;

// Shared SSE daemon that owns the J-Link. The extension starts/stops it; both this
// extension (via the bridge) and Claude Code connect to it as clients.
let daemonProc: ChildProcess | null = null;
const DAEMON_URL_DEFAULT = 'http://127.0.0.1:8765/sse';

export function activate(context: vscode.ExtensionContext): void {
  extensionCtx = context;
  logFileDefault = defaultLogFile();

  const config = vscode.workspace.getConfiguration('rtt-mcp');
  const binary = resolveBinary(config);
  const serverArgs = config.get<string[]>('serverArgs', ['bridge']);
  const pollMs = config.get<number>('pollIntervalMs', 300);
  const device = config.get<string>('device', 'Cortex-M0+');
  const speed = config.get<number>('speed', 4000);

  output = vscode.window.createOutputChannel(RTT_OUTPUT_NAME);
  context.subscriptions.push(output);

  provider = new RttProvider(binary, serverArgs, '', pollMs, device, speed, logFileDefault);

  statusBar = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 100);
  statusBar.command = 'rtt-mcp.showMenu';
  statusBar.text = '$(debug-disconnect) RTT';
  statusBar.tooltip = 'RTT MCP - click to open menu';
  statusBar.show();
  context.subscriptions.push(statusBar);

  disposables = [
    register('rtt-mcp.showMenu', () => showMenu()),
    register('rtt-mcp.connect', () => connectCmd()),
    register('rtt-mcp.disconnect', () => disconnectCmd()),
    register('rtt-mcp.read', () => readCmd()),
    register('rtt-mcp.write', () => writeCmd()),
    register('rtt-mcp.listDevices', () => listDevicesCmd()),
    register('rtt-mcp.status', () => statusCmd()),
    register('rtt-mcp.clear', () => clearCmd()),
    register('rtt-mcp.toggleMonitor', () => toggleMonitorCmd()),
    register('rtt-mcp.openOutput', () => output.show(true)),
    register('rtt-mcp.openLog', () => openLogCmd()),
    register('rtt-mcp.reset', () => resetCmd()),
    register('rtt-mcp.setDevice', () => setDeviceCmd()),
  ];
  context.subscriptions.push(...disposables);

  const configSub = vscode.workspace.onDidChangeConfiguration((e) => {
    if (e.affectsConfiguration('rtt-mcp')) {
      const cfg = vscode.workspace.getConfiguration('rtt-mcp');
      void provider.shutdown();
      provider = new RttProvider(
        resolveBinary(cfg),
        cfg.get<string[]>('serverArgs', ['bridge']),
        '',
        cfg.get<number>('pollIntervalMs', 300),
        cfg.get<string>('device', 'Cortex-M0+'),
        cfg.get<number>('speed', 4000),
        logFileDefault,
      );
      updateStatusBar();
    }
  });
  context.subscriptions.push(configSub);

  if (config.get<boolean>('autoConnect', false)) {
    void connectCmd();
  } else {
    updateStatusBar();
  }
}

export function deactivate(): void {
  void provider?.shutdown();
  // Best-effort graceful teardown; deactivate is synchronous, so we can't await.
  void stopDaemon();
  for (const d of disposables) d.dispose();
}

function register(cmd: string, fn: (...args: any[]) => any): vscode.Disposable {
  return vscode.commands.registerCommand(cmd, fn);
}

/**
 * Resolve the rtt-mcp-server Go binary to spawn. An explicit `binaryPath` setting
 * wins (absolute path or a name on PATH); otherwise the binary shipped inside
 * this extension (`<extensionPath>/bin/rtt-mcp-server[.exe]`) is used, so the
 * extension is self-contained after install.
 */
function resolveBinary(cfg: vscode.WorkspaceConfiguration): string {
  const override = cfg.get<string>('binaryPath', '').trim();
  if (override) return override;
  const exeName = process.platform === 'win32' ? 'rtt-mcp-server.exe' : 'rtt-mcp-server';
  return path.join(extensionCtx.extensionPath, 'bin', exeName);
}

/** Extract host/port from the daemon SSE URL for the `daemon -host -port` flags. */
function parseDaemonHostPort(url: string): { host: string; port: number } {
  try {
    const u = new URL(url);
    return {
      host: u.hostname || '127.0.0.1',
      port: u.port ? Number(u.port) : 8765,
    };
  } catch {
    return { host: '127.0.0.1', port: 8765 };
  }
}

/**
 * Default RTT log file location, mirroring the Go server's stateDir() so
 * "Open RTT Log File" opens the file the daemon actually writes. An explicit
 * RTT_LOG_FILE env var wins; otherwise %LOCALAPPDATA% (Win),
 * ~/Library/Caches (mac), XDG_STATE_HOME|~/.local/state (Linux).
 */
function defaultLogFile(): string {
  const envFile = process.env.RTT_LOG_FILE;
  if (envFile) return path.resolve(envFile);
  let base: string;
  if (process.platform === 'win32') {
    base = process.env.LOCALAPPDATA || os.homedir();
  } else if (process.platform === 'darwin') {
    base = path.join(os.homedir(), 'Library', 'Caches');
  } else {
    base = process.env.XDG_STATE_HOME || path.join(os.homedir(), '.local', 'state');
  }
  return path.join(base, 'mcp-rtt-server', 'rtt_output.log');
}

async function isDaemonUp(url: string): Promise<boolean> {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 2000);
  try {
    // A GET to the SSE endpoint resolves as soon as headers arrive (before the
    // event stream), so this won't hang; the abort just frees the connection.
    const res = await fetch(url, { signal: ctrl.signal });
    return res.ok;
  } catch {
    return false;
  } finally {
    clearTimeout(timer);
  }
}

async function ensureDaemon(): Promise<boolean> {
  const cfg = vscode.workspace.getConfiguration('rtt-mcp');
  const url = cfg.get<string>('daemonUrl', DAEMON_URL_DEFAULT);
  if (await isDaemonUp(url)) return true;
  const binary = resolveBinary(cfg);
  const { host, port } = parseDaemonHostPort(url);
  // The daemon reads these at first connect (module-global singleton). The RAM
  // window must cover the target's SRAM so the RTT control-block scan finds it;
  // defaults suit HC32L19x (0x20000000, 0x20000). Override via rtt-mcp.ramStart /
  // rtt-mcp.ramSize for a different MCU (e.g. STM32F103: 0x20000000 / 0x5000).
  const env = {
    ...process.env,
    JLINK_DEVICE: cfg.get<string>('device', 'Cortex-M0+'),
    JLINK_SPEED: String(cfg.get<number>('speed', 4000)),
    JLINK_RAM_START: cfg.get<string>('ramStart', '0x20000000'),
    JLINK_RAM_SIZE: cfg.get<string>('ramSize', '0x20000'),
    RTT_CHANNEL: String(cfg.get<number>('channel', 0)),
  };
  daemonProc = spawn(binary, ['daemon', '-host', host, '-port', String(port)], {
    env,
    windowsHide: true,
    stdio: 'ignore',
    detached: false,
  });
  daemonProc.on('exit', () => { daemonProc = null; });
  for (let i = 0; i < 50; i++) {
    await new Promise((resolve) => setTimeout(resolve, 200));
    if (await isDaemonUp(url)) return true;
  }
  return false;
}

async function stopDaemon(): Promise<void> {
  if (!daemonProc) return;
  // Ask the daemon to release the J-Link and exit cleanly. This is the supported
  // teardown path: on Windows, child.kill() is TerminateProcess, a hard kill that
  // bypasses Python cleanup and would strand the probe with RTT started. Try the
  // graceful POST first (short timeout), then fall back to kill().
  const cfg = vscode.workspace.getConfiguration('rtt-mcp');
  const sseUrl = cfg.get<string>('daemonUrl', DAEMON_URL_DEFAULT);
  const base = sseUrl.replace(/\/sse\/?$/, '');
  const headers: Record<string, string> = {};
  const token = process.env.RTT_AUTH_TOKEN;
  if (token) headers['Authorization'] = `Bearer ${token}`;
  try {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), 2000);
    await fetch(`${base}/shutdown`, { method: 'POST', headers, signal: ctrl.signal });
    clearTimeout(timer);
  } catch {
    /* daemon may already be down; fall through to kill */
  }
  // Give the graceful shutdown a moment to complete before forcing.
  for (let i = 0; i < 10 && daemonProc; i++) {
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  if (daemonProc) {
    try { daemonProc.kill(); } catch { /* ignore */ }
    daemonProc = null;
  }
}

async function showMenu(): Promise<void> {
  const items: (vscode.QuickPickItem & { cmd?: string })[] = [
    { label: '$(plug) Connect to J-Link', description: 'Start service + open J-Link + stream RTT', cmd: 'rtt-mcp.connect' },
    { label: '$(debug-disconnect) Disconnect', description: 'Close J-Link (service stays warm)', cmd: 'rtt-mcp.disconnect' },
    { label: '$(arrow-down) Read Accumulated Data', description: 'Pull current RTT buffer', cmd: 'rtt-mcp.read' },
    { label: '$(arrow-up) Write to Down-buffer', description: 'Send a string to the device', cmd: 'rtt-mcp.write' },
    { label: '$(eye) Toggle Continuous Monitor', description: 'Stream RTT data to output channel', cmd: 'rtt-mcp.toggleMonitor' },
    { label: '$(list-unordered) List J-Link Devices', cmd: 'rtt-mcp.listDevices' },
    { label: '$(info) Show Status', cmd: 'rtt-mcp.status' },
    { label: '$(trash) Clear Ring Buffer', cmd: 'rtt-mcp.clear' },
    { label: '$(output) Show RTT Output', cmd: 'rtt-mcp.openOutput' },
    { label: '$(file-text) Open RTT Log File', cmd: 'rtt-mcp.openLog' },
    { label: '$(refresh) Reset Extension', description: 'Recreate status bar + provider (recovery)', cmd: 'rtt-mcp.reset' },
    { label: '$(chip) Set Target Device', description: `Current: ${provider.deviceName}`, cmd: 'rtt-mcp.setDevice' },
  ];
  const pick = await vscode.window.showQuickPick(items, { title: 'RTT MCP' });
  if (pick?.cmd) {
    await vscode.commands.executeCommand(pick.cmd);
  }
}

async function connectCmd(): Promise<void> {
  output.show(true);
  await vscode.window.withProgress(
    { location: vscode.ProgressLocation.Notification, title: 'RTT', cancellable: false },
    async (progress) => {
      progress.report({ message: 'Starting service...' });
      const up = await ensureDaemon();
      if (!up) {
        const cfg = vscode.workspace.getConfiguration('rtt-mcp');
        const url = cfg.get<string>('daemonUrl', DAEMON_URL_DEFAULT);
        const fail = `RTT service failed to start at ${url}`;
        output.appendLine(`[${ts()}] [RTT ERROR] ${fail}`);
        vscode.window.showErrorMessage(fail);
        return;
      }
      progress.report({ message: 'Connecting to J-Link...' });
      try {
        const result = await provider.connect({
          onStatus: (msg) => {
            output.appendLine(`[${ts()}] [RTT] ${msg}`);
            progress.report({ message: msg });
          },
        });
        const text = (result.content ?? [])
          .filter((c) => c.type === 'text')
          .map((c) => c.text)
          .join('\n');
        output.appendLine(`[${ts()}] [RTT] ${text}`);

        // Auto-start monitor after successful connect
        provider.startMonitor({
          onData: (data) => output.append(data),
          onError: (err) => output.appendLine(`[${ts()}] [RTT Monitor Error] ${err.message}`),
        });
        output.appendLine(`[${ts()}] [RTT] Monitor started`);
        vscode.window.showInformationMessage(text || 'RTT connected & monitoring');
      } catch (e) {
        const msg = e instanceof Error ? e.message : String(e);
        output.appendLine(`[${ts()}] [RTT ERROR] ${msg}`);
        vscode.window.showErrorMessage(`RTT connect failed: ${msg}`);
      }
    },
  );
  updateStatusBar();
}

async function disconnectCmd(): Promise<void> {
  await provider.disconnect();
  output.appendLine(`[${ts()}] [RTT] Disconnected`);
  vscode.window.showInformationMessage('RTT disconnected');
  updateStatusBar();
}

async function readCmd(): Promise<void> {
  if (!provider.isConnected) {
    vscode.window.showWarningMessage('RTT not connected. Run "RTT: Connect to J-Link" first.');
    return;
  }
  try {
    const text = await provider.read();
    output.append(text);
    output.show(true);
  } catch (e) {
    vscode.window.showErrorMessage(`RTT read failed: ${(e as Error).message}`);
  }
}

async function writeCmd(): Promise<void> {
  if (!provider.isConnected) {
    vscode.window.showWarningMessage('RTT not connected. Run "RTT: Connect to J-Link" first.');
    return;
  }
  const data = await vscode.window.showInputBox({
    prompt: 'String to send to RTT down-buffer (use \\r\\n for line endings)',
    placeHolder: 'e.g. status\\r\\n',
  });
  if (!data) return;
  try {
    const result = await provider.write(data);
    vscode.window.showInformationMessage(result);
    output.appendLine(`[${ts()}] [RTT TX] ${data}`);
  } catch (e) {
    vscode.window.showErrorMessage(`RTT write failed: ${(e as Error).message}`);
  }
}

async function listDevicesCmd(): Promise<void> {
  if (!provider.isConnected) {
    const choice = await vscode.window.showInformationMessage(
      'Not connected. Connect first?',
      'Connect', 'Cancel',
    );
    if (choice !== 'Connect') return;
    await connectCmd();
    if (!provider.isConnected) return;
  }
  try {
    const text = await provider.listDevices();
    output.appendLine(`[${ts()}] [RTT] ${text}`);
    output.show(true);
  } catch (e) {
    vscode.window.showErrorMessage(`RTT list devices failed: ${(e as Error).message}`);
  }
}

async function statusCmd(): Promise<void> {
  if (!provider.isConnected) {
    vscode.window.showInformationMessage('RTT not connected');
    return;
  }
  try {
    const text = await provider.status();
    output.appendLine(`[${ts()}] [RTT Status]\n${text}`);
    output.show(true);
  } catch (e) {
    vscode.window.showErrorMessage(`RTT status failed: ${(e as Error).message}`);
  }
}

async function clearCmd(): Promise<void> {
  if (!provider.isConnected) {
    vscode.window.showWarningMessage('RTT not connected.');
    return;
  }
  await provider.clear();
  output.appendLine(`[${ts()}] [RTT] Buffer cleared`);
}

async function toggleMonitorCmd(): Promise<void> {
  if (provider.isMonitoring) {
    provider.stopMonitor();
    output.appendLine(`[${ts()}] [RTT] Monitor stopped`);
    vscode.window.showInformationMessage('RTT monitor stopped');
  } else {
    if (!provider.isConnected) {
      vscode.window.showWarningMessage('RTT not connected. Run "RTT: Connect to J-Link" first.');
      return;
    }
    provider.startMonitor({
      onData: (text) => output.append(text),
      onError: (err) => output.appendLine(`[${ts()}] [RTT Monitor Error] ${err.message}`),
    });
    output.appendLine(`[${ts()}] [RTT] Monitor started`);
    output.show(true);
    vscode.window.showInformationMessage('RTT monitor started - output streaming to "RTT" channel');
  }
  updateStatusBar();
}

async function openLogCmd(): Promise<void> {
  const logPath = provider.logFilePath;
  if (!fs.existsSync(logPath)) {
    vscode.window.showWarningMessage(`RTT log file not found: ${logPath}`);
    return;
  }
  const doc = await vscode.workspace.openTextDocument(logPath);
  await vscode.window.showTextDocument(doc, { preview: false });
}

async function resetCmd(): Promise<void> {
  await provider.shutdown();
  statusBar.dispose();
  statusBar = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 100);
  statusBar.command = 'rtt-mcp.showMenu';
  statusBar.show();
  const cfg = vscode.workspace.getConfiguration('rtt-mcp');
  provider = new RttProvider(
    resolveBinary(cfg),
    cfg.get<string[]>('serverArgs', ['bridge']),
    '',
    cfg.get<number>('pollIntervalMs', 300),
    cfg.get<string>('device', 'Cortex-M0+'),
    cfg.get<number>('speed', 4000),
    logFileDefault,
  );
  updateStatusBar();
  output.appendLine(`[${ts()}] [RTT] Extension reset`);
  vscode.window.showInformationMessage('RTT extension reset');
}

async function setDeviceCmd(): Promise<void> {
  const cfg = vscode.workspace.getConfiguration('rtt-mcp');
  const current = cfg.get<string>('device', 'Cortex-M0+');
  output.show(true);

  // Load the live J-Link device database (probe-less; enumerates every device,
  // so ~1-2s). Needs the shared daemon up — start it if necessary. Falls back to
  // generic cores + manual entry if the list can't be loaded.
  let devices: string[] = [];
  try {
    if (await ensureDaemon()) {
      devices = await vscode.window.withProgress(
        { location: vscode.ProgressLocation.Notification, title: 'RTT: loading J-Link device list…', cancellable: false },
        () => provider.listSupportedDevices(),
      );
    }
  } catch (e) {
    output.appendLine(`[${ts()}] [RTT] Could not load device list: ${(e as Error).message}`);
  }

  // Generic ARM cores are always valid in J-Link and suffice for RTT (SWD+RAM).
  const cores: vscode.QuickPickItem[] = [
    { label: 'Cortex-M0+', description: 'generic core (always valid)' },
    { label: 'Cortex-M0', description: 'generic core (always valid)' },
    { label: 'Cortex-M3', description: 'generic core (always valid)' },
    { label: 'Cortex-M4', description: 'generic core (always valid)' },
    { label: 'Cortex-M7', description: 'generic core (always valid)' },
    { label: 'Cortex-M33', description: 'generic core (always valid)' },
  ];
  const all: vscode.QuickPickItem[] = [...cores, ...devices.map((d) => ({ label: d }))];
  const customItem: vscode.QuickPickItem & { custom?: boolean } = {
    label: '$(edit) Custom device name…',
    description: 'type an exact J-Link name',
    custom: true,
  };

  // createQuickPick with a cap: rendering all ~14k items at once is laggy, so we
  // show the first MAX and re-filter client-side as the user types.
  const qp = vscode.window.createQuickPick();
  qp.title = devices.length ? `Set Target Device · ${devices.length} J-Link devices` : 'Set Target Device';
  qp.placeholder = `Current: ${current} — type to search${devices.length ? ` ${devices.length} devices` : ''}`;
  qp.matchOnDescription = false;
  qp.matchOnDetail = false;
  qp.ignoreFocusOut = true;
  const MAX = 500;
  const render = (q: string): void => {
    const ql = q.trim().toLowerCase();
    const base = ql
      ? all.filter((it) => it.label.toLowerCase().includes(ql)).slice(0, MAX)
      : all.slice(0, MAX);
    qp.items = [...base, customItem];
  };
  qp.onDidChangeValue(render);
  render('');

  const picked = await new Promise<(vscode.QuickPickItem & { custom?: boolean }) | undefined>((resolve) => {
    qp.onDidAccept(() => resolve(qp.activeItems[0] as (vscode.QuickPickItem & { custom?: boolean }) | undefined));
    qp.onDidHide(() => resolve(undefined));
    qp.show();
  });
  qp.dispose();
  if (!picked) return;

  let dev: string;
  if (picked.custom) {
    const typed = await vscode.window.showInputBox({
      title: 'Custom Target Device',
      prompt: 'Exact device name as in J-Link (search the list above if unsure).',
      value: current,
      placeHolder: 'e.g. STM32F407VE',
      validateInput: (v) => {
        const t = v.trim();
        if (!t) return 'Device name cannot be empty';
        if (devices.length > 0 && !devices.includes(t)) {
          return `'${t}' is not in the loaded J-Link list — check exact spelling/case.`;
        }
        return undefined;
      },
    });
    if (!typed) return;
    dev = typed.trim();
  } else {
    dev = picked.label;
  }

  if (dev === current) return;
  await cfg.update('device', dev, vscode.ConfigurationTarget.Workspace);
  vscode.window.showInformationMessage(`RTT target device set to '${dev}'. Reconnect to apply.`);
  output.appendLine(`[${ts()}] [RTT] Target device changed: ${current} → ${dev}`);
}

function updateStatusBar(): void {
  if (!statusBar) return;
  statusBar.backgroundColor = undefined;
  if (!provider.isConnected) {
    statusBar.text = '$(debug-disconnect) RTT';
    statusBar.tooltip = 'RTT MCP - Disconnected (click to open menu)';
  } else if (provider.isMonitoring) {
    statusBar.text = '$(eye) RTT ●';
    statusBar.tooltip = 'RTT MCP - Connected & monitoring (click to open menu)';
  } else {
    statusBar.text = '$(debug-start) RTT';
    statusBar.tooltip = 'RTT MCP - Connected (click to open menu)';
  }
}
