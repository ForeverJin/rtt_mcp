"""MCP Tools for Segger RTT via J-Link."""

from __future__ import annotations

from mcp.types import Tool, TextContent

from .jlink_rtt import get_instance


def get_tool_definitions() -> list[Tool]:
    """Get all MCP tool definitions."""
    return [
        Tool(
            name="jlink_connect",
            description="Connect to J-Link debugger and start RTT monitoring. "
                        "Must be called before reading or writing RTT data.",
            inputSchema={
                "type": "object",
                "properties": {
                    "serial": {
                        "type": "string",
                        "description": "J-Link serial number (optional). "
                                       "If not provided, connects to first available J-Link.",
                    },
                    "device": {
                        "type": "string",
                        "description": "Target device name (default: HC32L19x). "
                                       "Examples: Cortex-M0+, STM32F103, etc.",
                        "default": "HC32L19x",
                    },
                    "speed": {
                        "type": "integer",
                        "description": "JTAG/SWD speed in kHz (default: 4000).",
                        "default": 4000,
                    },
                },
            },
        ),
        Tool(
            name="jlink_disconnect",
            description="Disconnect from J-Link debugger and stop RTT monitoring.",
            inputSchema={
                "type": "object",
                "properties": {},
            },
        ),
        Tool(
            name="rtt_read",
            description="Read accumulated RTT data from the ring buffer. "
                        "Data is continuously collected by the background monitor thread. "
                        "Use this to get output from the target device.",
            inputSchema={
                "type": "object",
                "properties": {
                    "channel": {
                        "type": "integer",
                        "description": "RTT channel to read from (default: 0).",
                        "default": 0,
                    },
                    "max_bytes": {
                        "type": "integer",
                        "description": "Maximum bytes to return (default: 512). "
                                       "Note: This reads from the accumulated buffer.",
                        "default": 512,
                    },
                },
            },
        ),
        Tool(
            name="rtt_write",
            description="Write data to RTT down-buffer (host -> device). "
                        "The target device must be running RTT with a down-buffer listener.",
            inputSchema={
                "type": "object",
                "properties": {
                    "channel": {
                        "type": "integer",
                        "description": "RTT channel to write to (default: 0).",
                        "default": 0,
                    },
                    "data": {
                        "type": "string",
                        "description": "String data to send to the target device.",
                    },
                },
                "required": ["data"],
            },
        ),
        Tool(
            name="rtt_list_devices",
            description="List available J-Link debuggers connected to the host.",
            inputSchema={
                "type": "object",
                "properties": {},
            },
        ),
        Tool(
            name="jlink_status",
            description="Get current J-Link connection status and RTT buffer information.",
            inputSchema={
                "type": "object",
                "properties": {},
            },
        ),
        Tool(
            name="rtt_clear",
            description="Clear the RTT ring buffer. "
                        "This does not clear the actual RTT buffers on the device.",
            inputSchema={
                "type": "object",
                "properties": {},
            },
        ),
    ]


