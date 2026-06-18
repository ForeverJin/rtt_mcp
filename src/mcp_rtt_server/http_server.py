"""SSE (HTTP) MCP server for Segger RTT via J-Link.

Runs the SAME tool surface as ``server.py`` but over an SSE transport so a single
long-lived process can serve multiple clients (Claude Code + the VSCode extension)
that all share one J-Link connection through the module-global ``_rtt_instance``
singleton. This is the "single owner" daemon: only this process opens the J-Link.

Entry:  python -m mcp_rtt_server.http_server [--host 127.0.0.1] [--port 8765]

Shutdown:
  POST /shutdown   — releases the J-Link (rtt_stop + close) and stops the server.
                     This is the supported way for the VSCode extension to tear the
                     daemon down: on Windows, ``child.kill()`` is ``TerminateProcess``,
                     a hard kill that bypasses Python cleanup (atexit, signal handlers)
                     and would otherwise leave the probe open with RTT started. The
                     extension calls /shutdown first and only falls back to kill().
  Ctrl-C / normal exit — an atexit hook also releases the J-Link as a backstop.

Auth (optional): set the ``RTT_AUTH_TOKEN`` env var to require an
``Authorization: Bearer <token>`` header on /sse, /messages/ and /shutdown. When
unset the daemon is open to any local process (as before) — fine for a single-user
dev box. Clients that opt in must present the same token (bridge.py forwards
RTT_AUTH_TOKEN automatically).
"""

from __future__ import annotations

import argparse
import asyncio
import atexit
import os

import uvicorn
from mcp.server import Server
from mcp.server.sse import SseServerTransport
from mcp.types import Tool
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse, Response
from starlette.routing import Mount, Route
from starlette.types import Receive, Scope, Send

from .jlink_rtt import get_instance
from .tools import get_tool_definitions, call_tool

SERVER_NAME = "mcp-rtt-server"
SERVER_VERSION = "0.1.0"

DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 8765

# Holds the running uvicorn.Server so the /shutdown route can request a clean stop.
# Set once in main(); read defensively elsewhere (None during tests / before start).
_running_server: "uvicorn.Server | None" = None


def _auth_token() -> str | None:
    """Return the configured bearer token, or None when auth is disabled."""
    return os.environ.get("RTT_AUTH_TOKEN") or None


def _authorized(request: Request) -> bool:
    """Check the bearer token when RTT_AUTH_TOKEN is configured; allow all if not."""
    expected = _auth_token()
    if not expected:
        return True
    header = request.headers.get("authorization", "")
    prefix = "Bearer "
    return header.startswith(prefix) and header[len(prefix):].strip() == expected


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


def _set_should_exit() -> None:
    """Request the uvicorn server to exit (called on a short delay after /shutdown
    so the HTTP response can flush first)."""
    if _running_server is not None:
        _running_server.should_exit = True


def _cleanup_jlink() -> None:
    """Release the J-Link if this process still holds it.

    Registered via atexit so Ctrl-C and normal process exit free the probe. Windows
    ``TerminateProcess`` (the extension's fallback) bypasses atexit entirely, which
    is exactly why /shutdown exists as the primary teardown path.
    """
    try:
        rtt = get_instance()
        if rtt.is_connected():
            rtt.disconnect()
    except Exception:
        pass


def create_app() -> Starlette:
    """Build the Starlette ASGI app exposing the MCP server over SSE."""
    mcp_server = create_server()
    sse = SseServerTransport("/messages/")

    async def handle_sse(request: Request) -> Response:
        if not _authorized(request):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        async with sse.connect_sse(request.scope, request.receive, request._send) as (read_stream, write_stream):
            await mcp_server.run(
                read_stream=read_stream,
                write_stream=write_stream,
                initialization_options=mcp_server.create_initialization_options(),
            )
        return Response()

    async def handle_post(scope: Scope, receive: Receive, send: Send) -> None:
        # ASGI signature: required by Mount("/messages/", app=handle_post).
        # sse.handle_post_message also takes (scope, receive, send), so we forward
        # them through (the previous version wrapped it as `(request) -> Response`
        # which crashed at runtime with a signature mismatch → 500 on every POST).
        request = Request(scope, receive)
        if not _authorized(request):
            response = JSONResponse({"error": "unauthorized"}, status_code=401)
            await response(scope, receive, send)
            return
        await sse.handle_post_message(scope, receive, send)

    async def handle_shutdown(request: Request) -> Response:
        # Same gate as the other endpoints so a token-enabled daemon can't be
        # shut down by an unauthenticated local process.
        if not _authorized(request):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        # Release the J-Link first (blocking pylink I/O -> threadpool). This is the
        # whole point of the route: it frees the probe before a hard kill can strand
        # it. disconnect() is idempotent if nothing is connected.
        rtt = get_instance()
        if rtt.is_connected():
            await asyncio.to_thread(rtt.disconnect)
        # Ask uvicorn to exit shortly, after the response is flushed.
        loop = asyncio.get_running_loop()
        loop.call_later(0.1, _set_should_exit)
        return JSONResponse({"status": "shutting down"})

    return Starlette(
        routes=[
            Route("/sse", endpoint=handle_sse),
            Mount("/messages/", app=handle_post),
            Route("/shutdown", endpoint=handle_shutdown, methods=["POST"]),
        ],
    )


def main() -> None:
    global _running_server
    parser = argparse.ArgumentParser(description="RTT MCP server over SSE (shared daemon).")
    parser.add_argument("--host", default=DEFAULT_HOST)
    parser.add_argument("--port", type=int, default=DEFAULT_PORT)
    args = parser.parse_args()

    app = create_app()
    # Use Server(config).run() rather than uvicorn.run() so we can hold the server
    # reference and let /shutdown request a clean stop.
    config = uvicorn.Config(app, host=args.host, port=args.port, log_level="info")
    _running_server = uvicorn.Server(config)
    atexit.register(_cleanup_jlink)

    auth_note = " (auth: bearer token required)" if _auth_token() else " (auth: OPEN)"
    print(
        f"[rtt-mcp] SSE daemon on http://{args.host}:{args.port}/sse "
        f"(shared J-Link owner){auth_note}",
        flush=True,
    )
    print("[rtt-mcp] POST /shutdown to release the J-Link and stop cleanly.", flush=True)
    _running_server.run()


if __name__ == "__main__":
    main()
