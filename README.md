# MCP Server for Segger RTT via J-Link

An MCP server that provides access to Segger RTT (Real Time Transfer) through a J-Link debugger. This allows an LLM (like Claude) to interact with embedded devices by reading and writing RTT data.

## Features

- **RTT Read**: Read output from embedded devices through RTT up-buffer
- **RTT Write**: Send commands to embedded devices through RTT down-buffer
- **J-Link Management**: Connect/disconnect J-Link debuggers, list available devices
- **Real-time Display**: RTT data is continuously printed to terminal while monitoring
- **Ring Buffer**: Recent RTT data is accumulated for reading by the LLM

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

### Step 2: 启动 MCP Inspector（调试/测试用）

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

### Step 3: 使用工具

在浏览器 Inspector 中按顺序调用：

1. **`jlink_connect`** — 连接 J-Link 和目标设备，启动 RTT 监控
2. **`rtt_read`** — 读取设备发送的 RTT 数据
3. **`rtt_write`** — 向设备发送数据（需固件支持接收）
4. **`jlink_status`** — 查看连接状态和缓冲区信息
5. **`jlink_disconnect`** — 断开连接

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
| `rtt_read` | Read accumulated RTT data |
| `rtt_write` | Write data to RTT down-buffer |
| `rtt_clear` | Clear the RTT ring buffer |

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

```
┌─────────────────┐     stdio (JSON-RPC)     ┌─────────────────┐
│   Claude /      │ ◄──────────────────────►  │  MCP RTT Server │
│   MCP Client    │                          │    (Python)     │
└─────────────────┘                          └────────┬────────┘
                                                      │
                                             pylink   │
                                                      ▼
                                            ┌─────────────────┐
                                            │  JLinkARM.dll   │
                                            └────────┬────────┘
                                                      │ USB
                                                      ▼
                                            ┌─────────────────┐
                                            │   J-Link Probe  │
                                            └────────┬────────┘
                                                      │ SWD/JTAG
                                                      ▼
                                            ┌─────────────────┐
                                            │  Target Device  │
                                            │  (HC32L19x)     │
                                            │  SEGGER RTT     │
                                            └─────────────────┘
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `JLINK_SERIAL` | (none) | J-Link serial number |
| `JLINK_DEVICE` | `HC32L19x` | Target device name |
| `JLINK_SPEED` | `4000` | SWD speed in kHz |
| `RTT_CHANNEL` | `0` | Default RTT channel |
| `RTT_RING_BUFFER_SIZE` | `100` | Ring buffer entries |
| `RTT_POLL_INTERVAL_MS` | `10` | Poll interval in ms |

## Troubleshooting

### "J-Link not found"
- Make sure J-Link software is installed
- Check that J-Link is connected via USB
- Try running as administrator/root

### "Device not supported"
- Check if the device name is correct
- Try using `rtt_list_devices` to see available probes
- Verify the target is connected and powered

### "RTT not initialized"
- Make sure the target firmware calls `SEGGER_RTT_Init()`
- Check that RTT buffers are configured in `SEGGER_RTT_Conf.h`

## License

MIT
