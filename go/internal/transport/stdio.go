// Package transport wires the RTT tools to the three serving modes:
//
//   - serve  (stdio, default): if the shared daemon is reachable, proxy every
//     tool call to it over SSE (no local probe ownership); otherwise run the
//     MCP server directly, owning the J-Link in this process.
//   - daemon (HTTP/SSE): the single J-Link owner, serving the tools over SSE to
//     any number of clients (Claude Code + the VSCode extension share it).
//   - bridge (stdio↔SSE): a thin proxy the extension can spawn that forwards
//     stdio MCP to the daemon.
//
// The daemon+bridge split exists so two clients never fight over one USB probe.
package transport

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"rtt-mcp-server/internal/config"
	"rtt-mcp-server/internal/tools"
)

const (
	serverName    = "mcp-rtt-server"
	serverVersion = "0.1.0"
)

// impl is the server/client implementation identity advertised in the MCP
// initialize handshake.
func impl() *mcp.Implementation {
	return &mcp.Implementation{Name: serverName, Version: serverVersion}
}

// RunServe is the default (Claude Code) entry: proxy through the shared daemon
// when it is up, else fall back to owning the probe directly.
func RunServe(ctx context.Context) error {
	cfg := config.Load()
	if daemonReachable(cfg) {
		fmt.Fprintln(os.Stderr, "[rtt] shared daemon reachable — proxying via SSE (no local J-Link open)")
		if err := RunBridge(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "[rtt] daemon proxy failed (%v); falling back to direct J-Link\n", err)
		} else {
			return nil
		}
	}
	fmt.Fprintln(os.Stderr, "[rtt] shared daemon not reachable — opening J-Link directly (standalone owner)")
	srv := mcp.NewServer(impl(), nil)
	tools.Register(srv)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// daemonReachable is a cheap liveness probe: a GET on the SSE URL that resolves
// as soon as headers arrive (2xx/3xx = up), matching the extension's isDaemonUp.
func daemonReachable(cfg *config.Config) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: authTransport(cfg.AuthToken)}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, cfg.DaemonURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}
