<#
.SYNOPSIS
  One-command bootstrap for the RTT-MCP server (Windows).
.DESCRIPTION
  Builds the Go binary from source, optionally the VSCode extension, and
  registers the `rtt` MCP server at Claude Code USER scope — workspace-
  independent and not tied to any MCU project.

  Run from the repo root (the folder containing go/):
    powershell -ExecutionPolicy Bypass -File install.ps1
#>

$ErrorActionPreference = 'Stop'

function Info($m)  { Write-Host "[install] $m" -ForegroundColor Green }
function Warn($m)  { Write-Host "[install] WARN: $m" -ForegroundColor Yellow }
function Fail($m)  { Write-Host "[install] FAIL: $m" -ForegroundColor Red; exit 1 }

# --- Step 1: locate go (PATH first, then dev-box fallback) ---
$goCmd = (Get-Command go -ErrorAction SilentlyContinue)?.Path
if (-not $goCmd) {
  $fallback = 'C:\Users\jy\go-sdk\go\bin\go.exe'
  if (Test-Path $fallback) { $goCmd = $fallback }
}
if (-not $goCmd) { Fail "Go not found on PATH or at $fallback. Install Go >= 1.25 from https://go.dev/dl/" }
Info "Go: $goCmd"

# --- Step 2: build the main binary (Claude Code consumes this) ---
$repoRoot = (Resolve-Path "$PSScriptRoot").Path
$binDir   = Join-Path $repoRoot 'bin'
$exe      = Join-Path $binDir 'rtt-mcp-server.exe'
New-Item -ItemType Directory -Path $binDir -Force | Out-Null
$env:GOPROXY = if ($env:GOPROXY) { $env:GOPROXY } else { 'https://goproxy.cn,direct' }
# -C changes cwd to ./go, so -o must be absolute to land the binary in
# the repo-level bin/ directory.
& $goCmd build -C (Join-Path $repoRoot 'go') -o $exe .
if ($LASTEXITCODE -ne 0) { Fail "go build failed (exit $LASTEXITCODE)" }
if (-not (Test-Path $exe) -or (Get-Item $exe).Length -eq 0) { Fail "binary missing or empty at $exe" }
Info "Built: $exe"

# --- Step 3: SEGGER J-Link soft check (lazy-loaded; not fatal) ---
$segger = $null
Get-ChildItem 'C:\Program Files\SEGGER', 'C:\Program Files (x86)\SEGGER' -Filter 'JLink_x64.dll' -Recurse -ErrorAction SilentlyContinue | ForEach-Object { $segger = $_.FullName; return }
if (-not $segger) { Warn "SEGGER J-Link library not found; install before connecting hardware" }

# --- Step 4: VSCode extension (build + install via the extension's own build.sh) ---
$buildSh = Join-Path $repoRoot 'vscode-rtt-mcp\build.sh'
$code    = Get-Command code -ErrorAction SilentlyContinue
$bash    = Get-Command bash -ErrorAction SilentlyContinue
if ($code -and $bash -and (Test-Path $buildSh)) {
  Info "Building VSCode extension (Go binary + vsix)..."
  $vsixOutput = & $bash $buildSh 2>&1
  $vsixFile   = ($vsixOutput | Select-String -Pattern 'vscode-rtt-mcp-[0-9.]+\.vsix' -AllMatches).Matches | ForEach-Object { $_.Value } | Select-Object -Last 1
  if ($vsixFile -and (Test-Path (Join-Path $repoRoot "vscode-rtt-mcp\$vsixFile"))) {
    $vsixPath = Join-Path $repoRoot "vscode-rtt-mcp\$vsixFile"
    Info "Installing $vsixFile..."
    & code --uninstall-extension local.vscode-rtt-mcp 2>$null | Out-Null
    & code --install-extension $vsixPath --force 2>$null | Out-Null
    Info "Extension installed (reload VSCode to activate)"
  } else {
    Warn "Extension build did not produce a .vsix — run $buildSh manually to see the error"
  }
} elseif (-not $code) {
  Warn "`code` CLI not found — install the extension manually: drag the .vsix from vscode-rtt-mcp/ into VSCode's Extensions panel"
} elseif (-not $bash) {
  Warn "`bash` CLI not found (Git Bash / WSL required to run build.sh) — run $buildSh manually"
} else {
  Warn "Build script missing at $buildSh — extension will not be installed"
}

# --- Step 5: register MCP at Claude Code user scope (absolute .exe — spawn doesn't search PATHEXT) ---
$claude = Get-Command claude -ErrorAction SilentlyContinue
if ($claude) {
  Info "Registering `rtt` at Claude Code user scope (absolute path, no cwd)..."
  & claude mcp remove --scope user rtt 2>$null | Out-Null
  & claude mcp add --scope user rtt $exe
  if ($LASTEXITCODE -ne 0) { Fail "claude mcp add failed" }
  Info "Registered. Verify with: claude mcp list"
} else {
  Warn "`claude` CLI not found. Add this to ~/.claude.json manually (mcpServers):"
  Warn ('  "rtt": { "type": "stdio", "command": "' + ($exe -replace '\\','\\') + '" }')
}

# --- Step 6: smoke test ---
Info "Smoke test: $exe help"
& $exe help 2>&1 | Select-Object -First 2 | ForEach-Object { Write-Host "    $_" }

Info ""
Info "Done. RTT-MCP is now available in every Claude Code session (any workspace)."
Info "To target a specific MCU, override per project:"
Info ('  claude mcp remove --scope user rtt; claude mcp add --scope user rtt "' + $exe + '" -e JLINK_DEVICE=HC32L19x')
