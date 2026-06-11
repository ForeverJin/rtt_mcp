# vscode-rtt-mcp

A VSCode extension that wraps [`mcp-rtt-server`](../mcp-rtt-server) so you can drive J-Link RTT from the editor without typing commands in a terminal or invoking MCP tools by hand.

## Features

- **Status bar indicator** — `RTT` icon always visible, reflects connection + monitor state
- **Quick menu** — click the status bar (or run `RTT: Show Quick Menu`) for a one-click list of all operations
- **Output channel** — `RTT` channel streams data when monitor is on
- **10 commands** — Connect, Disconnect, Read, Write, List Devices, Status, Clear, Toggle Monitor, Open Output, Open Log

## Requirements

- VSCode 1.85+
- Node.js 18+ (only for building, not at runtime)
- Python 3.10+ with `mcp-rtt-server` installed
- J-Link probe + target running SEGGER RTT

## Build & Install

```bash
cd vscode-rtt-mcp
npm install
npm run build
```

### Option A — Symlink into extensions folder (dev)

```bash
# Windows (PowerShell)
& "$env:USERPROFILE\.vscode\extensions\vscode-rtt-mcp" -ItemType Junction -Target (Resolve-Path .)
```

```bash
# macOS / Linux
ln -s "$(pwd)" ~/.vscode/extensions/vscode-rtt-mcp
```

### Option B — Package as VSIX

```bash
npm run package
code --install-extension vscode-rtt-mcp-0.1.0.vsix
```

## Configuration

Open Settings (Ctrl+,) and search "RTT MCP", or add to `.vscode/settings.json`:

```json
{
  "rtt-mcp.pythonPath": "python",
  "rtt-mcp.serverCwd": "",
  "rtt-mcp.serverArgs": ["-m", "mcp_rtt_server.server"],
  "rtt-mcp.pollIntervalMs": 300,
  "rtt-mcp.autoConnect": false
}
```

> **注意**：`pythonPath` 默认使用 PATH 中的 `python`，如果不在 PATH 中需改为完整路径。`serverCwd` 留空表示自动定位，如需指定可填 `mcp-rtt-server` 的绝对路径。

| Key | Default | Description |
|-----|---------|-------------|
| `rtt-mcp.pythonPath` | `python` | Python interpreter (on PATH or full path) |
| `rtt-mcp.serverCwd` | (empty) | MCP server working dir (auto-detect if empty) |
| `rtt-mcp.serverArgs` | `["-m", "mcp_rtt_server.server"]` | Server CLI args |
| `rtt-mcp.pollIntervalMs` | `300` | Monitor poll interval (ms) |
| `rtt-mcp.autoConnect` | `false` | Connect on activation |

## Usage

1. **Status bar** (bottom-right): click the `RTT` icon
2. **Command palette** (Ctrl+Shift+P): type `RTT:`
3. **Default workflow**:
   - Click `RTT` → `Connect to J-Link`
   - Click `RTT` → `Toggle Continuous Monitor` to stream data
   - `Output` panel (Ctrl+Shift+U) → select `RTT` channel

### Status bar states

| Icon | Meaning |
|------|---------|
| `$(debug-disconnect) RTT` | Disconnected |
| `$(debug-start) RTT` | Connected, not monitoring |
| `$(eye) RTT ●` | Connected + monitoring |

## Architecture

```
VSCode Extension (TypeScript)
   │
   ├── status bar / commands / output channel (VSCode API)
   │
   └── RttProvider
         ├── spawns Python child process: mcp_rtt_server.server
         ├── speaks MCP/JSON-RPC over stdio (Content-Length framed)
         └── polls rtt_read at pollIntervalMs for monitor mode
```

The MCP client is implemented from scratch in `src/mcpClient.ts` (no external dependency on `@modelcontextprotocol/sdk`).

## Troubleshooting

- **"MCP request timed out"** — Python or `pylink-square` not installed in the configured `serverCwd`; verify `python -m mcp_rtt_server.server` works from a terminal first.
- **"RTT CB not found"** — target firmware didn't call `SEGGER_RTT_Init()`, or RTT buffers in `SEGGER_RTT_Conf.h` are too small.
- **Status bar doesn't change** — the extension defers spawning the Python process until the first command; click `Connect` to start it.
- **Multiple extensions competing** — only one process can hold the J-Link; if `.claude/settings.json` also runs the server, disconnect there first.

## Testing

Three layers, from fastest to most realistic.

### 1. Automated tests (no hardware) — `npm test`

A Python mock server (`test/mock_server.py`) speaks the full MCP/JSON-RPC protocol and simulates all 7 RTT tools with an in-memory text buffer. A background thread injects fake RTT data so monitor mode has output. The Node integration tests (`test/integration.test.js`) spawn the mock, exercise every tool, and verify the full lifecycle.

```bash
cd vscode-rtt-mcp
npm install
npm test          # builds TS, then runs 10 tests in ~3s
```

Override the Python path if needed:
```bash
MCP_PYTHON=python npm test      # macOS / Linux
$env:MCP_PYTHON = "python"; npm test   # PowerShell
```

What gets tested:
- `McpClient`: initialize handshake, `tools/list` returns 7 expected names, `tools/call` echo, unknown tool throws, timeout handling, `stop()` rejects pending requests
- `RttProvider`: full connect/read/status/write/clear/disconnect lifecycle, monitor mode streams data, list devices, read-before-connect throws

### 2. Manual smoke test in Extension Development Host

```bash
npm run mock      # starts mock server in a terminal — leave it running
```

In VSCode:
1. Open this folder (`vscode-rtt-mcp/`)
2. Press **F5** → "Run Extension" launches a new VSCode window with the extension loaded
3. In the new window: **Ctrl+Shift+P** → "RTT: Connect to J-Link"
4. Status bar shows `$(debug-start) RTT` when connected
5. Run "RTT: Toggle Continuous Monitor" — output channel streams `[Mock #0001] tick=1 ...` every 300ms
6. Try "RTT: Write to Down-buffer" → `hello` → expect "Wrote 5 bytes to RTT channel 0"

To point the extension at the mock server, add to `.vscode/launch.json` or use Settings UI:
```json
{
  "rtt-mcp.serverCwd": "${workspaceFolder}/vscode-rtt-mcp/test"
}
```

### 3. Real hardware

With firmware flashed and target powered:
```bash
npm run build
# install extension (see Build & Install above)
# status bar → Connect to J-Link
```

The extension uses the configured `pythonPath` and `serverCwd` to launch the real `mcp-rtt-server`, which talks to the J-Link probe via pylink.

## Files

- `src/extension.ts` — commands, status bar, output channel
- `src/mcpClient.ts` — MCP/JSON-RPC over stdio (no SDK dependency)
- `src/rttProvider.ts` — RTT-specific wrapper with monitor loop
- `package.json` — manifest, command + setting contributions
- `tsconfig.json` — TypeScript config (CJS, ES2022)
