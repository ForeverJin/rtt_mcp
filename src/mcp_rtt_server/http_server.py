"""SSE (HTTP) MCP server for Segger RTT via J-Link.

Runs the SAME tool surface as ``server.py`` but over an SSE transport so a single
long-lived process can serve multiple clients (Claude Code + the VSCode extension)
that all share one J-Link connection through the module-global ``_rtt_instance``
singleton. This is the "single owner" daemon: only this process opens the J-Link.

Entry:  python -m mcp_rtt_server.http_server [--host 127.0.0.1] [--port 8765]
"""

from __future__ import annotations

import argparse

import uvicorn
from mcp.server import Server
from mcp.server.sse import SseServerTransport
from mcp.types import Tool
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import Response
from starlette.routing import Mount, Route

from .jlink_rtt import get_instance
from .tools import get_tool_definitions, call_tool

SERVER_NAME = "mcp-rtt-server"
SERVER_VERSION = "0.1.0"

DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 8765


def create_server() -> Server:
    """Build the low-level Server with the same tool handlers as server.py."""
    server = Server(name=SERVER_NAME, version=SERVER_VERSION)
    tool_definitions = get_tool_definitions()

    @server.list_tools()
    async def list_tools() -> list[Tool]:
        return tool_definitions

    @server.call_tool()
    async def handle_call_tool(name: str, arguments: dict) -> list:
        return await call_tool(name, arguments)

    return server


def create_app(host: str = DEFAULT_HOST, port: int = DEFAULT_PORT) -> Starlette:
    """Build the Starlette ASGI app exposing the MCP server over SSE."""
    mcp_server = create_server()
    sse = SseServerTransport("/messages/")

    async def handle_sse(request: Request) -> Response:
        async with sse.connect_sse(request.scope, request.receive, request._send) as (read_stream, write_stream):
            await mcp_server.run(
                read_stream=read_stream,
                write_stream=write_stream,
                initialization_options=mcp_server.create_initialization_options(),
            )
        return Response()

    return Starlette(
        routes=[
            Route("/sse", endpoint=handle_sse),
            Mount("/messages/", app=sse.handle_post_message),
        ],
    )


def main() -> None:
    parser = argparse.ArgumentParser(description="RTT MCP server over SSE (shared daemon).")
    parser.add_argument("--host", default=DEFAULT_HOST)
    parser.add_argument("--port", type=int, default=DEFAULT_PORT)
    args = parser.parse_args()

    app = create_app()
    print(f"[rtt-mcp] SSE daemon on http://{args.host}:{args.port}/sse (shared J-Link owner)", flush=True)
    uvicorn.run(app, host=args.host, port=args.port, log_level="info")


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        rtt = get_instance()
        if rtt.is_connected():
            rtt.disconnect()