async def call_tool(name: str, arguments: dict) -> list[TextContent]:
    """Call a tool by name with the given arguments.

    Args:
        name: Tool name.
        arguments: Tool arguments.

    Returns:
        List of TextContent results.
    """
    rtt = get_instance()

    if name == "jlink_connect":
        serial = arguments.get("serial")
        device = arguments.get("device", "HC32L19x")
        speed = arguments.get("speed", 4000)

        if rtt.is_connected():
            rtt.disconnect()

        success, err_msg = rtt.connect(serial=serial, device=device, speed=speed)
        if success:
            status = rtt.status()
            return [TextContent(
                type="text",
                text=f"Connected to J-Link device '{status.device_name}' "
                     f"(serial: {status.serial}, speed: {status.speed} kHz)\n"
                     f"RTT monitoring started on channel {status.channel}",
            )]
        else:
            return [TextContent(
                type="text",
                text=f"Failed to connect to J-Link.\n\nError:\n{err_msg}",
            )]

    elif name == "jlink_disconnect":
        if not rtt.is_connected():
            return [TextContent(type="text", text="J-Link is not connected.")]

        rtt.disconnect()
        return [TextContent(type="text", text="J-Link disconnected successfully.")]

    elif name == "rtt_read":
        if not rtt.is_connected():
            return [TextContent(
                type="text",
                text="J-Link is not connected. Call jlink_connect first.",
            )]

        channel = arguments.get("channel", 0)
        max_bytes = arguments.get("max_bytes", 512)

        # Try direct J-Link read (bypass ring buffer for debugging)
        diag_parts = []
        try:
            jlink = rtt._jlink
            if jlink:
                # Get RTT buffer info
                try:
                    num_up = jlink.rtt_get_num_up_buffers()
                    num_down = jlink.rtt_get_num_down_buffers()
                    diag_parts.append(f"RTT buffers: {num_up} up, {num_down} down")
                except Exception as e:
                    diag_parts.append(f"RTT buffers error: {e}")

                # Get RTT status
                try:
                    rtt_status = jlink.rtt_get_status()
                    diag_parts.append(f"RTT status: {rtt_status}")
                except Exception as e:
                    diag_parts.append(f"RTT status error: {e}")

                # Direct read
                try:
                    raw = jlink.rtt_read(channel, max_bytes)
                    diag_parts.append(f"Direct read: {len(raw)} bytes")
                    if raw:
                        decoded = bytes(raw).decode('utf-8', errors='replace')
                        diag_parts.append(f"Data: {repr(decoded[:200])}")
                except Exception as e:
                    diag_parts.append(f"Direct read error: {e}")
        except Exception as e:
            diag_parts.append(f"J-Link access error: {e}")

        # Also read from ring buffer
        ring_data = rtt.read(channel=channel, max_bytes=max_bytes)
        diag_parts.append(f"Ring buffer: {len(ring_data)} chars, {rtt.status().ring_buffer_size} entries")

        return [TextContent(
            type="text",
            text="[RTT Diagnostics]\n" + "\n".join(diag_parts) +
                 (f"\n\n[RTT Data]\n{ring_data}" if ring_data else "\n\n(no RTT data)"),
        )]

    elif name == "rtt_write":
        if not rtt.is_connected():
            return [TextContent(
                type="text",
                text="J-Link is not connected. Call jlink_connect first.",
            )]

        channel = arguments.get("channel", 0)
        data = arguments.get("data", "")

        if not data:
            return [TextContent(type="text", text="No data provided to write.")]

        written = rtt.write(channel=channel, data=data)
        if written >= 0:
            return [TextContent(
                type="text",
                text=f"Wrote {written} bytes to RTT channel {channel}",
            )]
        else:
            return [TextContent(
                type="text",
                text="Failed to write to RTT.",
            )]

    elif name == "rtt_list_devices":
        devices = rtt.list_devices()
        if devices:
            return [TextContent(
                type="text",
                text="Available J-Link devices:\n" + "\n".join(f"  - {d}" for d in devices),
            )]
        else:
            return [TextContent(
                type="text",
                text="No J-Link devices found. Make sure J-Link is connected.",
            )]

    elif name == "jlink_status":
        if not rtt.is_connected():
            return [TextContent(type="text", text="J-Link is not connected.")]

        status = rtt.status()
        return [TextContent(
            type="text",
            text=f"J-Link Status:\n"
                 f"  Connected: {status.connected}\n"
                 f"  RTT Started: {status.rtt_started}\n"
                 f"  Device: {status.device_name}\n"
                 f"  Serial: {status.serial}\n"
                 f"  Speed: {status.speed} kHz\n"
                 f"  Channel: {status.channel}\n"
                 f"  Buffer Size: {status.ring_buffer_size} entries",
        )]

    elif name == "rtt_clear":
        if not rtt.is_connected():
            return [TextContent(
                type="text",
                text="J-Link is not connected. Call jlink_connect first.",
            )]

        rtt.clear_buffer()
        return [TextContent(type="text", text="RTT buffer cleared.")]

    else:
        return [TextContent(
            type="text",
            text=f"Unknown tool: {name}",
        )]
