/**
 * High-level RTT operations on top of McpClient.
 *
 * Owns the lifecycle of one MCP server process and exposes the 7 tools
 * exposed by mcp-rtt-server, plus a continuous monitor mode that polls
 * `rtt_read` and forwards data to a callback.
 */

import * as path from 'path';
import { McpCallResult, McpClient } from './mcpClient';

export interface RttConnectOptions {
  serial?: string;
  device?: string;
  speed?: number;
}

export interface RttProviderEvents {
  onData: (text: string) => void;
  onError: (err: Error) => void;
}

export class RttProvider {
  private client: McpClient | null = null;
  private monitorTimer: NodeJS.Timeout | null = null;
  private _monitoring = false;

  constructor(
    private readonly command: string,
    private readonly args: string[],
    private readonly cwd: string,
    private readonly pollIntervalMs: number,
    private readonly device: string,
    private readonly speed: number,
  ) {}

  get isConnected(): boolean {
    return this.client !== null && this.client.initialized;
  }

  get isMonitoring(): boolean {
    return this._monitoring;
  }

  get deviceName(): string {
    return this.device;
  }

  get logFilePath(): string {
    return path.join(this.cwd, 'rtt_output.log');
  }

  get serverStderr(): readonly string[] {
    return this.client?.stderrLines ?? [];
  }

  async connect(opts: RttConnectOptions = {}): Promise<McpCallResult> {
    await this.shutdown();
    const client = new McpClient(this.command, this.args, this.cwd);
    await client.start();
    this.client = client;
    return await this.callTool('jlink_connect', {
      serial: opts.serial,
      device: opts.device ?? this.device,
      speed: opts.speed ?? this.speed,
    });
  }

  async disconnect(): Promise<void> {
    this.stopMonitor();
    if (!this.client) return;
    try {
      await this.client.callTool('jlink_disconnect', {});
    } catch {
      /* ignore */
    }
    await this.shutdown();
  }

  async read(maxBytes = 4096): Promise<string> {
    const result = await this.callTool('rtt_read', { channel: 0, max_bytes: maxBytes });
    return extractText(result);
  }

  async write(data: string, channel = 0): Promise<string> {
    const result = await this.callTool('rtt_write', { channel, data });
    return extractText(result);
  }

  async listDevices(): Promise<string> {
    const result = await this.callTool('rtt_list_devices', {});
    return extractText(result);
  }

  async status(): Promise<string> {
    const result = await this.callTool('jlink_status', {});
    return extractText(result);
  }

  async clear(): Promise<void> {
    await this.callTool('rtt_clear', {});
  }

  startMonitor(events: RttProviderEvents): void {
    this.stopMonitor();
    if (!this.isConnected) return;
    this._monitoring = true;
    this.monitorTimer = setInterval(() => {
      this.tickMonitor(events).catch((e) => {
        events.onError(e instanceof Error ? e : new Error(String(e)));
      });
    }, this.pollIntervalMs);
  }

  stopMonitor(): void {
    if (this.monitorTimer) {
      clearInterval(this.monitorTimer);
      this.monitorTimer = null;
    }
    this._monitoring = false;
  }

  async shutdown(): Promise<void> {
    this.stopMonitor();
    if (this.client) {
      const c = this.client;
      this.client = null;
      await c.stop();
    }
  }

  private async tickMonitor(events: RttProviderEvents): Promise<void> {
    if (!this.isConnected) return;
    const text = await this.read(4096);
    if (!text) return;
    if (text.includes('(no RTT data)')) return;
    const cleaned = stripDiagnostics(text);
    if (cleaned) events.onData(cleaned);
  }

  private async callTool(name: string, args: Record<string, unknown>): Promise<McpCallResult> {
    if (!this.client) throw new Error('RTT not connected');
    return await this.client.callTool(name, args);
  }
}

function extractText(result: McpCallResult): string {
  const parts = (result.content ?? [])
    .filter((c) => c.type === 'text')
    .map((c) => c.text);
  return parts.join('\n');
}

/**
 * The MCP server's `rtt_read` tool returns diagnostics + ring buffer data
 * prefixed with `[RTT Diagnostics]` / `[RTT Data]`. Extract just the data.
 */
function stripDiagnostics(text: string): string {
  const idx = text.indexOf('[RTT Data]');
  if (idx < 0) return text.trim();
  return text.slice(idx + '[RTT Data]'.length).trim();
}
