"""MCP Server for Segger RTT via J-Link.

Claude Code spawns this module (``python -m mcp_rtt_server.server``). To avoid
fighting the VSCode extension over the single physical J-Link probe, this server
does **not** unconditionally open the probe. Instead:

  * If the shared SSE daemon (``mcp_rtt_server.http_server``) is reachable, it
    proxies every tool call to the daemon over SSE. The daemon is the single
    J-Link owner; both Claude Code and the VSCode extension share it with zero
    contention.
  * Only when the daemon is NOT running (a Claude-only session with the extension
    closed) does it fall back to opening the J-Link directly, exactly as before.

This keeps ``settings.json`` unchanged and still makes the documented single-owner
architecture true for the Claude Code path.
"""

from __future__ import annotations

import asyncio
import sys

from mcp.server import Server
from mcp.server.stdio import stdio_server
from mcp.types import Tool

from .bridge import is_daemon_reachable, run_proxy_over_stdio
from .jlink_rtt import get_instance
from .tools import get_tool_definitions, call_tool

# Server instance
SERVER_NAME = "mcp-rtt-server"
SERVER_VERSION = "0.1.0"


async def run_direct_owner() -> None:
    """Serve the RTT tools directly over stdio, owning the J-Link in this process.

    Used only as a fallback when the shared daemon is unreachable.
    """
    server = Server(name=SERVER_NAME, version=SERVER_VERSION)
    tool_definitions = get_tool_definitions()

    @server.list_tools()
    async def list_tools() -> list[Tool]:
        """List all available MCP tools."""
        return tool_definitions

    @server.call_tool()
    async def handle_call_tool(name: str, arguments: dict) -> list:
        """Handle tool call requests."""
        return await call_tool(name, arguments)

    # Run the server with stdio transport
    async with stdio_server() as (read_stream, write_stream):
        await server.run(
            read_stream=read_stream,
            write_stream=write_stream,
            initialization_options=server.create_initialization_options(),
        )


async def main():
    """Main entry point for the MCP server.

    Proxy through the shared daemon when it is up (single J-Link owner); otherwise
    fall back to being the direct owner so a Claude-only session still works.
    """
    if is_daemon_reachable():
        print("[rtt] shared daemon reachable — proxying via SSE (no local J-Link open)",
              file=sys.stderr, flush=True)
        try:
            await run_proxy_over_stdio()
            return
        except Exception as e:
            # Daemon disappeared mid-session: fall back to direct ownership rather
            # than stranding the Claude Code client.
            print(f"[rtt] daemon proxy failed ({e}); falling back to direct J-Link",
                  file=sys.stderr, flush=True)

    print("[rtt] shared daemon not reachable — opening J-Link directly (standalone owner)",
          file=sys.stderr, flush=True)
    await run_direct_owner()


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        # Clean shutdown
        rtt = get_instance()
        if rtt.is_connected():
            rtt.disconnect()
        sys.exit(0)
