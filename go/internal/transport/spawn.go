package transport

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"rtt-mcp-server/internal/config"
)

// daemonReadyTimeout / daemonReadyInterval tune the readiness poll after a cold
// spawn. Binding the HTTP listener takes well under a second; 15s is a generous
// ceiling that also covers slow first-time J-Link DLL probing on a cold disk.
const (
	daemonReadyTimeout  = 15 * time.Second
	daemonReadyInterval = 200 * time.Millisecond
)

// spawnDaemon launches a resident daemon child (same binary, "daemon" subcommand)
// bound to host:port, fully detached from this process. It returns started=true
// once the child has been launched; the child then owns the J-Link exclusively
// and outlives this process. stderrPath is where the child's stderr is captured
// for diagnostics (DLL load failure, port conflict, …). serve never owns the
// probe directly — it always proxies through the daemon, which is what keeps it
// from racing the VSCode extension for the single USB probe.
func spawnDaemon(host, port string) (started bool, stderrPath string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return false, "", fmt.Errorf("locate own executable: %w", err)
	}

	stderrPath = daemonLogPath()
	stderrFile, openErr := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if openErr != nil {
		stderrPath = ""
		stderrFile = nil
	}

	// Discard stdout (a daemon banner must never reach serve's MCP stdout, which
	// is the JSON-RPC channel), and keep stderr for diagnostics. Leaving cmd.Env
	// nil makes the child inherit the parent environment, so JLINK_*/RTT_* and
	// JLINK_LIB_PATH propagate for free.
	devnull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)

	cmd := exec.Command(exe, "daemon", "-host", host, "-port", port)
	cmd.SysProcAttr = sysProcAttrDetached()
	if devnull != nil {
		cmd.Stdin = devnull
		cmd.Stdout = devnull
	}
	if stderrFile != nil {
		cmd.Stderr = stderrFile
	}

	if startErr := cmd.Start(); startErr != nil {
		if stderrFile != nil {
			stderrFile.Close()
		}
		if devnull != nil {
			devnull.Close()
		}
		return false, stderrPath, fmt.Errorf("start daemon: %w", startErr)
	}

	// Start has duplicated these fds into the child, so the parent handles are no
	// longer required. Close the null device now; hand the stderr file to the
	// wait goroutine so it is closed exactly when the daemon exits (no zombies,
	// no blocking serve).
	if devnull != nil {
		devnull.Close()
	}
	pid := cmd.Process.Pid
	go func() {
		if waitErr := cmd.Wait(); waitErr != nil {
			fmt.Fprintf(os.Stderr, "[rtt] spawned daemon (pid %d) exited: %v\n", pid, waitErr)
		}
		if stderrFile != nil {
			stderrFile.Close()
		}
	}()

	return true, stderrPath, nil
}

// waitDaemonReady polls the daemon liveness endpoint until it answers or the
// timeout elapses. The daemon serves the instant its HTTP listener binds — the
// J-Link is connected lazily by the jlink_connect tool, never at startup.
func waitDaemonReady(cfg *config.Config, timeout, interval time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if daemonReachable(cfg) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
}

// daemonHostPort extracts the host and port serve should spawn/listen on from
// RTT_DAEMON_URL (default http://127.0.0.1:8765/sse). The /sse path is ignored.
func daemonHostPort(cfg *config.Config) (host, port string, err error) {
	u, err := url.Parse(cfg.DaemonURL)
	if err != nil {
		return "", "", err
	}
	host = u.Hostname()
	port = u.Port()
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "8765"
	}
	return host, port, nil
}

// isLocalHost reports whether host is a loopback literal. serve only auto-spawns
// a daemon for local endpoints; a non-local RTT_DAEMON_URL that is unreachable
// is a configuration issue, not something to spawn over.
func isLocalHost(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// daemonLogPath is where the spawned daemon's stderr is captured. It reuses the
// same per-platform state directory as the RTT broadcast log so diagnostics live
// alongside rtt_output.log.
func daemonLogPath() string {
	base, err := stateDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "mcp-rtt-server")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return filepath.Join(os.TempDir(), "mcp-rtt-server-daemon_stderr.log")
	}
	return filepath.Join(dir, "daemon_stderr.log")
}

// stateDir mirrors rttcore.stateDir: the per-platform base for persistent
// state/logs (%LOCALAPPDATA% on Windows, ~/Library/Caches on macOS, XDG_STATE_HOME
// or ~/.local/state on Linux). Duplicated here to avoid a cross-package export.
func stateDir() (string, error) {
	switch runtime.GOOS {
	case "windows", "darwin":
		return os.UserCacheDir()
	default:
		if v := os.Getenv("XDG_STATE_HOME"); v != "" {
			return v, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "state"), nil
	}
}
