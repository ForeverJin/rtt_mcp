# vscode-rtt-mcp

一个 VSCode 扩展，把 [`rtt-mcp-server`](../go)（Go 版）封装起来，让你**在编辑器里直接用 J-Link RTT**——不用敲命令行、不用手搓 MCP 工具。自带一个独立的 **RTT 监视面板**（在底部面板里单独占一栏，和串口监视器一个体验），实时流式显示设备输出、按日志等级自动着色，还能直接在里面打字给单片机发命令。

## 特性

- **独立 RTT 面板**：底部面板里一个名为 `RTT` 的标签页（webview），像串口监视器一样——滚动输出区 + 输入框，不挤在集成终端里。
- **日志等级着色**：`ERROR/失败`→红、`WARN/警告`→黄、`INFO`→绿、`DEBUG/调试`→暗灰、`[RTT TX]` 发送记录→青；无标签的普通行默认色。按行识别，支持 `ERROR/WARN/INFO/DEBUG` 关键字、`[E]/[W]/[I]/[D]`、`<err>/<warn>`、ESP-IDF `I (1234) tag:` 等常见格式。
- **交互式输入**：在面板输入框里打字、回车即发给单片机（自动补 `\r\n`）；退格本地编辑、`Ctrl+C` 发中断。`rtt-mcp.localEcho` 控制本地回显。
- **实时设备选择器**：从 J-Link 设备库（本机一万四千多个设备）**实时拉取**，输入即搜索（如 `STM32F407` → `VE/VG/ZE…`），也可选通用 ARM 核或手输精确型号。
- **共享单主守护进程**：扩展起一个长驻 SSE daemon 作为**唯一**持有 J-Link 的进程；Claude Code 自动连同一个 daemon，两个客户端共享一个探头、零冲突。
- **非抽取式监视**：监视器轮询广播日志（`rtt_read_raw`），绝不偷走按需读取或 Claude Code 的字节。
- **优雅退出**：断开/卸载时先 `POST /shutdown` 让 daemon 释放 J-Link，再退而求其次 `kill()`，避免 Windows `TerminateProcess` 把探头卡死。
- **状态栏指示**：右下角 `RTT` 图标，反映连接 + 监视状态；点击出快捷菜单。
- **13 条命令**：连接 / 断开 / 读取 / 写入 / 列设备 / 状态 / 清缓冲 / 开关监视 / 打开面板 / 打开日志 / 重置 / 设置目标设备 / 显示快捷菜单。

## 环境要求

