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
  /** Optional progress callback invoked at key connect steps. */
  onStatus?: (msg: string) => void;
}

export interface RttProviderEvents {
  onData: (text: string) => void;
  onError: (err: Error) => void;
}

export class RttProvider {
  private client: McpClient | null = null;
  private monitorTimer: NodeJS.Timeout | null = null;
  private _monitoring = false;
  // Byte offset into the broadcast log for the next rtt_read_raw poll. Reset to 0
  // when the log rotates (server reports next_offset <= this value).
  private monitorOffset = 0;

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
    opts.onStatus?.('Starting MCP server...');
    const client = new McpClient(this.command, this.args, this.cwd);
    await client.start();
    this.client = client;
    opts.onStatus?.(`Connecting to J-Link (${opts.device ?? this.device})...`);
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

  /**
   * Read new bytes from the non-draining broadcast log starting at `offset`.
   * Returns the raw server JSON ({"data","next_offset"}); the caller advances a
   * cursor. Used by the continuous monitor so it coexists with rtt_read consumers.
   */
  async readLogRaw(offset: number, maxBytes = 8192): Promise<string> {
    const result = await this.callTool('rtt_read_raw', { offset, max_bytes: maxBytes });
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
    // Start from the current end of the broadcast log so we only stream NEW data.
    this.monitorOffset = 0;
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
    // Poll the NON-draining broadcast log via rtt_read_raw, so the continuous
    // monitor never steals bytes that an on-demand `rtt_read` (or another client)
    // would otherwise see. The server tracks no per-client cursor, so we dedup by
    // advancing monitorOffset through the returned next_offset.
    const payload = await this.readLogRaw(this.monitorOffset, 8192);
    if (!payload) return;
    let nextOffset = this.monitorOffset;
    let data = '';
    try {
      const parsed = JSON.parse(payload);
      data = typeof parsed.data === 'string' ? parsed.data : '';
      nextOffset = typeof parsed.next_offset === 'number' ? parsed.next_offset : this.monitorOffset;
    } catch {
      return;
    }
    // Rotation / no progress: server resets cursor to <= current; clamp so we never
    // feed stale content on the next poll.
    if (nextOffset <= this.monitorOffset) {
      this.monitorOffset = 0;
    } else {
      this.monitorOffset = nextOffset;
    }
    if (data) events.onData(data);
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
