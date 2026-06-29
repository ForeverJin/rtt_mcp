/**
 * Read-only tail of the daemon's broadcast RTT log (rtt_output.log).
 *
 * The Go daemon writes every RTT line to a rotating broadcast log that any
 * number of consumers may read without interfering with each other — it is the
 * same file `rtt_read_raw` serves. RttLogTail polls that file and forwards new
 * bytes to a callback, crucially WITHOUT touching the J-Link or spawning an
 * MCP bridge, so it never competes with Claude/agent for probe ownership. This
 * is what lets the RTT panel light up automatically when an agent starts
 * reading, with no manual "Connect to J-Link" and no J-Link contention.
 *
 * Polling is serial (setTimeout self-scheduled, not setInterval) to avoid
 * overlapping ticks reading the same offset and duplicating lines — mirroring
 * RttProvider.tickMonitor.
 */
import * as fs from 'fs';

export interface RttLogTailEvents {
  /** New bytes appended to the log since the last tick. */
  onData: (text: string) => void;
  /** Optional, fired once the first time data arrives after start(). */
  onFirstData?: () => void;
  /** Optional, fired when the stream goes active (data flowing) or idle. */
  onStreamState?: (streaming: boolean) => void;
  /** Optional, fired on unexpected read/stat errors (file missing is NOT one). */
  onError?: (err: Error) => void;
}

export class RttLogTail {
  private timer: NodeJS.Timeout | null = null;
  // Marks the status bar "streaming" for a grace period after the last data;
  // cleared by idleTimeout so the bar settles back to "armed" when quiet.
  private idleTimer: NodeJS.Timeout | null = null;
  private offset = 0;
  private running = false;
  private sawData = false;

  constructor(
    private readonly file: string,
    private readonly intervalMs: number,
    private readonly events: RttLogTailEvents,
  ) {}

  get isRunning(): boolean {
    return this.running;
  }

  /** Begin polling. Starts at the current end of the file (no history replay). */
  start(): void {
    if (this.running) return;
    this.running = true;
    this.sawData = false;
    // Start from the current size so we stream only data written after start
    // (the agent activity that follows), not the whole accumulated log.
    try {
      this.offset = fs.statSync(this.file).size;
    } catch {
      // File missing (daemon not up yet) — start at 0; we'll pick up the first
      // bytes once the daemon creates it.
      this.offset = 0;
    }
    this.schedule(0);
  }

  stop(): void {
    this.running = false;
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    if (this.idleTimer) {
      clearTimeout(this.idleTimer);
      this.idleTimer = null;
    }
  }

  private schedule(delay: number): void {
    if (!this.running) return;
    this.timer = setTimeout(() => { void this.tick(); }, delay);
  }

  private tick(): void {
    if (!this.running) return;
    try {
      const st = fs.statSync(this.file);
      // The Go server rotates by truncating the log at 1 MiB; if the file
      // shrank below our cursor, it rotated — restart from 0 (matches the
      // rtt_read_raw rotate semantics).
      if (st.size < this.offset) this.offset = 0;
      if (st.size > this.offset) {
        const len = st.size - this.offset;
        const buf = Buffer.allocUnsafe(len);
        const fd = fs.openSync(this.file, 'r');
        let bytesRead: number;
        try {
          // readSync with an explicit position reads at the offset without
          // disturbing any shared read pointer; the return value is the number
          // of bytes actually read (may be < len if the file is concurrently
          // truncated).
          bytesRead = fs.readSync(fd, buf, 0, len, this.offset);
        } finally {
          fs.closeSync(fd);
        }
        this.offset += bytesRead;
        if (bytesRead > 0) {
          this.events.onData(buf.slice(0, bytesRead).toString('utf-8'));
          if (!this.sawData) {
            this.sawData = true;
            this.events.onFirstData?.();
          }
          // Mark streaming and (re)arm an idle timer so the status bar shows
          // "active" only while data is actually flowing, not merely armed.
          this.events.onStreamState?.(true);
          if (this.idleTimer) clearTimeout(this.idleTimer);
          this.idleTimer = setTimeout(() => {
            this.idleTimer = null;
            this.events.onStreamState?.(false);
          }, 2000);
        }
      }
    } catch (e) {
      // A missing file while the daemon is down is the normal idle state, not
      // an error worth surfacing; only forward unexpected errors.
      const code = (e as NodeJS.ErrnoException).code;
      if (code !== 'ENOENT') {
        this.events.onError?.(e instanceof Error ? e : new Error(String(e)));
      }
    }
    this.schedule(this.intervalMs);
  }
}
