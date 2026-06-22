#!/usr/bin/env bash
# Build the Go daemon binary then repackage the VSCode extension.
#
# Run from anywhere — resolves paths relative to this script:
#   ./build.sh
#
# Steps:
#   1. Locate `go` (PATH first, then the local SDK at C:/Users/jy/go-sdk).
#   2. `go build` the daemon into ./bin/<rtt-mcp-server[.exe]>.
#   3. Verify the binary exists (fail-fast if step 2 silently produced nothing).
#   4. `npx @vscode/vsce package` so the binary is bundled in the .vsix.
#
# After this script, install with:
#   code --install-extension vscode-rtt-mcp-*.vsix --force
set -euo pipefail

cd "$(dirname "$0")"
EXT_DIR="$(pwd)"
GO_DIR="$(cd "$EXT_DIR/../go" && pwd)"
BIN_DIR="$EXT_DIR/bin"

# --- Locate Go ---
GO=""
if command -v go >/dev/null 2>&1; then
  GO="go"
elif [ -x "/c/Users/jy/go-sdk/go/bin/go.exe" ]; then
  GO="/c/Users/jy/go-sdk/go/bin/go.exe"
fi
[ -n "$GO" ] || { echo "[build] FAIL: Go not found on PATH or at /c/Users/jy/go-sdk/go/bin/go.exe" >&2; exit 1; }

# --- Determine output name (extension expects .exe on Windows) ---
exe_name="rtt-mcp-server"
case "$OSTYPE" in
  msys*|cygwin*|win32*) exe_name="rtt-mcp-server.exe" ;;
esac

# --- Build ---
mkdir -p "$BIN_DIR"
echo "[build] Compiling Go binary (GOPROXY=${GOPROXY:-https://goproxy.cn,direct})..."
# -C: tell go to use this as the working directory (avoids Git Bash path
# normalisation issues where pwd's POSIX path gets mangled back to a
# parent dir without the /go suffix when passed as a positional arg).
GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" "$GO" build -C "$GO_DIR" -o "$BIN_DIR/$exe_name" .

# --- Verify (this is the missing step that bit us before) ---
[ -s "$BIN_DIR/$exe_name" ] || { echo "[build] FAIL: binary missing or empty at $BIN_DIR/$exe_name" >&2; exit 1; }
size=$(du -h "$BIN_DIR/$exe_name" | cut -f1)
echo "[build] Binary: $BIN_DIR/$exe_name ($size)"

# --- Package vsix ---
echo "[build] Packaging vsix..."
npx --yes @vscode/vsce package

# --- Verify the .vsix actually contains the binary ---
vsix_file="$(ls -1t vscode-rtt-mcp-*.vsix 2>/dev/null | head -1)"
[ -n "$vsix_file" ] || { echo "[build] FAIL: no .vsix produced" >&2; exit 1; }
if ! unzip -l "$vsix_file" 2>/dev/null | grep -q "extension/bin/$exe_name"; then
  echo "[build] FAIL: $vsix_file does NOT contain bin/$exe_name — packaging bug" >&2
  exit 1
fi
echo "[build] Verified: $vsix_file contains bin/$exe_name"
echo "[build] Done. Install with:"
echo "    code --install-extension $vsix_file --force"