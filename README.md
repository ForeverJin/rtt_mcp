# MCP Server for Segger RTT via J-Link

An MCP server that provides access to Segger RTT (Real Time Transfer) through a J-Link debugger. This allows an LLM (like Claude) to interact with embedded devices by reading and writing RTT data.

## Features

- **Shared single-owner daemon** — one long-lived SSE process owns the J-Link; Claude Code **and** the VSCode extension connect as clients, so they never fight over the single physical probe
- **Standalone fallback** — a Claude-only session with no daemon running opens the J-Link directly (auto-detected; no config change needed)
- **RTT Read/Write** — read up-buffer output, write commands to the down-buffer
- **Multi-consumer reads** — a draining ring buffer (`rtt_read`) **plus** a non-draining broadcast log (`rtt_read_log` / `rtt_read_raw`), so a continuous monitor never steals bytes from an on-demand read
- **Real-time display** — RTT data is continuously printed to terminal + a rotating log file while monitoring
- **Optional auth** — opt-in bearer token (`RTT_AUTH_TOKEN`) gates `/sse`, `/messages/` and `/shutdown`
- **Graceful shutdown** — `POST /shutdown` releases the J-Link and stops the daemon cleanly (works around Windows `TerminateProcess` stranding the probe)

## Prerequisites

- Python 3.10+
- SEGGER J-Link Software installed (provides `JLinkARM.dll`/`libjlinkarm.so`)
- A J-Link compatible debugger
- Target device running SEGGER RTT

## Installation

```bash
# Clone or navigate to this directory
cd mcp-rtt-server

# Install dependencies
pip install -e .
```

## Usage

### Step 1: 安装

```bash
cd mcp-rtt-server
pip install -e .
```

### Step 2: 启动守护进程（可选，推荐）

守护进程是"单一 J-Link 拥有者"。VSCode 扩展会自动启动它；手动调试/独立使用时也可以自己起：

```bash
python -m mcp_rtt_server.http_server --host 127.0.0.1 --port 8765
```

> 不启动也没关系：Claude Code（`server.py`）会探测守护进程，可达则经它代理，不可达则直接开 J-Link（独立会话模式）。

### Step 3: 启动 MCP Inspector（调试/测试用）

**终端 A — 启动 MCP 服务器：**

```bash
npx @modelcontextprotocol/inspector python -m mcp_rtt_server.server
```

启动后会输出一个本地网址（如 `http://localhost:6274/?...`），在浏览器打开。

**终端 B — 实时查看 RTT 数据：**

```powershell
# PowerShell
Get-Content .\rtt_output.log -Wait
```

```bash
# Git Bash / WSL
tail -f ./rtt_output.log
```

### Step 4: 使用工具

在浏览器 Inspector 中按顺序调用：

1. **`jlink_connect`** — 连接 J-Link 和目标设备，启动 RTT 监控
2. **`rtt_read`** — 读取环形缓冲区累积的 RTT 数据（读后清空）
3. **`rtt_read_log` / `rtt_read_raw`** — 读取广播日志（多消费者安全，不互相偷数据）
4. **`rtt_write`** — 向设备发送数据（需固件支持接收）
5. **`jlink_status`** — 查看连接状态和缓冲区信息
6. **`jlink_disconnect`** — 断开连接

### Claude Code 集成

在项目 `.claude/settings.json` 中添加：

```json
{
  "mcpServers": {
    "rtt": {
      "command": "python",
      "args": ["-m", "mcp_rtt_server.server"],
      "cwd": "."
    }
  }
}
```

> **注意**：如果 `python` 不在 PATH 中，需替换为完整路径（如 `C:\Python313\python.exe` 或 `/usr/bin/python3`）。

> **单拥有者行为**：`server.py` 启动时探测守护进程（`http://127.0.0.1:8765/sse`）——可达则经 SSE 代理所有工具调用（**不开第二个 J-Link**），与 VSCode 扩展共享同一连接，零争用；守护进程未运行则自动回退为直接开 J-Link（Claude-only 会话）。无需手动选择模式。

重启 Claude Code 会话后，可直接用自然语言与设备交互，例如：
- "连接 RTT 读取设备数据"
- "查看设备状态"

### Claude Desktop 集成

编辑 `%APPDATA%\Claude\claude_desktop_config.json`：

```json
{
  "mcpServers": {
    "rtt": {
      "command": "python",
      "args": ["-m", "mcp_rtt_server.server"],
      "cwd": "."
    }
  }
}
```

> **注意**：`cwd` 需要改为本项目的实际绝对路径，因为 Claude Desktop 不会自动定位到项目目录。

## Available Tools

| Tool | Description |
|------|-------------|
| `jlink_connect` | Connect to J-Link and start RTT monitoring |
| `jlink_disconnect` | Disconnect from J-Link |
| `jlink_status` | Get connection status and buffer info |
| `rtt_list_devices` | List available J-Link devices |
| `rtt_read` | Read accumulated RTT data from the **draining** ring buffer (read-once) |
| `rtt_read_log` | Read the tail of the broadcast log (non-draining; full output even while another client streams) |
| `rtt_read_raw` | Read new log bytes from a byte offset (non-draining, multi-consumer safe; returns `{data, next_offset}`) |
| `rtt_write` | Write data to RTT down-buffer |
| `rtt_clear` | Clear the RTT ring buffer |

