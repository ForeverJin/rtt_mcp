/**
 * Minimal MCP client over stdio for the rtt-mcp server.
 *
 * Implements the subset of the Model Context Protocol needed for `tools/call`:
 *   - initialize / notifications/initialized
 *   - tools/list
 *   - tools/call
 *
 * Wire format: line-delimited JSON (one JSON-RPC message per line, terminated by \n).
 * This matches the actual MCP Python SDK (mcp.server.stdio.stdin_reader reads `async for line in stdin`).
 * NOTE: this is NOT the LSP-style `Content-Length: N\r\n\r\n{json}` framing - the
 * Python SDK does not implement that despite the spec mentioning it.
 */

import { ChildProcess, spawn } from 'child_process';

export interface McpTool {
  name: string;
  description?: string;
  inputSchema: any;
}

export interface McpTextContent {
  type: 'text';
  text: string;
}

export interface McpCallResult {
  content: McpTextContent[];
  isError?: boolean;
}

interface JsonRpcIdMessage {
  jsonrpc: '2.0';
  id: number;
  result?: unknown;
  error?: { code: number; message: string; data?: unknown };
}

interface PendingRequest {
  resolve: (value: unknown) => void;
  reject: (reason: Error) => void;
}

export class McpClient {
  private proc: ChildProcess | null = null;
  private nextId = 1;
  private pending = new Map<number, PendingRequest>();
  private buffer = Buffer.alloc(0);
  private readonly maxBufferSize = 1 * 1024 * 1024; // 1MB max line buffer
  private _initialized = false;
  private stderrTail: string[] = [];
  private readonly maxStderrTail = 50;

  constructor(
    private readonly command: string,
    private readonly args: string[],
    private readonly cwd: string,
    /**
     * Extra environment variables merged into the child process env. The child
     * always inherits process.env as a base; this object overrides / adds keys.
     * Used by the integration test to inject RTT_MOCK=1 without leaking to the
     * test runner's own process.env.
     */
    private readonly env: NodeJS.ProcessEnv = {},
  ) {}

  get initialized(): boolean {
    return this._initialized;
  }

  get stderrLines(): readonly string[] {
    return this.stderrTail;
  }

  async start(timeoutMs = 15000): Promise<void> {
    if (this.proc) {
      throw new Error('MCP client already started');
    }

    this.proc = spawn(this.command, this.args, {
      cwd: this.cwd || undefined,
      env: { ...process.env, ...this.env },
      stdio: ['pipe', 'pipe', 'pipe'],
      windowsHide: true,
      windowsVerbatimArguments: false,
    });

    const stdout = this.proc.stdout;
    const stderr = this.proc.stderr;
    if (!stdout || !stderr || !this.proc.stdin) {
      throw new Error('Failed to obtain stdio streams from child process');
    }

    stdout.setEncoding('utf-8');
    stdout.on('data', (chunk: string) => this.handleData(Buffer.from(chunk, 'utf-8')));

    stderr.setEncoding('utf-8');
    stderr.on('data', (chunk: string) => this.handleStderr(chunk));

    this.proc.on('exit', (code, signal) => {
      this.proc = null;
      this._initialized = false;
      const err = new Error(`MCP server exited (code=${code}, signal=${signal})`);
      for (const [, p] of this.pending) p.reject(err);
      this.pending.clear();
    });

    this.proc.on('error', (err) => {
      for (const [, p] of this.pending) p.reject(err);
      this.pending.clear();
    });

    await this.request('initialize', {
      protocolVersion: '2024-11-05',
      capabilities: {},
      clientInfo: { name: 'vscode-rtt-mcp', version: '0.1.0' },
    }, timeoutMs);

    this.notify('notifications/initialized', {});
    this._initialized = true;
  }

  async listTools(timeoutMs = 10000): Promise<McpTool[]> {
    const result = (await this.request('tools/list', {}, timeoutMs)) as { tools: McpTool[] };
    return result.tools ?? [];
  }

  async callTool(name: string, args: Record<string, unknown>, timeoutMs = 30000): Promise<McpCallResult> {
    const result = (await this.request('tools/call', { name, arguments: args }, timeoutMs)) as McpCallResult;
    return result;
  }

