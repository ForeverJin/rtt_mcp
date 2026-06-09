"""MCP Server for Segger RTT via J-Link.

This server exposes J-Link RTT functionality as MCP tools, allowing
an LLM to interact with an embedded device through RTT.
"""

from __future__ import annotations

import asyncio
import sys
from mcp.server import Server
from mcp.server.stdio import stdio_server
from mcp.types import Tool

from .jlink_rtt import get_instance
from .tools import get_tool_definitions, call_tool

# Server instance
SERVER_NAME = "mcp-rtt-server"
SERVER_VERSION = "0.1.0"


async def main():
    """Main entry point for the MCP server."""
    server = Server(
        name=SERVER_NAME,
        version=SERVER_VERSION,
    )

    # Get tool definitions
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


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        # Clean shutdown
        rtt = get_instance()
        if rtt.is_connected():
            rtt.disconnect()
        sys.exit(0)
