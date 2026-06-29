// Package transport wires the RTT tools to the three serving modes:
//
//   - serve  (stdio, default): proxy every tool call to the shared daemon over
//     SSE (no local probe ownership); if no daemon is reachable, spawn a
//     resident local daemon first and then proxy through it. serve never owns
//     the J-Link directly — that is what keeps it from racing the extension.
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

// RunServe is the default (Claude Code) entry. It never owns the J-Link itself
// — that direct-ownership fallback is exactly what raced the VSCode extension
// for the single USB probe on a cold start. Instead it proxies through the
// shared daemon, spawning a resident local daemon first if none is reachable.
// J-Link ownership lives entirely in the daemon; serve and the extension are
// peers, so they never fight over the probe.
func RunServe(ctx context.Context) error {
	cfg := config.Load()

	// Fast path: shared daemon already up → proxy via SSE, no local probe.
	if daemonReachable(cfg) {
		fmt.Fprintln(os.Stderr, "[rtt] shared daemon reachable — proxying via SSE (no local J-Link open)")
		return RunBridge(ctx)
	}

	// Daemon not reachable. Only a local endpoint is worth spawning; a missing
	// remote daemon is a configuration issue, not something to spawn over.
	host, port, err := daemonHostPort(cfg)
	if err != nil {
		return fmt.Errorf("parse RTT_DAEMON_URL %q: %w", cfg.DaemonURL, err)
	}
	if !isLocalHost(host) {
		return fmt.Errorf("shared daemon at %s not reachable and non-local; refusing to spawn (point RTT_DAEMON_URL at 127.0.0.1 or start the daemon manually)", cfg.DaemonURL)
	}

	started, stderrPath, err := spawnDaemon(host, port)
	if err != nil || !started {
		// spawn may have lost the port race to another serve's daemon; re-probe
		// before giving up — the winner is just as good for proxying.
		if daemonReachable(cfg) {
			fmt.Fprintln(os.Stderr, "[rtt] another daemon won the race — proxying via SSE")
			return RunBridge(ctx)
		}
		hint := stderrHint(stderrPath)
		if err != nil {
			return fmt.Errorf("spawn shared daemon%s: %w", hint, err)
		}
		return fmt.Errorf("spawn shared daemon%s: child did not start", hint)
	}

	if !waitDaemonReady(cfg, daemonReadyTimeout, daemonReadyInterval) {
		return fmt.Errorf("spawned daemon did not become ready in %s%s", daemonReadyTimeout, stderrHint(stderrPath))
	}

	fmt.Fprintf(os.Stderr, "[rtt] spawned local daemon (stderr: %s) — proxying via SSE; daemon stays resident, POST :8765/shutdown to reclaim\n", stderrPath)
	return RunBridge(ctx)
}

// stderrHint formats a "(see <path>)" suffix so a failed spawn points at the
// daemon's captured stderr for diagnosis.
func stderrHint(path string) string {
	if path == "" {
		return ""
	}
	return fmt.Sprintf(" (see %s)", path)
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
