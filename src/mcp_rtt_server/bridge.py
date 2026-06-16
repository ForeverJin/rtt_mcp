"""stdio <-> SSE bridge for the rtt MCP server.

The VSCode extension keeps its existing (proven) line-delimited-JSON stdio client.
This module is the child that client spawns: it speaks MCP over stdio on one side
and forwards every call to the shared SSE daemon on the other. The daemon (not this
bridge) owns the J-Link, so the extension never opens the probe directly and there
is no contention with Claude Code.

Entry:  python -m mcp_rtt_server.bridge
Env:    RTT_DAEMON_URL (default http://127.0.0.1:8765/sse)
"""

from __future__ import annotations

import asyncio
import os
import sys

from mcp.client.session import ClientSession
from mcp.client.sse import sse_client
from mcp.server import Server
from mcp.server.stdio import stdio_server

DEFAULT_URL = "http://127.0.0.1:8765/sse"


async def main() -> None:
    url = os.environ.get("RTT_DAEMON_URL", DEFAULT_URL)

    async with sse_client(url) as (read, write):
        async with ClientSession(read, write) as daemon:
            await daemon.initialize()

            server = Server("rtt-bridge")

            @server.list_tools()
            async def list_tools():
                result = await daemon.list_tools()
                return result.tools

            @server.call_tool()
            async def handle_call_tool(name: str, arguments: dict):
                result = await daemon.call_tool(name, arguments)
                return result.content

            async with stdio_server() as (stdin_read, stdin_write):
                await server.run(
                    stdin_read,
                    stdin_write,
                    server.create_initialization_options(),
                )


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        sys.exit(0)