> **读哪个？** 单消费者随手读用 `rtt_read`；持续监视器、或与其它客户端（如 VSCode 扩展）同时读时，用 `rtt_read_log` / `rtt_read_raw`，互不偷数据。

## Example Session

```
> jlink_connect(device="HC32L19x")
Connected to J-Link device 'HC32L19x' (serial: 12345678, speed: 4000 kHz)
RTT monitoring started on channel 0

> rtt_read()
[OSAL] HC32L19x OSAL starting...
[RTT] SEGGER RTT initialized
[OSAL] Registering tasks...
[OSAL] Starting scheduler...

> rtt_write(channel=0, data="status\r\n")
Wrote 7 bytes to RTT channel 0
```

## Architecture

单一 J-Link 拥有者模型：守护进程 `http_server.py` 是唯一 `pylink.open()` 的进程。Claude Code 与 VSCode 扩展都作为客户端连到它，永不开第二个 J-Link，因此不会争抢探针、也不会互相偷走 RTT 数据。

```
  ┌──────────────┐        ┌────────────────────┐
  │  Claude Code │ stdio  │   server.py        │
  │              │────────│  探测 daemon:        │
  └──────────────┘        │   可达→SSE 代理     │
                          │   不可达→直接开 J-Link│
                          └─────────┬──────────┘
                                    │ (可达时)
  ┌──────────────┐        ┌─────────▼──────────┐      ┌──────────────┐
  │ VSCode 扩展  │ stdio  │  bridge.py         │ SSE  │ http_server  │
  │ (RttProvider)│────────│  (stdio↔SSE 桥)    │─────│  (DAEMON)    │
  └──────────────┘        └────────────────────┘      │ 单一拥有者   │
                                                      └──────┬───────┘
                                                             │ pylink
                                                             ▼
                                                      ┌──────────────┐
                                                      │ JLinkARM.dll │
                                                      └──────┬───────┘
                                                             │ USB
                                                             ▼
                                                      ┌──────────────┐
                                                      │ J-Link Probe │── SWD ──▶ Target (HC32L19x, SEGGER RTT)
                                                      └──────────────┘
```

**模块职责**

| 模块 | 角色 |
|---|---|
| `http_server.py` | 守护进程，SSE 传输，**唯一**持有 J-Link；`POST /shutdown` 优雅释放 |
| `bridge.py` | stdio↔SSE 桥（VSCode 扩展用）；提供 `is_daemon_reachable()` 探测 + `run_proxy_over_stdio()` |
| `server.py` | Claude Code 入口；daemon 可达则代理，不可达则回退为直接拥有者 |
| `jlink_rtt.py` | pylink 封装：后台监视线程、环形缓冲、广播日志、轮转 |
| `tools.py` | 9 个 MCP 工具；所有阻塞 pylink 调用经 `asyncio.to_thread` 卸载到线程 |

### 关闭与 J-Link 释放

`POST /shutdown`（可选 `Authorization: Bearer <token>`）→ `rtt.disconnect()`（`rtt_stop` + `close`）→ uvicorn 干净退出。VSCode 扩展停掉守护进程时先调它，再回退到 `kill()` —— 这避免了 Windows `TerminateProcess` 硬杀导致 J-Link 卡在 RTT-started 状态。`atexit` 钩子作为 Ctrl-C / 正常退出的兜底。

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `JLINK_SERIAL` | (none) | J-Link serial number |
| `JLINK_DEVICE` | `HC32L19x` | Target device name |
| `JLINK_SPEED` | `4000` | SWD speed in kHz |
| `JLINK_RAM_START` | `0x20000000` | RAM 起始地址（扫描 RTT 控制块用） |
| `JLINK_RAM_SIZE` | `0x8000` | RAM 扫描窗口大小（字节） |
| `RTT_CHANNEL` | `0` | Default RTT channel |
| `RTT_RING_BUFFER_SIZE` | `100` | Ring buffer entries |
| `RTT_POLL_INTERVAL_MS` | `10` | Poll interval in ms |
| `RTT_DAEMON_URL` | `http://127.0.0.1:8765/sse` | 守护进程 SSE 地址（`server.py`/`bridge.py` 探测与代理用） |
| `RTT_AUTH_TOKEN` | (none) | 可选：设此值后 `/sse`、`/messages/`、`/shutdown` 需 `Authorization: Bearer <token>`；不设则开放给本机任意进程 |

## Troubleshooting

### "J-Link not found" / 连接失败
- Make sure J-Link software is installed
- Check that J-Link is connected via USB
- Try running as administrator/root
- **J-Link 被锁住（卡在 RTT-started）**：上一次进程被 Windows `TerminateProcess` 硬杀，没释放。用 `POST http://127.0.0.1:8765/shutdown` 优雅释放，或拔插 J-Link USB。

### Claude Code 连上了但读不到数据（VSCode 扩展同时在用）
- 这是预期行为：扩展在用环形缓冲（`rtt_read` 会清空）。改用 `rtt_read_log` 或 `rtt_read_raw`，它们读广播日志，不被任何消费者清空。

### "Device not supported"
- Check if the device name is correct
- Try using `rtt_list_devices` to see available probes
- Verify the target is connected and powered

### "RTT not initialized"
- Make sure the target firmware calls `SEGGER_RTT_Init()`
- Check that RTT buffers are configured in `SEGGER_RTT_Conf.h`

## License

MIT
