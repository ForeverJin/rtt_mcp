#!/usr/bin/env bash
# One-command bootstrap for the RTT-MCP server (macOS / Linux).
#
# Installs the Python package (console scripts on PATH), optionally the VSCode
# extension, and registers the `rtt` MCP server at Claude Code USER scope —
# workspace-independent and not tied to any MCU project.
#
# Run from the repo root (the folder containing pyproject.toml):
#   ./install.sh
set -euo pipefail

info()  { printf '\033[0;32m[install]\033[0m %s\n' "$1"; }
warn()  { printf '\033[0;33m[install] WARN:\033[0m %s\n' "$1"; }
fail()  { printf '\033[0;31m[install] FAIL:\033[0m %s\n' "$1" >&2; exit 1; }

# Resolve python (python3, then python).
if command -v python3 >/dev/null 2>&1; then PYTHON=python3
elif command -v python >/dev/null 2>&1; then PYTHON=python
else fail "Python not found on PATH. Install Python 3.10+ (https://www.python.org/downloads/) and re-run."; fi

# --- Step 1: Python version >= 3.10 ---
ver=$($PYTHON --version 2>&1 | sed 's/Python //')
major=$(echo "$ver" | cut -d. -f1)
minor=$(echo "$ver" | cut -d. -f2)
[ -n "$major" ] && [ -n "$minor" ] || fail "Could not parse Python version from '$ver'."
if [ "$major" -lt 3 ] || { [ "$major" -eq 3 ] && [ "$minor" -lt 10 ]; }; then
  fail "Need Python >= 3.10, found $ver."
fi
info "Python $ver OK"

# --- Step 2: non-editable pip install (editable masks the portable log path) ---
info "Installing package (non-editable)..."
$PYTHON -m pip install . || fail "pip install failed. Try: $PYTHON -m pip install --user ."
info "Package installed"

# --- Step 3: resolve console-script dir + verify the binary exists ---
scripts_dir=$($PYTHON -c 'import sysconfig;print(sysconfig.get_path("scripts"))')
bin="$scripts_dir/rtt-mcp-server"
[ -x "$bin" ] || { warn "Console script not found at $bin (pip may have used a dir off PATH)."; fail "rtt-mcp-server missing after install"; }
info "Console script: $bin"

# --- Step 4: pylink health check (DLL/dylib loads lazily — never hard-gate on SEGGER) ---
if $PYTHON -c "import pylink; pylink.JLink()" 2>/dev/null; then info "pylink-square import OK"
else warn "pylink import failed — install SEGGER J-Link software before connecting hardware"; fi

# --- Step 5: VSCode extension (best-effort) ---
vsix="$(dirname "$0")/vscode-rtt-mcp/vscode-rtt-mcp-0.1.0.vsix"
if command -v code >/dev/null 2>&1 && [ -f "$vsix" ]; then
  info "Installing VSCode extension..."
  code --uninstall-extension local.vscode-rtt-mcp >/dev/null 2>&1 || true
  code --install-extension "$vsix" --force >/dev/null 2>&1 || warn "code --install-extension failed"
  info "Extension installed (reload VSCode to activate)"
elif ! command -v code >/dev/null 2>&1; then
  warn "\`code\` CLI not found — install the extension manually: drag $vsix into VSCode's Extensions panel"
else
  warn "VSIX not found at $vsix — build it first with \`vsce package\` in vscode-rtt-mcp/"
fi

# --- Step 6: register MCP at Claude Code user scope ---
if command -v claude >/dev/null 2>&1; then
  info "Registering \`rtt\` at Claude Code user scope (absolute path, no cwd)..."
  claude mcp remove --scope user rtt >/dev/null 2>&1 || true
  claude mcp add --scope user rtt "$bin" || fail "claude mcp add failed"
  info "Registered. Verify with: claude mcp list"
else
  warn "\`claude\` CLI not found. Add this to ~/.claude.json manually (mcpServers):"
  warn "  \"rtt\": { \"type\": \"stdio\", \"command\": \"$bin\" }"
fi

# --- Step 7: smoke test ---
info "Smoke test: rtt-mcp-daemon --help"
"${scripts_dir}/rtt-mcp-daemon" --help 2>&1 | head -2 | sed 's/^/    /'

info ""
info "Done. RTT-MCP is now available in every Claude Code session (any workspace)."
info "To target a specific MCU per project:"
info "  claude mcp remove --scope user rtt; claude mcp add --scope user rtt \"$bin\" -e JLINK_DEVICE=HC32L19x"
