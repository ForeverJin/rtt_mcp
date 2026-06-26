# MCP Server for Segger RTT via J-Link

An MCP server that provides access to Segger RTT (Real Time Transfer) through a J-Link debugger. This allows an LLM (like Claude) to interact with embedded devices by reading and writing RTT data.

A single static Go binary (`rtt-mcp-server`) — pure Go, no cgo, no Python, no pip. The SEGGER J-Link library is loaded at runtime via `purego`; just install the [SEGGER J-Link software](https://www.segger.com/downloads/jlink/) and the tool finds it.

## Features

- **Shared single-owner daemon** — one long-lived SSE process owns the J-Link; Claude Code **and** the VSCode extension connect as clients, so they never fight over the single physical probe
- **Standalone fallback** — a Claude-only session with no daemon running opens the J-Link directly (auto-detected; no config change needed)
- **RTT Read/Write** — read up-buffer output, write commands to the down-buffer
- **Multi-consumer reads** — a draining ring buffer (`rtt_read`) **plus** a non-draining broadcast log (`rtt_read_log` / `rtt_read_raw`), so a continuous monitor never steals bytes from an on-demand read
- **Mock mode** — `RTT_MOCK=1` swaps the J-Link backend for a software heartbeat generator; develop / CI without hardware
- **Real-time display** — RTT data is continuously printed to terminal + a rotating log file while monitoring
- **Optional auth** — opt-in bearer token (`RTT_AUTH_TOKEN`) gates `/sse`, `/messages/` and `/shutdown`
- **Graceful shutdown** — `POST /shutdown` releases the J-Link and stops the daemon cleanly (works around Windows `TerminateProcess` stranding the probe)

## Prerequisites

- **Go 1.25+** ([go.dev/dl](https://go.dev/dl/)) — to build the binary from source
- SEGGER J-Link Software installed (provides `JLink_x64.dll` / `libjlinkarm.so` / `libjlinkarm.dylib`)
- A J-Link compatible debugger
- Target device running SEGGER RTT

## Installation（新机器一键装机）

本工具**装一次、全局可用**——不绑死任何单片机工程，不依赖具体 VSCode 工作区。

### 前置依赖

- **Go 1.25+**（在 PATH 上）—— 用来从 `./go` 源码构建 `rtt-mcp-server` 二进制
- **SEGGER J-Link 软件**（提供 `JLink_x64.dll`；Go 二进制在连接 J-Link 时才加载它，安装期不强制）——连硬件前装好
- 可选：**VS Code + `code` CLI**（用配套扩展时需要）

### 一键安装

```powershell
# Windows（在 mcp-rtt-server 目录下）
powershell -ExecutionPolicy Bypass -File install.ps1
```
```bash
# macOS / Linux
./install.sh
```

脚本完成：定位 Go → `go build` 编译 `bin/rtt-mcp-server` → 软检查 SEGGER 库 → 可选装 VSCode 扩展（在 **Claude Code 用户级**注册 `rtt`（绝对路径、无 cwd，任意工作区可用））→ 烟测。

### 手动安装

```bash
git clone https://github.com/ForeverJin/rtt_mcp
cd rtt_mcp
go build -C ./go -o ./bin/rtt-mcp-server .
./bin/rtt-mcp-server --help        # 烟测：应打印 usage 横幅
```

注册到 Claude Code（**用户级**——任意工作区可用；用绝对路径，Windows spawn 不搜 PATHEXT）：

```bash
# 取二进制绝对路径
BIN="$(pwd)/bin/rtt-mcp-server"          # macOS / Linux
# $BIN = (Resolve-Path .\bin\rtt-mcp-server.exe).Path   # PowerShell

claude mcp remove --scope user rtt 2>/dev/null
claude mcp add --scope user rtt "$BIN"
```

### 烟测（不需 J-Link）

```bash
./bin/rtt-mcp-server help     # 打印 usage 横幅
```

## Usage

### 三种启动模式

`rtt-mcp-server` 是一个二进制，三个子命令：

| 子命令 | 角色 | 谁用 |
|---|---|---|
| `serve` (默认) | stdio MCP 入口；daemon 可达时经 SSE 代理，否则直连 J-Link | Claude Code |
| `daemon` | SSE 守护进程；**唯一**持有 J-Link | 独立后台 / VSCode 扩展拉起 |
| `bridge` | stdio↔SSE 透传（不开第二个 J-Link） | VSCode 扩展 |

### 启动守护进程（可选，推荐）

守护进程是"单一 J-Link 拥有者"。VSCode 扩展会自动启动它；手动调试/独立使用时也可以自己起：

```bash
./bin/rtt-mcp-server daemon --host 127.0.0.1 --port 8765
```

> 不启动也没关系：`serve` 会探测守护进程（`http://127.0.0.1:8765/sse`），可达则经它代理，不可达则直接开 J-Link（独立会话模式）。

### 硬件无关的 mock 模式

```bash
RTT_MOCK=1 ./bin/rtt-mcp-server serve
# 或起守护进程供多个客户端共享：
RTT_MOCK=1 ./bin/rtt-mcp-server daemon
```

`RTT_MOCK=1` 把 J-Link 后端换成软件版——`rtt_read` 每次返回一行 `[mock] heartbeat #N ...`。适合本地开发 / CI / 文档示例，无需任何硬件。

### Step 3: 启动 MCP Inspector（调试/测试用）

**终端 A — 启动 MCP 服务器：**

```bash
npx @modelcontextprotocol/inspector ./bin/rtt-mcp-server
# 或 mock：
RTT_MOCK=1 npx @modelcontextprotocol/inspector ./bin/rtt-mcp-server
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
7. **`rtt_list_supported_devices` / `rtt_check_device`** — 查 J-Link 设备库（无需连接 probe）

### Claude Code 集成

一键脚本已自动注册（用户级，任意工作区可用）。手动注册：

```bash
BIN="$(pwd)/bin/rtt-mcp-server"          # macOS / Linux
# 或 Windows:  (Resolve-Path .\bin\rtt-mcp-server.exe).Path
claude mcp add --scope user rtt "$BIN"
```

> **用户级（user scope）= 与工程无关**：注册一次后，任意 Claude Code 会话（无论打开哪个工作区）都能用 `rtt` 工具。**不要**在项目 `.claude/settings.json` 里重复写 `mcpServers.rtt`——那会用绝对路径覆盖用户级配置。

> **指定芯片**：默认设备 `Cortex-M0+`（泛用）。针对某工程固定芯片，按工程覆盖环境变量：
> ```bash
> claude mcp remove --scope user rtt
> claude mcp add --scope user rtt "$BIN" -e JLINK_DEVICE=HC32L19x
> ```
> 或调用 `jlink_connect` 时直接传 `device` 参数（优先级最高）。

> **单拥有者行为**：`serve` 启动时探测守护进程（`http://127.0.0.1:8765/sse`）——可达则经 SSE 代理所有工具调用（**不开第二个 J-Link**），与 VSCode 扩展共享同一连接，零争用；守护进程未运行则自动回退为直接开 J-Link（Claude-only 会话）。无需手动选择模式。

重启 Claude Code 会话后，可直接用自然语言与设备交互，例如：
- "连接 RTT 读取设备数据"
- "查看设备状态"

### Claude Desktop 集成

编辑 `%APPDATA%\Claude\claude_desktop_config.json`：

```json
{
  "mcpServers": {
    "rtt": {
      "command": "<abs path to>/bin/rtt-mcp-server",
      "args": []
    }
  }
}
```

> **注意**：`command` 必须是 Go 二进制的绝对路径（Claude Desktop 不搜 PATH 也不搜 PATHEXT）。`args` 留空走默认的 `serve` 子命令。

## Available Tools

| Tool | Description |
|------|-------------|
| `jlink_connect` | Connect to J-Link and start RTT monitoring |
| `jlink_disconnect` | Disconnect from J-Link |
| `jlink_status` | Get connection status and buffer info |
| `rtt_list_devices` | List available J-Link probes (emulator serial numbers) |
| `rtt_list_supported_devices` | List names in the loaded J-Link device database (probe-less; works before connect) |
| `rtt_check_device` | Check whether a device name exists in the J-Link device database (probe-less) |
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

单一 J-Link 拥有者模型：守护进程是唯一 `pylink.open()` 替代品——Go 用 `purego` 直接调 SEGGER 库的 `JLINKARM_*` 符号，效果一致。Claude Code 与 VSCode 扩展都作为客户端连到 daemon，永不开第二个 J-Link，因此不会争抢探针、也不会互相偷走 RTT 数据。

```
  ┌──────────────┐        ┌────────────────────────┐
  │  Claude Code │ stdio  │  rtt-mcp-server serve  │
  │              │────────│  探测 daemon:           │
  └──────────────┘        │   可达→SSE 代理        │
                          │   不可达→直接开 J-Link  │
                          └─────────┬──────────────┘
                                    │ (可达时)
  ┌──────────────┐        ┌─────────▼──────────────┐      ┌────────────────┐
  │ VSCode 扩展  │ stdio  │  rtt-mcp-server bridge │ SSE  │ rtt-mcp-server │
  │ (RttProvider)│────────│  (stdio↔SSE 桥)        │─────│     daemon     │
  └──────────────┘        └────────────────────────┘      │  单一拥有者    │
                                                           └───────┬────────┘
                                                                   │ purego FFI
                                                                   ▼
                                                           ┌────────────────┐
                                                           │  JLink_x64.dll │
                                                           └───────┬────────┘
                                                                   │ USB
                                                                   ▼
                                                           ┌────────────────┐
                                                           │  J-Link Probe  │── SWD ──▶ Target
                                                           └────────────────┘
```

**模块职责** (Go 内部)

| 模块 | 角色 |
|---|---|
| `rtt-mcp-server daemon` | SSE 守护进程，**唯一**持有 J-Link；`POST /shutdown` 优雅释放 |
| `rtt-mcp-server bridge` | stdio↔SSE 桥（VSCode 扩展用）；`daemonReachable()` 探测 + 透传 |
| `rtt-mcp-server serve` | Claude Code 入口；daemon 可达则代理，不可达则回退为直接拥有者 |
| `internal/rttcore` | 单例引擎：监视 goroutine、ring buffer、广播日志、轮转 |
| `internal/jlink` | purego FFI：动态加载 JLinkARM/JLink_x64、符号绑定、mock backend |
| `internal/tools` | 11 个 MCP 工具（名称/参数/结果与原 Python 1:1 对齐，外加 `rtt_list_supported_devices` + `rtt_check_device`） |
| `internal/transport` | `serve`（stdio）、`daemon`（HTTP/SSE）、`bridge`（stdio↔SSE）的传输层 |
| `internal/config` | 环境变量统一读取（`JLINK_*` / `RTT_*`，与原 Python 同名同默认） |

### 关闭与 J-Link 释放

`POST /shutdown`（可选 `Authorization: Bearer <token>`）→ `rttcore.Disconnect()`（`RTTStop` + `Close`）→ Go 的 `http.Server.Shutdown` 干净退出。VSCode 扩展停掉守护进程时先调它，再退而求其次 `kill()` —— 这避免了 Windows `TerminateProcess` 硬杀导致 J-Link 卡在 RTT-started 状态。`signal.NotifyContext` 钩子作为 Ctrl-C / 正常退出的兜底。

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `JLINK_SERIAL` | (none) | J-Link serial number |
| `JLINK_DEVICE` | `Cortex-M0+` | Target device name（泛用默认；按工程覆盖，如 `HC32L19x`/`STM32F103`） |
| `JLINK_SPEED` | `4000` | SWD speed in kHz |
| `JLINK_RAM_START` | `0x20000000` | RAM 起始地址（扫描 RTT 控制块用，按 MCU 调整） |
| `JLINK_RAM_SIZE` | `0x8000` | RAM 扫描窗口大小（字节，按 MCU 调整） |
| `JLINK_LIB_PATH` / `RTT_LIB_PATH` | 自动探测 | 强制指定 SEGGER 库路径（`JLink_x64.dll` / `libjlinkarm.so` / `libjlinkarm.dylib`），覆盖默认 glob |
| `RTT_CHANNEL` | `0` | Default RTT channel |
| `RTT_RING_BUFFER_SIZE` | `100` | Ring buffer entries |
| `RTT_POLL_INTERVAL_MS` | `10` | Poll interval in ms |
| `RTT_LOG_FILE` | 可移植默认 | RTT 输出日志路径（留空=`%LOCALAPPDATA%\mcp-rtt-server\rtt_output.log` / `~/.local/state/...`；设值则覆盖） |
| `RTT_DAEMON_URL` | `http://127.0.0.1:8765/sse` | 守护进程 SSE 地址（`serve`/`bridge` 探测与代理用） |
| `RTT_AUTH_TOKEN` | (none) | 可选：设此值后 `/sse`、`/messages/`、`/shutdown` 需 `Authorization: Bearer <token>`；不设则开放给本机任意进程 |
| `RTT_MOCK` | (none) | 设为 `1` 启用 mock 后端（无 J-Link/无硬件也能跑；每读返回一行 heartbeat） |

## Troubleshooting

### "J-Link not found" / 连接失败
- Make sure J-Link software is installed
- Check that J-Link is connected via USB
- Try running as administrator/root
- **J-Link 被锁住（卡在 RTT-started）**：上一次进程被 Windows `TerminateProcess` 硬杀，没释放。用 `POST http://127.0.0.1:8765/shutdown` 优雅释放，或拔插 J-Link USB。
- **找不到 SEGGER 库**：Go 默认 glob `C:\Program Files\SEGGER\JLink_V*/JLink_x64.dll`（Windows）/ `/opt/SEGGER/JLink/libjlinkarm.so`（Linux）/ `/Applications/SEGGER/JLink/libjlinkarm.dylib`（macOS）。装在别处用 `JLINK_LIB_PATH` 指定完整路径。

### Claude Code 连上了但读不到数据（VSCode 扩展同时在用）
- 这是预期行为：扩展在用环形缓冲（`rtt_read` 会清空）。改用 `rtt_read_log` 或 `rtt_read_raw`，它们读广播日志，不被任何消费者清空。

### "Device not supported"
- Check if the device name is correct
- Use `rtt_list_supported_devices` to enumerate the loaded J-Link database (~14k devices); `rtt_check_device(name)` to validate a name before connecting
- Verify the target is connected and powered

### "RTT not initialized"
- Make sure the target firmware calls `SEGGER_RTT_Init()`
- Check that RTT buffers are configured in `SEGGER_RTT_Conf.h`
- Confirm `JLINK_RAM_START` / `JLINK_RAM_SIZE` cover the target's SRAM (RTT control block is in RAM)

## License

MIT