  async stop(): Promise<void> {
    const proc = this.proc;
    this.proc = null;
    this._initialized = false;
    for (const [, p] of this.pending) p.reject(new Error('MCP client stopped'));
    this.pending.clear();
    if (!proc) return;
    try {
      proc.stdin?.end();
    } catch {
      /* ignore */
    }
    await new Promise<void>((resolve) => {
      let settled = false;
      const done = (): void => { if (!settled) { settled = true; resolve(); } };
      proc.once('exit', done);
      try {
        proc.kill();
      } catch {
        done();
      }
      setTimeout(done, 1000);
    });
    // Remove listeners to allow GC of the process object
    proc.removeAllListeners();
  }

  private handleData(chunk: Buffer): void {
    this.buffer = Buffer.concat([this.buffer, chunk]);
    // Drop data if buffer exceeds max size (server sent a line > 1MB)
    if (this.buffer.length > this.maxBufferSize) {
      const dropTo = this.buffer.indexOf(0x0a);
      if (dropTo >= 0) {
        this.buffer = this.buffer.slice(dropTo + 1);
      } else {
        this.buffer = Buffer.alloc(0);
        return;
      }
    }
    let newlineIdx = this.buffer.indexOf(0x0a);
    while (newlineIdx >= 0) {
      const lineBytes = this.buffer.slice(0, newlineIdx);
      this.buffer = this.buffer.slice(newlineIdx + 1);
      newlineIdx = this.buffer.indexOf(0x0a);
      if (lineBytes.length === 0) continue;
      const line = lineBytes.toString('utf-8').replace(/\r$/, '').trim();
      if (!line) continue;
      this.dispatch(line);
    }
  }

  private dispatch(line: string): void {
    let msg: JsonRpcIdMessage;
    try {
      msg = JSON.parse(line) as JsonRpcIdMessage;
    } catch {
      return;
    }
    if (typeof msg.id !== 'number') return;
    const pending = this.pending.get(msg.id);
    if (!pending) return;
    this.pending.delete(msg.id);
    if (msg.error) {
      pending.reject(new Error(`${msg.error.message} (code=${msg.error.code})`));
    } else {
      pending.resolve(msg.result);
    }
  }

  private handleStderr(chunk: string): void {
    for (const line of chunk.split(/\r?\n/)) {
      if (!line) continue;
      this.stderrTail.push(line);
      if (this.stderrTail.length > this.maxStderrTail) this.stderrTail.shift();
    }
  }

  private request(method: string, params: unknown, timeoutMs = 15000): Promise<unknown> {
    return new Promise<unknown>((resolve, reject) => {
      if (!this.proc || !this.proc.stdin) {
        reject(new Error('MCP client not running'));
        return;
      }
      const id = this.nextId++;
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`MCP request '${method}' timed out after ${timeoutMs}ms${this.diagContext()}`));
      }, timeoutMs);
      this.pending.set(id, {
        resolve: (v) => { clearTimeout(timer); resolve(v); },
        reject: (e) => { clearTimeout(timer); reject(e); },
      });
      const payload = JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n';
      try {
        this.proc.stdin.write(payload);
      } catch (e) {
        clearTimeout(timer);
        this.pending.delete(id);
        reject(e instanceof Error ? e : new Error(String(e)));
      }
    });
  }

  private notify(method: string, params: unknown): void {
    if (!this.proc || !this.proc.stdin) return;
    const payload = JSON.stringify({ jsonrpc: '2.0', method, params }) + '\n';
    try {
      this.proc.stdin.write(payload);
    } catch {
      /* ignore */
    }
  }

  private diagContext(): string {
    const lines: string[] = [];
    const recent = this.stderrTail.slice(-10);
    if (recent.length === 0) {
      lines.push('Server produced no stderr output (may be hanging silently).');
    } else {
      lines.push(`Server stderr (last ${recent.length} lines):`);
      for (const l of recent) lines.push(`  | ${l}`);
    }
    if (this.proc) {
      lines.push(`Process: pid=${this.proc.pid ?? '?'} exitCode=${this.proc.exitCode ?? 'still running'}`);
    }
    return '\n  ' + lines.join('\n  ');
  }
}
