"""J-Link RTT wrapper with background monitor thread."""

from __future__ import annotations

import os
import sys
import threading
import time
from collections import deque
from dataclasses import dataclass
from datetime import datetime
from typing import Optional

import pylink


def _default_log_file() -> str:
    """Resolve the RTT log file path.

    Priority:
      1. ``RTT_LOG_FILE`` env var (explicit override, any path).
      2. A portable per-user default under the OS state dir:
         - Windows: ``%LOCALAPPDATA%\\mcp-rtt-server\\rtt_output.log``
         - Linux/others: ``$XDG_STATE_HOME/mcp-rtt-server/rtt_output.log``
           (falls back to ``~/.local/state/...`` when XDG is unset).
      3. ``tempfile.gettempdir()`` as a last resort.

    The directory is created on demand. This must NOT derive from ``__file__``:
    a normal ``pip install`` places the package in site-packages, whose parent
    is not user-writable, and the log open() at connect() would crash.
    """
    env = os.environ.get("RTT_LOG_FILE")
    if env:
        return os.path.abspath(env)

    # Portable user state directory (POSIX + Windows).
    local_appdata = os.environ.get("LOCALAPPDATA")  # Windows
    xdg_state = os.environ.get("XDG_STATE_HOME")    # Linux (if set)
    if local_appdata:
        base = os.path.join(local_appdata, "mcp-rtt-server")
    elif xdg_state:
        base = os.path.join(xdg_state, "mcp-rtt-server")
    else:
        # ~/.local/state on Linux; elsewhere fall back to temp.
        home = os.path.expanduser("~")
        if home and home != "~":
            base = os.path.join(home, ".local", "state", "mcp-rtt-server")
        else:
            import tempfile
            return os.path.join(tempfile.gettempdir(), "mcp-rtt-server-rtt_output.log")

    try:
        os.makedirs(base, exist_ok=True)
    except OSError:
        import tempfile
        return os.path.join(tempfile.gettempdir(), "mcp-rtt-server-rtt_output.log")
    return os.path.join(base, "rtt_output.log")


@dataclass
class RTTStatus:
    """RTT connection status."""
    connected: bool
    rtt_started: bool
    device_name: str
    speed: int
    serial: Optional[str]
    channel: int
    ring_buffer_size: int


