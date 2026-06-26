#!/usr/bin/env bash
# One-command bootstrap for the RTT-MCP server (macOS / Linux).
#
# Builds the Go binary from source, optionally the VSCode extension, and
# registers the `rtt` MCP server at Claude Code USER scope — workspace-
# independent and not tied to any MCU project.
#
# Run from the repo root (the folder containing go/):
#   ./install.sh
set -euo pipefail

info()  { printf '\033[0;32m[install]\033[0m %s\n' "$1"; }
warn()  { printf '\033[0;33m[install] WARN:\033[0m %s\n' "$1"; }
fail()  { printf '\033[0;31m[install] FAIL:\033[0m %s\n' "$1" >&2; exit 1; }

# --- Step 1: locate go (PATH first, dev-box fallback for the msys env) ---
GO=""
if command -v go >/dev/null 2>&1; then
  GO="go"
elif [ -x "/c/Users/jy/go-sdk/go/bin/go.exe" ]; then
  GO="/c/Users/jy/go-sdk/go/bin/go.exe"
fi
[ -n "$GO" ] || fail "Go not found on PATH or at /c/Users/jy/go-sdk/go/bin/go.exe. Install Go >= 1.25 from https://go.dev/dl/"
info "Go: $($GO version | head -1)"

# --- Step 2: build the main binary (Claude Code consumes this) ---
REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$REPO_ROOT/bin"
mkdir -p "$BIN_DIR"
exe_name="rtt-mcp-server"
# -C changes cwd to ./go, so -o is interpreted relative to ./go; use an
# absolute path to land the binary in the repo-level bin/ directory.
GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" "$GO" build -C "$REPO_ROOT/go" -o "$BIN_DIR/$exe_name" .
[ -s "$BIN_DIR/$exe_name" ] || fail "binary missing or empty at $BIN_DIR/$exe_name"
info "Built: $BIN_DIR/$exe_name ($(du -h "$BIN_DIR/$exe_name" | cut -f1))"

# --- Step 3: SEGGER J-Link soft check (lazy-loaded; not fatal) ---
case "$(uname -s 2>/dev/null || echo Windows)" in
  Linux)   [ -f /opt/SEGGER/JLink/libjlinkarm.so ] || [ -f /usr/lib/libjlinkarm.so ] || warn "SEGGER J-Link library not found in default locations; install before connecting hardware" ;;
  Darwin)  [ -f /Applications/SEGGER/JLink/libjlinkarm.dylib ] || warn "SEGGER J-Link not found; install before connecting hardware" ;;
  *)       # Windows under msys; soft check skipped (install.ps1 handles it)
    ;;
esac

# --- Step 4: VSCode extension (build + install via the extension's own build.sh) ---
build_sh="$REPO_ROOT/vscode-rtt-mcp/build.sh"
if command -v code >/dev/null 2>&1 && [ -x "$build_sh" ]; then
  info "Building VSCode extension (Go binary + vsix)..."
  if vsix_file=$("$build_sh" 2>/dev/null | grep -E '\.vsix$' | tail -1) && [ -n "$vsix_file" ] && [ -f "$REPO_ROOT/vscode-rtt-mcp/$vsix_file" ]; then
    info "Installing $vsix_file..."
    code --uninstall-extension local.vscode-rtt-mcp >/dev/null 2>&1 || true
    code --install-extension "$REPO_ROOT/vscode-rtt-mcp/$vsix_file" --force >/dev/null 2>&1 || warn "code --install-extension failed"
    info "Extension installed (reload VSCode to activate)"
  else
    warn "Extension build did not produce a .vsix — run $build_sh manually to see the error"
  fi
elif ! command -v code >/dev/null 2>&1; then
  warn "\`code\` CLI not found — run $build_sh manually then drag the .vsix into VSCode's Extensions panel"
else
  warn "Build script missing at $build_sh — extension will not be installed"
fi

# --- Step 5: register MCP at Claude Code user scope (absolute path, no cwd) ---
if command -v claude >/dev/null 2>&1; then
  info "Registering \`rtt\` at Claude Code user scope (absolute path, no cwd)..."
  claude mcp remove --scope user rtt >/dev/null 2>&1 || true
  claude mcp add --scope user rtt "$BIN_DIR/$exe_name" || fail "claude mcp add failed"
  info "Registered. Verify with: claude mcp list"
else
  warn "\`claude\` CLI not found. Add this to ~/.claude.json manually (mcpServers):"
  warn "  \"rtt\": { \"type\": \"stdio\", \"command\": \"$BIN_DIR/$exe_name\" }"
fi

# --- Step 6: smoke test ---
info "Smoke test: $BIN_DIR/$exe_name help"
"$BIN_DIR/$exe_name" help 2>&1 | head -2 | sed 's/^/    /'

info ""
info "Done. RTT-MCP is now available in every Claude Code session (any workspace)."
info "To target a specific MCU per project:"
info "  claude mcp remove --scope user rtt; claude mcp add --scope user rtt \"$BIN_DIR/$exe_name\" -e JLINK_DEVICE=HC32L19x"