- VSCode 1.85+
- **SEGGER J-Link 已安装**（提供 `JLink_x64.dll`；扩展会自动在 `C:\Program Files\SEGGER\JLink_V***\` 下找）。无需 Python、无需 pip。
- J-Link 探头 + 目标板固件里跑着 SEGGER RTT（调了 `SEGGER_RTT_Init()`）。

## 安装

### 方式 A：装 VSIX（推荐）

```bash
code --install-extension vscode-rtt-mcp-0.2.0.vsix
```

VSIX 里已自带编译好的 Go 二进制（`bin/rtt-mcp-server.exe`），开箱即用。

### 方式 B：从源码构建

见文末「从源码构建」。

## 快速上手

1. **重载 VSCode**（装完 VSIX 后）。
2. 点状态栏右下角 `RTT` 图标 → **Connect to J-Link**（或 `Ctrl+Shift+P` → `RTT: Connect to J-Link`）。
3. 连上后，底部面板会出现 **`RTT`** 标签——设备的 RTT 输出实时刷在里面，并按日志等级着色。
4. 想发数据：在面板底部的输入框里打字、回车。
5. 想清屏：面板标题栏的垃圾桶图标（或 `RTT: Clear Monitor`）。

### 状态栏图标

| 图标 | 含义 |
|------|------|
| `$(debug-disconnect) RTT` | 已断开 |
| `$(debug-start) RTT` | 已连接，未监视 |
| `$(eye) RTT ●` | 已连接 + 监视中 |

## 设备选择

J-Link 只认它设备库里**精确**的名字（比如 `STM32F407VE`，而不是裸的 `STM32F407`）。`RTT: Set Target Device` 打开选择器：

- **通用 ARM 核**（`Cortex-M0+/M0/M3/M4/M7/M33`）——始终有效，跑 RTT 足够；
- **从 J-Link 库实时搜索**——输入前缀过滤本机全部设备（一万四千多个）；
- **自定义**——手输精确型号，会实时校验是否在库里。

> 小贴士：HC32L19x 这类芯片本身就是 Cortex-M0+ 核，用 `Cortex-M0+` 当设备名开箱即连。

## 日志标签规则（给固件侧）

RTT 面板按**整行**识别日志级别并着色（大小写不敏感，关键字按词边界匹配）。优先级 `TX > ERROR > WARN > DEBUG > INFO`（一行命中多个取高的）。

| 级别 / 颜色 | 关键字（独立词） | 标签 | 示例 |
|---|---|---|---|
| **ERROR** 红 | `error` `err` `failed` `failure` `fatal` `panic` `assert` `fault` `exception` | `[E]` `[F]` `[C]` `<E>` `<F>` `<C>` `<err>` `<fatal>` | `[E] flash erase failed` |
| **WARN** 黄 | `warn` `warning` | `[W]` `<W>` `<warn>` | `WARN: thermal high` |
| **INFO** 绿 | `info` `information` | `[I]` `<I>` `<inf>` | `INFO boot complete` |
| **DEBUG** 暗 | `debug` `dbg` `trace` `verbose` | `[D]` `<D>` `<dbg>` | `[D] adc=1234mV` |
| **TX** 青 | —— | —— | 面板打字 / Write 命令（扩展发的 `[RTT TX]`） |

不含任何关键字的行（如 `tick 387 s`）→ 默认前景色；扩展自身状态行（`[RTT] Monitor started` 等）不含级别关键字时为暗灰。INFO 还兼容 ESP-IDF 行首格式 `I (1234) tag: ...`。

**推荐**：全工程统一用方括号前缀，最干净：

```c
SEGGER_RTT_printf(0, "[E] flash erase failed @0x%08X\n", addr);  // 红
SEGGER_RTT_printf(0, "[W] battery %d%%\n", pct);                 // 黄
SEGGER_RTT_printf(0, "[I] boot complete, v%d.%d\n", maj, min);   // 绿
SEGGER_RTT_printf(0, "[D] adc=%dmV\n", mv);                      // 暗
```

关键字须是独立单词（`errorcode` 连写不触发，`error code` 或 `[E]` 才会）；一行一条日志；`\n` / `\r\n` 行尾都行。

## 配置

设置里搜 `RTT MCP`，或写进 `.vscode/settings.json`：

```json
{
  "rtt-mcp.binaryPath": "",
  "rtt-mcp.serverArgs": ["bridge"],
  "rtt-mcp.daemonUrl": "http://127.0.0.1:8765/sse",
  "rtt-mcp.device": "Cortex-M0+",
  "rtt-mcp.speed": 4000,
  "rtt-mcp.pollIntervalMs": 300,
  "rtt-mcp.autoConnect": false,
  "rtt-mcp.localEcho": true
}
```

| 配置项 | 默认值 | 说明 |
|-----|---------|-------------|
| `rtt-mcp.binaryPath` | (空) | `rtt-mcp-server` Go 二进制路径；留空=用扩展自带的，也可填绝对路径或 PATH 上的名字指向你自己的构建 |
| `rtt-mcp.serverArgs` | `["bridge"]` | stdio 客户端的启动参数；`bridge` 经共享 daemon 代理 |
| `rtt-mcp.daemonUrl` | `http://127.0.0.1:8765/sse` | 共享 daemon 的 SSE 地址；没起会自动拉起 |
| `rtt-mcp.device` | `Cortex-M0+` | J-Link 设备名（自由文本）：通用 ARM 核，或 J-Link 库里的精确型号 |
| `rtt-mcp.speed` | `4000` | SWD 速度（kHz） |
| `rtt-mcp.channel` | `0` | 默认 RTT 通道 |
| `rtt-mcp.ramStart` | `0x20000000` | RTT 控制块扫描的 RAM 起始地址（十六进制）；默认适配 HC32L19x |
| `rtt-mcp.ramSize` | `0x20000` | RTT 控制块扫描的 RAM 窗口大小（十六进制）；默认适配 HC32L19x（128KB） |
| `rtt-mcp.pollIntervalMs` | `300` | 监视器轮询间隔（ms） |
| `rtt-mcp.autoConnect` | `false` | 扩展激活时是否自动连接 |
| `rtt-mcp.localEcho` | `true` | 在 RTT 面板里打字是否本地回显；固件自己会回显就关掉，避免重复 |

