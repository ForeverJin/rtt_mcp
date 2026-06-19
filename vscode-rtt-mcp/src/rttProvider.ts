/**
 * High-level RTT operations on top of McpClient.
 *
 * Owns the lifecycle of one MCP server process and exposes the 7 tools
 * exposed by mcp-rtt-server, plus a continuous monitor mode that polls
 * `rtt_read` and forwards data to a callback.
 */

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
    private readonly logFile: string,
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
    return this.logFile;
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

  /**
   * Fetch device names from the J-Link device database via the shared daemon
   * (probe-less — works before connecting to any target). Uses a short-lived
   * bridge client of its own so it neither requires nor disturbs an existing
   * J-Link connection. `query` optionally filters server-side (substring).
   * Returns [] if the daemon/list is unavailable.
   */
  async listSupportedDevices(query?: string): Promise<string[]> {
    const client = new McpClient(this.command, this.args, this.cwd);
    try {
      await client.start();
      const args: Record<string, unknown> = {};
      if (query) args.query = query;
      const result = await client.callTool('rtt_list_supported_devices', args);
      return parseDeviceList(extractText(result));
    } finally {
      await client.stop();
    }
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
    // Stream from the start of this connection's broadcast log (the server
    // truncates it on connect, so offset 0 == only data seen this session).
    this.monitorOffset = 0;
    // Poll SERIALLY: schedule the next tick only after the current round-trip
    // (bridge -> daemon -> readRaw) resolves. setInterval would fire overlapping
    // ticks whenever a poll takes longer than the interval, and both would read
    // the same offset — duplicating every line.
    const poll = async (): Promise<void> => {
      if (!this._monitoring || !this.isConnected) return;
      try {
        await this.tickMonitor(events);
      } catch (e) {
        events.onError(e instanceof Error ? e : new Error(String(e)));
      }
      if (this._monitoring) {
        this.monitorTimer = setTimeout(() => { void poll(); }, this.pollIntervalMs);
      }
    };
    this.monitorTimer = setTimeout(() => { void poll(); }, 0);
  }

  stopMonitor(): void {
    if (this.monitorTimer) {
      clearTimeout(this.monitorTimer);
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
    // Trust the server's cursor unconditionally: on progress it advances; when
    // the device is quiet it holds (next == current); after a log rotation it
    // resumes from the post-rotation position (next < current, the server having
    // replayed from 0). The previous code reset to 0 whenever next <= current,
    // which on every idle poll made the next active poll re-stream the ENTIRE log
    // from the start — every line duplicated after any pause in device output.
    this.monitorOffset = nextOffset;
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

/**
 * Parse the rtt_list_supported_devices text payload into device names. The first
 * line is a "N device(s)" header and a trailing "... truncated" footer may be
 * present; both are dropped, leaving the bare device names.
 */
function parseDeviceList(text: string): string[] {
  return text
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l.length > 0 && !/\d+\s+device\(s\)/.test(l) && !l.startsWith('...'));
}