class JLinkRTT:
    """J-Link RTT wrapper with background monitor thread.

    This class wraps pylink to provide RTT read/write functionality
    with a background thread that continuously monitors RTT and writes
    data to a log file while storing it in a ring buffer.
    """

    # Log file path for RTT output. Computed once via _default_log_file() below:
    # env override wins, else a portable user-writable default (LOCALAPPDATA on
    # Windows, XDG_STATE_HOME on Linux, temp dir as last resort). Resolves to a
    # writable location whether the package is installed editable or normally
    # (a normal install puts us in site-packages, whose parent is NOT writable —
    # the old "walk up 3 dirs from __file__" form crashed the daemon on connect).
    RTT_LOG_FILE = None  # set in __init__ via _default_log_file()
    # Max log file size in bytes (default: 1MB)
    LOG_MAX_SIZE = 1 * 1024 * 1024

    def __init__(
        self,
        serial: Optional[str] = None,
        device: str = "Cortex-M0+",
        speed: int = 4000,
        channel: int = 0,
        ring_buffer_size: int = 100,
        poll_interval_ms: int = 10,
        ram_start: int = 0x20000000,
        ram_size: int = 0x8000,
    ):
        self._serial = serial
        self._device = device
        self._speed = speed
        self._channel = channel
        self._poll_interval = poll_interval_ms / 1000.0
        self._ring_buffer_size = ring_buffer_size
        self._ram_start = ram_start
        self._ram_size = ram_size

        self._jlink: Optional[pylink.JLink] = None
        self._running = False
        self._rtt_started = False
        self._monitor_thread: Optional[threading.Thread] = None
        self._ring_buffer: deque[str] = deque(maxlen=ring_buffer_size)
        self._lock = threading.Lock()
        self._log_file = None
        self._log_size = 0  # Track log file size for rotation
        self._line_buf = ""  # Partial line buffer for line-aligned timestamping
        self._max_line_buf = 4096  # Max partial line buffer size (bytes)
        # Resolve the per-instance log path (env override > portable default).
        # Overrides the class-level RTT_LOG_FILE=None so all methods see a real path.
        self.RTT_LOG_FILE = _default_log_file()

    @staticmethod
    def _ts() -> str:
        """Return current time as [HH:MM:SS.mmm]."""
        now = datetime.now()
        return f"[{now.strftime('%H:%M:%S')}.{now.microsecond // 1000:03d}]"

    def _flush_line(self, line: str) -> None:
        """Write a complete timestamped line to log, stderr, and ring buffer."""
        if not line:
            return
        stamped = f"{self._ts()} {line}\n"
        encoded = stamped.encode('utf-8')
        if self._log_file:
            try:
                self._log_file.write(stamped)
                self._log_size += len(encoded)
                if self._log_size >= self.LOG_MAX_SIZE:
                    self._log_file.close()
                    self._log_file = open(self.RTT_LOG_FILE, 'w', encoding='utf-8')
                    self._log_size = 0
                    self._log_file.write("[RTT Log rotated]\n")
                else:
                    self._log_file.flush()
            except Exception:
                pass
        print(stamped, end='', file=sys.stderr, flush=True)
        with self._lock:
            self._ring_buffer.append(stamped)

    def _connect_jlink(self) -> str:
        """Connect to J-Link hardware.

        Returns:
            Error message string on failure, empty string on success.
        """
        try:
            self._jlink = pylink.JLink()
            if self._serial:
                self._jlink.open(serial_no=self._serial)
            else:
                self._jlink.open()
            print(f"[JLink] J-Link opened: {self._jlink.product_name}", file=sys.stderr, flush=True)
            return ""
        except Exception as e:
            import traceback
            tb = traceback.format_exc()
            print(f"[JLink] Failed to open J-Link:\n{tb}", file=sys.stderr, flush=True)
            # Release the partially-opened handle so a retry on the process-global
            # singleton doesn't orphan it (and risk locking the probe until the
            # process is killed). Mirrors the cleanup in connect()'s other paths.
            if self._jlink:
                try:
                    self._jlink.close()
                except Exception:
                    pass
                self._jlink = None
            return tb

    def _connect_device(self) -> bool:
        """Connect to target device.

        Tries the configured device name first, then falls back to
        generic Cortex-M0+ if the specific name is not recognized.
        """
        if not self._jlink:
            return False

        # Set SWD interface and speed (like rtt_t2)
        try:
            import pylink.enums
            self._jlink.set_tif(pylink.enums.JLinkInterfaces.SWD)
        except Exception:
            pass

        # Try device names in order
        device_names = [self._device]
        if self._device != "Cortex-M0+":
            device_names.append("Cortex-M0+")

        for dev in device_names:
            try:
                print(f"[JLink] Trying device '{dev}' at {self._speed} kHz...", file=sys.stderr, flush=True)
                self._jlink.set_speed(self._speed)
                self._jlink.connect(dev)
                print(f"[JLink] Connected! Core: {self._jlink.core_name()}", file=sys.stderr, flush=True)
                return True
            except Exception as e:
                print(f"[JLink] '{dev}' failed: {e}", file=sys.stderr, flush=True)

        print(f"[JLink] All device names failed. Check your J-Link connection.", file=sys.stderr, flush=True)
        return False

    def _find_rtt_address(self) -> int:
        """Search RAM for the SEGGER RTT control block.

        Reads RAM in chunks to avoid allocating one huge string.
        Returns the address if found, or -1.

        Returns:
            Address of RTT control block, or -1 if not found.
        """
        try:
            CHUNK_WORDS = 1024  # Read 4KB at a time
            total_words = self._ram_size // 4
            search_sig = b"SEGGER RTT"

            for offset_words in range(0, total_words, CHUNK_WORDS):
                words_to_read = min(CHUNK_WORDS, total_words - offset_words)
                addr = self._ram_start + offset_words * 4
                raw = self._jlink.memory_read32(addr, words_to_read)

                # Convert to bytes for searching (more efficient than string)
                chunk = b''.join(
                    n.to_bytes(4, 'little', signed=False) for n in raw
                )
                idx = chunk.find(search_sig)
                if idx >= 0:
                    return addr + idx
            return -1
        except Exception as e:
            print(f"[JLink] RTT address search error: {e}", file=sys.stderr, flush=True)
            return -1

    def connect(self, serial: Optional[str] = None,
                device: Optional[str] = None,
                speed: Optional[int] = None) -> tuple[bool, str]:
        """Connect to J-Link and start RTT monitoring.

        Args:
            serial: Override J-Link serial number.
            device: Override target device name.
            speed: Override SWD speed in kHz.

        Returns:
            (success, error_message) tuple.
        """
        if serial is not None:
            self._serial = serial
        if device is not None:
            self._device = device
        if speed is not None:
            self._speed = speed
        err = self._connect_jlink()
        if err:
            return False, err

        if not self._connect_device():
            dev_err = f"Failed to connect to target device '{self._device}'"
            self._jlink.close()
            self._jlink = None
            return False, dev_err

        # Set SWD interface explicitly (like rtt_t2 does)
        try:
            import pylink.enums
            self._jlink.set_tif(pylink.enums.JLinkInterfaces.SWD)
        except Exception:
            pass

        # Manually search for RTT control block in RAM
        print(f"[JLink] Searching for RTT CB in RAM 0x{self._ram_start:08X}-0x{self._ram_start + self._ram_size:08X}...",
              file=sys.stderr, flush=True)
        rtt_addr = self._find_rtt_address()

        if rtt_addr >= 0:
            print(f"[JLink] RTT control block found at 0x{rtt_addr:08X}", file=sys.stderr, flush=True)
            try:
                self._jlink.rtt_start(rtt_addr)
                self._rtt_started = True
                print(f"[JLink] RTT started with explicit address", file=sys.stderr, flush=True)
            except Exception as e:
                print(f"[JLink] rtt_start(0x{rtt_addr:08X}) failed: {e}", file=sys.stderr, flush=True)
                self._jlink.close()
                self._jlink = None
                return False, f"rtt_start failed: {e}"
        else:
            print("[JLink] RTT CB not found, trying rtt_start() without address...", file=sys.stderr, flush=True)
            try:
                self._jlink.rtt_start()
                self._rtt_started = True
                print("[JLink] RTT started (auto-search)", file=sys.stderr, flush=True)
            except Exception as e:
                print(f"[JLink] rtt_start() failed: {e}", file=sys.stderr, flush=True)
                self._jlink.close()
                self._jlink = None
                return False, f"rtt_start failed: {e}"

        # Wait briefly for RTT to stabilize
        time.sleep(0.5)

        # Verify RTT is working
        try:
            num_up = self._jlink.rtt_get_num_up_buffers()
            num_down = self._jlink.rtt_get_num_down_buffers()
            print(f"[JLink] RTT buffers: {num_up} up, {num_down} down", file=sys.stderr, flush=True)
        except Exception as e:
            print(f"[JLink] Buffer query error: {e}", file=sys.stderr, flush=True)

        # Test read (route through line buffer for consistent timestamping)
        try:
            test_data = self._jlink.rtt_read(self._channel, 512)
            print(f"[JLink] Test read: {len(test_data)} bytes", file=sys.stderr, flush=True)
            if test_data:
                decoded = bytes(test_data).decode('utf-8', errors='replace')
                print(f"[JLink] Data preview: {repr(decoded[:100])}", file=sys.stderr, flush=True)
                # Route through line buffer so initial data gets timestamps
                self._line_buf += decoded
                while '\n' in self._line_buf:
                    line, self._line_buf = self._line_buf.split('\n', 1)
                    self._flush_line(line)
        except Exception as e:
            print(f"[JLink] Test read error: {e}", file=sys.stderr, flush=True)

        # Open log file for RTT output
        try:
            self._log_file = open(self.RTT_LOG_FILE, 'w', encoding='utf-8')
            print(f"[JLink] RTT log file: {self.RTT_LOG_FILE}", file=sys.stderr, flush=True)
        except Exception as e:
            print(f"[JLink] Warning: cannot open log file: {e}", file=sys.stderr, flush=True)
            self._log_file = None

        # Start background monitor thread
        self._running = True
        self._monitor_thread = threading.Thread(target=self._monitor_loop, daemon=True)
        self._monitor_thread.start()

        print(f"[JLink] Connected to {self._device} via J-Link", file=sys.stderr, flush=True)
        print(f"[JLink] To view RTT data in terminal, run: tail -f {self.RTT_LOG_FILE}", file=sys.stderr, flush=True)
        return True, ""

    def disconnect(self) -> None:
        """Disconnect from J-Link and stop monitoring."""
        self._running = False

        if self._monitor_thread:
            self._monitor_thread.join(timeout=1.0)
            self._monitor_thread = None

        # Flush any remaining partial line
        if self._line_buf:
            self._flush_line(self._line_buf)
            self._line_buf = ""

        # Close log file
        if self._log_file:
            try:
                self._log_file.close()
            except Exception:
                pass
            self._log_file = None

        if self._jlink:
            try:
                self._jlink.rtt_stop()
            except Exception:
                pass
            try:
                self._jlink.close()
            except Exception:
                pass
            self._jlink = None

        self._rtt_started = False
        print("[JLink] Disconnected", file=sys.stderr, flush=True)

    def _monitor_loop(self) -> None:
        """Background thread that continuously reads RTT and writes to log file."""
        while self._running:
            if not self._jlink:
                time.sleep(self._poll_interval)
                continue

            try:
                # Read from up-buffer (device -> host)
                data_bytes = self._jlink.rtt_read(self._channel, 512)
                if data_bytes:
                    try:
                        data_str = bytes(data_bytes).decode('utf-8', errors='replace')
                    except Exception:
                        data_str = str(data_bytes)

                    if data_str:
                        # Line-buffer: accumulate partial lines, flush complete ones
                        self._line_buf += data_str
                        # Guard: drop stale partial line if buffer exceeds limit
                        if len(self._line_buf) > self._max_line_buf:
                            self._line_buf = self._line_buf[-self._max_line_buf // 2:]
                        while '\n' in self._line_buf:
                            line, self._line_buf = self._line_buf.split('\n', 1)
                            self._flush_line(line)
            except Exception as e:
                # Log error for debugging
                print(f"[RTT Monitor] Error: {e}", file=sys.stderr, flush=True)
                time.sleep(0.5)  # Slow down on error to avoid log spam

            time.sleep(self._poll_interval)

    def read(self, channel: int = 0, max_bytes: int = 512) -> str:
        """Read and consume accumulated data from the ring buffer.

        Returns all data and clears the buffer (read-once semantics),
        so repeated calls don't return duplicate data.

        Args:
            channel: RTT channel to read from (unused, for API compatibility).
            max_bytes: Maximum bytes to return.

        Returns:
            Accumulated RTT data as a string.
        """
        with self._lock:
            data = ''.join(self._ring_buffer)
            self._ring_buffer.clear()
            # Truncate if needed
            if len(data) > max_bytes:
                data = data[-max_bytes:]
            return data

    def read_log_tail(self, max_bytes: int = 8192) -> str:
        """Return the tail of the complete RTT log file.

        Unlike ``read()`` (which drains a single shared ring buffer), the log file
        is an independent broadcast sink: nobody drains it, so every consumer sees
        the full output. Use this when another client (e.g. the VSCode extension)
        is actively streaming via the ring buffer.
        """
        try:
            size = os.path.getsize(self.RTT_LOG_FILE)
            with open(self.RTT_LOG_FILE, "rb") as f:
                if size > max_bytes:
                    f.seek(-max_bytes, os.SEEK_END)
                    f.readline()  # discard a possibly partial first line
                data = f.read()
            return data.decode("utf-8", errors="replace")
        except FileNotFoundError:
            return ""
        except Exception as e:
            return f"(log read error: {e})\n"

    def log_file_size(self) -> int:
        """Return the current byte size of the RTT log file (0 if absent).

        Used by continuous monitors (``read_log_raw``) to detect rotation: if the
        file shrank below a previously returned offset, the log was rotated and the
        cursor must reset to 0.
        """
        try:
            return os.path.getsize(self.RTT_LOG_FILE)
        except OSError:
            return 0

    def read_log_raw(self, offset: int = 0, max_bytes: int = 8192) -> tuple[str, int]:
        """Read new bytes from the broadcast log starting at ``offset``.

        Non-draining, multi-consumer safe: this never consumes data, so an
        arbitrary number of monitors can poll independently. The caller tracks the
        returned ``next_offset`` and passes it back as ``offset`` on the next poll.

        If the log rotated since the last call (file smaller than ``offset``), the
        read restarts from 0 and ``next_offset`` reflects only the fresh content.

        Returns:
            (data, next_offset) where next_offset is the cursor for the next call.
        """
        try:
            size = os.path.getsize(self.RTT_LOG_FILE)
            if offset > size:
                # Log was rotated/truncated under us; restart from the beginning.
                offset = 0
            with open(self.RTT_LOG_FILE, "rb") as f:
                f.seek(offset)
                data = f.read(max_bytes)
            return data.decode("utf-8", errors="replace"), offset + len(data)
        except FileNotFoundError:
            return "", 0
        except Exception as e:
            return f"(log read error: {e})\n", offset

    def write(self, channel: int, data: str) -> int:
        """Write data to RTT down-buffer (host -> device).

        Args:
            channel: RTT channel to write to.
            data: String data to write.

        Returns:
            Number of bytes written, or -1 on error.
        """
        if not self._jlink:
            return -1

        try:
            data_bytes = data.encode('utf-8')
            written = self._jlink.rtt_write(channel, data_bytes)
            return written
        except Exception as e:
            print(f"[JLink] Write error: {e}", file=sys.stderr, flush=True)
            return -1

    def list_devices(self) -> list[str]:
        """List available J-Link devices.

        Returns:
            List of J-Link serial numbers as strings.
        """
        try:
            jlink = pylink.JLink()
            emulators = jlink.connected_emulators()
            return [str(e) for e in emulators]
        except Exception:
            return []

    def is_connected(self) -> bool:
        """Check if J-Link is connected and RTT is running."""
        return self._jlink is not None and self._running

    def status(self) -> RTTStatus:
        """Get current RTT status.

        Returns:
            RTTStatus object with current connection info.
        """
        return RTTStatus(
            connected=self._jlink is not None,
            rtt_started=self._rtt_started,
            device_name=self._device,
            speed=self._speed,
            serial=self._serial,
            channel=self._channel,
            ring_buffer_size=len(self._ring_buffer),
        )

    def clear_buffer(self) -> None:
        """Clear the ring buffer."""
        with self._lock:
            self._ring_buffer.clear()


# Global singleton instance
_rtt_instance: Optional[JLinkRTT] = None


def get_instance() -> JLinkRTT:
    """Get or create the global RTT instance."""
    global _rtt_instance
    if _rtt_instance is None:
        # Load from environment variables
        serial = os.environ.get("JLINK_SERIAL")
        device = os.environ.get("JLINK_DEVICE", "Cortex-M0+")
        speed = int(os.environ.get("JLINK_SPEED", "4000"))
        channel = int(os.environ.get("RTT_CHANNEL", "0"))
        ring_size = int(os.environ.get("RTT_RING_BUFFER_SIZE", "100"))
        poll_ms = int(os.environ.get("RTT_POLL_INTERVAL_MS", "10"))
        ram_start = int(os.environ.get("JLINK_RAM_START", "0x20000000"), 16) if os.environ.get("JLINK_RAM_START") else 0x20000000
        ram_size = int(os.environ.get("JLINK_RAM_SIZE", "0x8000"), 16) if os.environ.get("JLINK_RAM_SIZE") else 0x8000

        _rtt_instance = JLinkRTT(
            serial=serial,
            device=device,
            speed=speed,
            channel=channel,
            ring_buffer_size=ring_size,
            poll_interval_ms=poll_ms,
            ram_start=ram_start,
            ram_size=ram_size,
        )
    return _rtt_instance