## 架构

```
VSCode 扩展（TypeScript）
   │
   ├─ 状态栏 / 命令 / RTT 监视面板（webview）
   │
   ├─ ensureDaemon：确保共享 SSE daemon 在跑（127.0.0.1:8765/sse 没响应就
   │  拉起 `rtt-mcp-server daemon`）——它是唯一持有 J-Link 的进程
   │
   └─ RttProvider
        ├─ spawn 桥接进程 `rtt-mcp-server bridge`（stdio↔SSE）
        │    └─ 把每个 MCP 调用经 SSE 转发给 daemon
        ├─ 经 stdio 讲 MCP/JSON-RPC（换行分隔 JSON）
        └─ 监视模式轮询 rtt_read_raw（广播日志，非抽取）
```

扩展和 Claude Code 都作为客户端连**同一个** daemon，所以 J-Link 永远不会被打开两次。`deactivate()` / 退出时先 `POST /shutdown`（释放 J-Link）再 `kill()`。MCP 客户端在 `src/mcpClient.ts` 里从零实现（不依赖 `@modelcontextprotocol/sdk`）。

> **省内存**：默认 `bridge` 模式跑两个 Go 进程（daemon + bridge）。如果你**不**同时用 Claude Code 连这块板子，可把 `serverArgs` 改成 `["serve"]`（单进程、直接持有 J-Link，约省 ~15MB）——但此时 Claude Code 无法共享同一连接。

## 故障排查

- **连不上 / "unknown device"**：`device` 名字不在 J-Link 设备库里。用 `RTT: Set Target Device` 从库里搜精确型号，或先用通用核 `Cortex-M4` 之类。
- **找不到 DLL / `JLink_x64.dll`**：扩展自动在版本号目录（`JLink_V950` 等）下找；若装在别处，用 `JLINK_LIB_PATH` 环境变量指定完整路径。
- **"RTT CB not found"**：固件没调 `SEGGER_RTT_Init()`，或 `SEGGER_RTT_Conf.h` 里缓冲太小，或 `ramStart/ramSize` 没覆盖到目标 SRAM。
- **J-Link 卡住 / "already connected"**：上个进程被硬杀没释放探头。重启 VSCode（卸载时会 `POST /shutdown`），或拔插 J-Link。
- **面板里打字没反应**：固件没在读 RTT 下行缓冲（没调 `SEGGER_RTT_Read()`）。写入本身成功，但单片机不消费就看不到效果。

## 从源码构建

需要 Go 工具链（构建后端二进制）和 Node.js（构建扩展）。

```bash
# 1) 构建 Go 二进制（pure Go，无 cgo；运行时加载 JLink_x64.dll）
cd ../go
#   本机 Go 不在 PATH / proxy.golang.org 不通时：
export PATH="/c/Users/<你>/go-sdk/go/bin:$PATH"
export GOPROXY="https://goproxy.cn,direct"
export GOSUMDB=off
go build -o rtt-mcp-server .

# 2) 拷进扩展的 bin/（VSIX 会打包进去）
mkdir -p ../vscode-rtt-mcp/bin
cp rtt-mcp-server.exe ../vscode-rtt-mcp/bin/

# 3) 构建扩展并打包成 VSIX
cd ../vscode-rtt-mcp
npm install
npm run build
npx @vscode/vsce package
code --install-extension vscode-rtt-mcp-0.2.0.vsix
```

> `bin/` 是构建产物（已 gitignore）。`npm run build` 编译 TS；`vsce package` 把 `bin/` + `dist/` 一起打进 VSIX。

## 文件结构

- `src/extension.ts` — 命令、状态栏、RTT 监视面板（webview provider）
- `src/mcpClient.ts` — stdio 上的 MCP/JSON-RPC 客户端（无 SDK 依赖）
- `src/rttProvider.ts` — RTT 操作封装 + 监视循环
- `bin/rtt-mcp-server[.exe]` — 打包进 VSIX 的 Go 后端二进制
- `package.json` — 清单、命令与配置项贡献
