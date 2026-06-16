"""stdio <-> SSE bridge for the rtt MCP server.

Speaks MCP over stdio on one side and forwards every call to the shared SSE daemon
on the other. The daemon (not this bridge) owns the J-Link, so clients that proxy
through it never open the probe directly and there is no contention.

Two entry points use this proxy:
  * ``python -m mcp_rtt_server.bridge``  — standalone bridge (used by the VSCode
    extension, which keeps its existing line-delimited-JSON stdio client).
  * ``python -m mcp_rtt_server.server``  — delegates to ``run_proxy_over_stdio()``
    when the daemon is reachable, so Claude Code shares the single owner too.

Env:
  RTT_DAEMON_URL   (default http://127.0.0.1:8765/sse)
  RTT_AUTH_TOKEN   (optional bearer token forwarded to the daemon)
"""

from __future__ import annotations

import asyncio
import os
import sys
import urllib.request

from mcp.client.session import ClientSession
from mcp.client.sse import sse_client
from mcp.server import Server
from mcp.server.stdio import stdio_server

DEFAULT_URL = "http://127.0.0.1:8765/sse"


def daemon_url() -> str:
    """Return the configured daemon SSE URL."""
    return os.environ.get("RTT_DAEMON_URL", DEFAULT_URL)


def daemon_headers() -> dict[str, str]:
    """Return request headers, forwarding the bearer token when auth is opted in."""
    headers: dict[str, str] = {}
    token = os.environ.get("RTT_AUTH_TOKEN")
    if token:
        headers["Authorization"] = f"Bearer {token}"
    return headers


def is_daemon_reachable(url: str | None = None, timeout: float = 0.5) -> bool:
    """Cheap liveness probe: a GET on the SSE URL that resolves as soon as the
    connection is accepted (before the event stream). Returns True if the daemon
    answers with a 2xx/3xx within ``timeout`` seconds.

    Used by ``server.py`` to decide whether to proxy through the shared daemon
    (single J-Link owner, no contention with the VSCode extension) or to fall back
    to opening the probe directly (a Claude-only session with no daemon running).
    Mirrors the VSCode extension's ``isDaemonUp()``.
    """
    target = url or daemon_url()
    try:
        req = urllib.request.Request(target, headers=daemon_headers(), method="GET")
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return 200 <= resp.status < 400
    except Exception:
        return False


async def run_proxy_over_stdio(url: str | None = None) -> None:
    """Forward MCP-over-stdio to the SSE daemon.

    Used by the bridge entry point and by ``server.py`` when the daemon is
    reachable, so a client never opens a second J-Link.
    """
    target = url or daemon_url()
    async with sse_client(target, headers=daemon_headers()) as (read, write):
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


async def main() -> None:
    await run_proxy_over_stdio()


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        sys.exit(0)
