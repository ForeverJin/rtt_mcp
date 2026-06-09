"""J-Link RTT wrapper with background monitor thread."""

from __future__ import annotations

import os
import sys
import threading
import time
from collections import deque
from dataclasses import dataclass
from typing import Optional

import pylink


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

    # Log file path for RTT output (always in the mcp-rtt-server directory)
    RTT_LOG_FILE = os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(
        os.path.abspath(__file__)))), "rtt_output.log")

    def __init__(
        self,
        serial: Optional[str] = None,
        device: str = "HC32L19x",
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

        Manually reads RAM and searches for the "SEGGER RTT" signature,
        exactly like rtt_t2 does. Returns the address if found, or -1.

        Returns:
            Address of RTT control block, or -1 if not found.
        """
        try:
            num_words = self._ram_size // 4
            num_bytes = self._jlink.memory_read32(self._ram_start, num_words)
            # Convert list of 32-bit integers to string for searching
            mem_str = ''.join(
                n.to_bytes(4, 'little', signed=False).decode('latin-1')
                for n in num_bytes
            )
            offset = mem_str.find("SEGGER RTT")
            if offset >= 0:
                return self._ram_start + offset
            return -1
        except Exception as e:
            print(f"[JLink] RTT address search error: {e}", file=sys.stderr, flush=True)
            return -1

    def connect(self) -> tuple[bool, str]:
        """Connect to J-Link and start RTT monitoring.

        Returns:
            (success, error_message) tuple.
        """
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

        # Test read
        try:
            test_data = self._jlink.rtt_read(self._channel, 512)
            print(f"[JLink] Test read: {len(test_data)} bytes", file=sys.stderr, flush=True)
            if test_data:
                decoded = bytes(test_data).decode('utf-8', errors='replace')
                print(f"[JLink] Data preview: {repr(decoded[:100])}", file=sys.stderr, flush=True)
                # Store initial data in ring buffer
                with self._lock:
                    self._ring_buffer.append(decoded)
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
                        # Write to log file (for terminal viewing)
                        if self._log_file:
                            try:
                                self._log_file.write(data_str)
                                self._log_file.flush()
                            except Exception:
                                pass
                        # Also print to stderr (for MCP Inspector notifications)
                        print(data_str, end='', file=sys.stderr, flush=True)
                        # Store in ring buffer
                        with self._lock:
                            self._ring_buffer.append(data_str)
            except Exception as e:
                # Log error for debugging
                print(f"[RTT Monitor] Error: {e}", file=sys.stderr, flush=True)
                time.sleep(0.5)  # Slow down on error to avoid log spam

            time.sleep(self._poll_interval)

    def read(self, channel: int = 0, max_bytes: int = 512) -> str:
        """Read all accumulated data from the ring buffer.

        Args:
            channel: RTT channel to read from (unused, for API compatibility).
            max_bytes: Maximum bytes to return (unused, for API compatibility).

        Returns:
            All accumulated RTT data as a string.
        """
        with self._lock:
            return ''.join(self._ring_buffer)

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
        device = os.environ.get("JLINK_DEVICE", "HC32L19x")
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
