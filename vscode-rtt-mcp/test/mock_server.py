#!/usr/bin/env python3
"""Mock MCP RTT server for testing vscode-rtt-mcp without J-Link hardware.

Speaks the MCP/JSON-RPC over stdio protocol (Content-Length framed) and
simulates all 7 RTT tools with an in-memory text buffer. A background
thread periodically injects mock RTT data so monitor mode has output.

Run standalone:    python test/mock_server.py
Or via npm test:   npm test  (spawns this script under McpClient)
"""

import json
import os
import sys
import threading
import time
from collections import deque

SERVER_INFO = {"name": "mock-rtt-server", "version": "0.1.0"}

TOOL_DEFS = [
    {
        "name": "jlink_connect",
        "description": "Connect to J-Link debugger and start RTT monitoring.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "serial": {"type": "string"},
                "device": {"type": "string", "default": "HC32L19x"},
                "speed": {"type": "integer", "default": 4000},
            },
        },
    },
    {
        "name": "jlink_disconnect",
        "description": "Disconnect from J-Link debugger and stop RTT monitoring.",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "rtt_read",
        "description": "Read accumulated RTT data from the ring buffer.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "channel": {"type": "integer", "default": 0},
                "max_bytes": {"type": "integer", "default": 512},
            },
        },
    },
    {
        "name": "rtt_write",
        "description": "Write data to RTT down-buffer (host -> device).",
        "inputSchema": {
            "type": "object",
            "properties": {
                "channel": {"type": "integer", "default": 0},
                "data": {"type": "string"},
            },
            "required": ["data"],
        },
    },
    {
        "name": "rtt_list_devices",
        "description": "List available J-Link debuggers connected to the host.",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "jlink_status",
        "description": "Get current J-Link connection status and RTT buffer information.",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "rtt_clear",
        "description": "Clear the RTT ring buffer.",
        "inputSchema": {"type": "object", "properties": {}},
    },
]

state = {
    "connected": False,
    "device": "HC32L19x",
    "serial": "MOCK12345",
    "speed": 4000,
    "channel": 0,
    "ring_buffer": deque(maxlen=100),
    "host_log": [],
    "counter": 0,
}


def _text(payload):
    return {"content": [{"type": "text", "text": payload}]}


def tool_jlink_connect(args):
    state["connected"] = True
    if args.get("device"):
        state["device"] = args["device"]
    if args.get("serial"):
        state["serial"] = args["serial"]
    if args.get("speed"):
        state["speed"] = args["speed"]
    return _text(
        f"Connected to J-Link device '{state['device']}' "
        f"(serial: {state['serial']}, speed: {state['speed']} kHz)\n"
        f"RTT monitoring started on channel {state['channel']}"
    )


def tool_jlink_disconnect(_args):
    state["connected"] = False
    return _text("J-Link disconnected successfully.")


def tool_rtt_read(args):
    if not state["connected"]:
        return {
            "content": [{"type": "text", "text": "J-Link is not connected. Call jlink_connect first."}],
            "isError": True,
        }
    data = "".join(state["ring_buffer"])
    state["ring_buffer"].clear()
    diag = (
        "[RTT Diagnostics]\n"
        "Mock buffers: 1 up, 1 down\n"
        "Mock status: OK\n"
        f"Direct read: {len(data)} bytes\n"
        f"Ring buffer: {len(data)} chars, {len(state['ring_buffer'])} entries\n"
    )
    body = diag + ("[RTT Data]\n" + data if data else "\n(no RTT data)")
    return _text(body)


def tool_rtt_write(args):
    if not state["connected"]:
        return {
            "content": [{"type": "text", "text": "J-Link is not connected."}],
            "isError": True,
        }
    data = args.get("data", "")
    channel = args.get("channel", 0)
    state["host_log"].append(data)
    return _text(f"Wrote {len(data.encode('utf-8'))} bytes to RTT channel {channel}")


def tool_rtt_list_devices(_args):
    return _text("Available J-Link devices:\n  - MOCK12345\n  - MOCK67890")


def tool_jlink_status(_args):
    if not state["connected"]:
        return _text("J-Link is not connected.")
    return _text(
        "J-Link Status:\n"
        f"  Connected: True\n"
        f"  RTT Started: True\n"
        f"  Device: {state['device']}\n"
        f"  Serial: {state['serial']}\n"
        f"  Speed: {state['speed']} kHz\n"
        f"  Channel: {state['channel']}\n"
        f"  Buffer Size: {len(state['ring_buffer'])} entries"
    )


def tool_rtt_clear(_args):
    state["ring_buffer"].clear()
    return _text("RTT buffer cleared.")


TOOL_DISPATCH = {
    "jlink_connect": tool_jlink_connect,
    "jlink_disconnect": tool_jlink_disconnect,
    "rtt_read": tool_rtt_read,
    "rtt_write": tool_rtt_write,
    "rtt_list_devices": tool_rtt_list_devices,
    "jlink_status": tool_jlink_status,
    "rtt_clear": tool_rtt_clear,
}


def make_response(msg_id, result):
    return {"jsonrpc": "2.0", "id": msg_id, "result": result}


def make_error(msg_id, code, message):
    return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": code, "message": message}}


def handle_request(msg):
    method = msg.get("method")
    msg_id = msg.get("id")
    params = msg.get("params", {})

    if method == "initialize":
        return make_response(msg_id, {
            "protocolVersion": "2024-11-05",
            "capabilities": {"tools": {}},
            "serverInfo": SERVER_INFO,
        })
    if method == "tools/list":
        return make_response(msg_id, {"tools": TOOL_DEFS})
    if method == "tools/call":
        name = params.get("name")
        args = params.get("arguments", {})
        handler = TOOL_DISPATCH.get(name)
        if not handler:
            return make_error(msg_id, -32602, f"Unknown tool: {name}")
        try:
            return make_response(msg_id, handler(args))
        except Exception as exc:
            return make_error(msg_id, -32603, str(exc))
    if method == "notifications/initialized":
        return None
    return make_error(msg_id, -32601, f"Method not found: {method}")


def send_message(msg):
    line = (json.dumps(msg) + "\n").encode("utf-8")
    sys.stdout.buffer.write(line)
    sys.stdout.buffer.flush()


def read_loop():
    """Read line-delimited JSON-RPC messages from stdin (matches MCP Python SDK)."""
    while True:
        line = sys.stdin.readline()
        if not line:
            return
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        response = handle_request(msg)
        if response is not None:
            send_message(response)


def background_injector():
    while True:
        time.sleep(0.3)
        if state["connected"]:
            state["counter"] += 1
            state["ring_buffer"].append(
                f"[Mock #{state['counter']:04d}] tick={state['counter']} uptime={state['counter'] * 0.3:.1f}s\n"
            )


def main():
    print(f"[mock-rtt] starting pid={os.getpid()}", file=sys.stderr, flush=True)
    threading.Thread(target=background_injector, daemon=True).start()
    try:
        read_loop()
    except (BrokenPipeError, KeyboardInterrupt, EOFError):
        pass
    print("[mock-rtt] shutting down", file=sys.stderr, flush=True)


if __name__ == "__main__":
    main()
