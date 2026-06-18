# rtt-mcp-server (Go)

Pure-Go rewrite of the Python `mcp-rtt-server`. A single static binary that
loads the SEGGER J-Link library at runtime (no cgo, no Python, no pip). This is
the `go版本` branch — the Python implementation under `../src/` remains the
default on `main` until this is verified on hardware and the cutover is done.

## Why

The Python version's `pip install --user` lands console scripts off-PATH on
Windows, so Claude Code's `child_process.spawn` can't launch it by bare name —
the MCP server must be registered with an absolute path, and editing source
requires re-syncing into site-packages. A single Go binary removes that whole
layer: register by bare name, rebuild = overwrite one file, cross-compile to
macOS/Linux from one host.

## Build

```bash
# toolchain not on PATH on this machine; see memory note go-toolchain-goproxy
export PATH="/c/Users/jy/go-sdk/go/bin:$PATH"
export GOPROXY="https://goproxy.cn,direct"   # proxy.golang.org is unreachable here
export GOSUMDB=off
go build -o rtt-mcp-server .
```

Pure Go: `CGO_ENABLED=0` by default, and cross-compiles cleanly:

```bash
GOOS=linux  GOARCH=amd64 go build -o rtt-mcp-server-linux-amd64  .
GOOS=darwin GOARCH=arm64 go build -o rtt-mcp-server-darwin-arm64 .
```

## Modes (same architecture as the Python version)

```
rtt-mcp-server [serve]            stdio MCP (default; Claude Code entry)
rtt-mcp-server daemon [-host -port]   SSE daemon (single J-Link owner)
rtt-mcp-server bridge             stdio<->SSE proxy (VSCode extension entry)
```

The daemon owns the probe; `serve` proxies through it when up (so Claude Code
and the VSCode extension share one J-Link), else owns the probe directly.

## Layout

```
main.go                       subcommand dispatch
internal/config/              env knobs (JLINK_*, RTT_*, defaults)
internal/jlink/               purego FFI into JLinkARM/JLink_x64 (iface, dynload,
                              api signatures, backend, mock for hardware-free tests)
internal/rttcore/             singleton engine: monitor goroutine, triple-sink
                              (rotating log + stderr + ring buffer), connect sequence
internal/tools/               9 MCP tools (names/schemas/text parity with Python)
internal/transport/           stdio / daemon SSE+HTTP / bridge proxy
```

## Status (2026-06-18)

Verified without hardware: cross-compile (no cgo), stdio MCP handshake +
`tools/list` parity, daemon `/sse` + `/shutdown`, serve→bridge→daemon proxy,
all three read semantics via the mock backend, and **real `JLink_x64.dll` load +
symbol registration + SEGGER function calls** (the J-Link window appeared).

Pending: real RTT data round-trip on HC32L196 hardware; then the cutover
(remove Python `src/`, `pyproject.toml`, old installers; rewrite installers to
download this binary; repoint the VSCode extension at it).

## Note: 64-bit SEGGER library

A 64-bit build must load `JLink_x64.dll`, **not** `JLinkARM.dll` (the latter is
32-bit and fails with error 193). `internal/jlink/dynload.go` picks the
arch-appropriate candidate.
