"""Entry point for running as `python -m mcp_rtt_server`."""

from .server import main

if __name__ == "__main__":
    import asyncio
    import sys
    from .jlink_rtt import get_instance

    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        # Clean shutdown
        rtt = get_instance()
        if rtt.is_connected():
            rtt.disconnect()
        sys.exit(0)
