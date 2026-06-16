"""MCP Tools for Segger RTT via J-Link."""

from __future__ import annotations

import asyncio
import json

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
            name="rtt_read_log",
            description="Read the tail of the complete RTT log file. This is an "
                        "independent broadcast sink (not drained by other clients), "
                        "so it returns the full RTT output even while another client "
                        "(e.g. the VSCode extension) is streaming via rtt_read. "
                        "Prefer this over rtt_read when another consumer is active.",
            inputSchema={
                "type": "object",
                "properties": {
                    "max_bytes": {
                        "type": "integer",
                        "description": "Maximum bytes to return from the end of the log (default: 8192).",
                        "default": 8192,
                    },
                },
            },
        ),
        Tool(
            name="rtt_read_raw",
            description="Read new bytes from the broadcast log starting at a byte offset. "
                        "Non-draining and multi-consumer safe: ideal for a continuous "
                        "monitor that must coexist with other readers without stealing "
                        "their data. Pass the returned next_offset as 'offset' on the "
                        "next call; if the log rotated (next_offset > file size), pass "
                        "offset=0. Returns JSON: {\"data\": \"...\", \"next_offset\": N}.",
            inputSchema={
                "type": "object",
                "properties": {
                    "offset": {
                        "type": "integer",
                        "description": "Byte offset to read from (default: 0).",
                        "default": 0,
                    },
                    "max_bytes": {
                        "type": "integer",
                        "description": "Maximum bytes to return (default: 8192).",
                        "default": 8192,
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

    The JLinkRTT layer wraps pylink, which performs blocking USB/DLL I/O. To keep
    the SSE daemon's asyncio event loop responsive for all clients, every blocking
    pylink call is offloaded to a worker thread via ``asyncio.to_thread``. The
    stdio server (single client) is unaffected by this.

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

        # Shared daemon: if a connection already exists (another client opened it),
        # keep it. Force-reconnecting here would drop the J-Link for everyone.
        if rtt.is_connected():
            status = rtt.status()
            return [TextContent(
                type="text",
                text=f"Already connected to J-Link device '{status.device_name}' "
                     f"(serial: {status.serial}, speed: {status.speed} kHz)\n"
                     f"RTT monitoring active on channel {status.channel} (shared connection)",
            )]

        success, err_msg = await asyncio.to_thread(
            rtt.connect, serial=serial, device=device, speed=speed
        )
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

        await asyncio.to_thread(rtt.disconnect)
        return [TextContent(type="text", text="J-Link disconnected successfully.")]

    elif name == "rtt_read":
        if not rtt.is_connected():
            return [TextContent(
                type="text",
                text="J-Link is not connected. Call jlink_connect first.",
            )]

        channel = arguments.get("channel", 0)
        max_bytes = arguments.get("max_bytes", 512)

        # NOTE: the monitor thread is the sole reader of the physical RTT up-buffer.
        # rtt_read returns only what it accumulated in the ring buffer, so it never
        # steals bytes from the monitor's log file / stderr / other consumers.
        ring_data = await asyncio.to_thread(rtt.read, channel=channel, max_bytes=max_bytes)
        return [TextContent(type="text", text=ring_data if ring_data else "(no RTT data)")]

    elif name == "rtt_read_log":
        max_bytes = arguments.get("max_bytes", 8192)
        data = await asyncio.to_thread(rtt.read_log_tail, max_bytes)
        return [TextContent(type="text", text=data if data else "(no RTT log yet — connect first)")]

    elif name == "rtt_read_raw":
        offset = arguments.get("offset", 0)
        max_bytes = arguments.get("max_bytes", 8192)
        data, next_offset = await asyncio.to_thread(rtt.read_log_raw, offset, max_bytes)
        payload = json.dumps({"data": data, "next_offset": next_offset})
        return [TextContent(type="text", text=payload)]

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

        written = await asyncio.to_thread(rtt.write, channel=channel, data=data)
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
        devices = await asyncio.to_thread(rtt.list_devices)
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

        # status() only reads cached fields; no blocking pylink call.
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

        await asyncio.to_thread(rtt.clear_buffer)
        return [TextContent(type="text", text="RTT buffer cleared.")]

    else:
        return [TextContent(
            type="text",
            text=f"Unknown tool: {name}",
        )]
