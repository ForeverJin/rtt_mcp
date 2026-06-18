// Command rtt-mcp-server is the Go rewrite of the SEGGER RTT MCP server.
//
// It is a single binary with three modes:
//
//	serve   (default) stdio MCP server; proxies to the shared daemon when up,
//	         otherwise owns the J-Link directly. This is the Claude Code entry.
//	daemon  HTTP/SSE server; the single J-Link owner shared by all clients.
//	bridge  stdio↔SSE proxy; the entry the VSCode extension spawns.
//
// Pure Go (no cgo): the SEGGER JLinkARM library is loaded at runtime.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"rtt-mcp-server/internal/transport"
)

const usage = `rtt-mcp-server — SEGGER RTT MCP server (Go)

Usage:
  rtt-mcp-server [serve]        stdio MCP server (default; Claude Code entry)
  rtt-mcp-server daemon [-host H] [-port N]   SSE daemon (single J-Link owner)
  rtt-mcp-server bridge         stdio<->SSE proxy (VSCode extension entry)

Options:
  -h, --help                    show this help

Environment (all optional, see .env.example):
  JLINK_SERIAL, JLINK_DEVICE, JLINK_SPEED, RTT_CHANNEL,
  RTT_RING_BUFFER_SIZE, RTT_POLL_INTERVAL_MS,
  JLINK_RAM_START, JLINK_RAM_SIZE (hex), RTT_LOG_FILE,
  JLINK_LIB_PATH / RTT_LIB_PATH, RTT_DAEMON_URL, RTT_AUTH_TOKEN, RTT_MOCK
`

func main() {
	if err := run(); err != nil && err != errSilent {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

var errSilent = fmt.Errorf("silent")

func run() error {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}

	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		fmt.Print(usage)
		return nil
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch cmd {
	case "serve", "":
		return transport.RunServe(ctx)
	case "bridge":
		return transport.RunBridge(ctx)
	case "daemon":
		fs := flag.NewFlagSet("daemon", flag.ExitOnError)
		host := fs.String("host", "127.0.0.1", "bind host")
		port := fs.Int("port", 8765, "bind port")
		if err := fs.Parse(args); err != nil {
			return err
		}
		return transport.RunDaemon(ctx, *host, *port)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Print(usage)
		return errSilent
	}
}
