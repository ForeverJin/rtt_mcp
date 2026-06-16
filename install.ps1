<#
.SYNOPSIS
  One-command bootstrap for the RTT-MCP server (Windows).
.DESCRIPTION
  Installs the Python package (console scripts on PATH), optionally the VSCode
  extension, and registers the `rtt` MCP server at Claude Code USER scope —
  workspace-independent and not tied to any MCU project.

  Run from the repo root (the folder containing pyproject.toml):
    powershell -ExecutionPolicy Bypass -File install.ps1
#>

$ErrorActionPreference = 'Stop'

function Info($m)  { Write-Host "[install] $m" -ForegroundColor Green }
function Warn($m)  { Write-Host "[install] WARN: $m" -ForegroundColor Yellow }
function Fail($m)  { Write-Host "[install] FAIL: $m" -ForegroundColor Red; exit 1 }

# Locate Python (python, then python3, then py launcher).
function Resolve-Python {
    foreach ($c in @('python', 'python3')) {
        $p = Get-Command $c -ErrorAction SilentlyContinue
        if ($p) { return $c }
    }
    $py = Get-Command py -ErrorAction SilentlyContinue
    if ($py) { return @('py', '-3') }
    return $null
}

$python = Resolve-Python
if (-not $python) { Fail "Python not found on PATH. Install Python 3.10+ from https://www.python.org/downloads/ and re-run." }

# --- Step 1: Python version >= 3.10 ---
$verRaw = & $python --version 2>&1
$verStr = ($verRaw -replace 'Python ', '').Trim()
try {
    $parts = $verStr.Split('.')
    $major = [int]$parts[0]; $minor = [int]$parts[1]
} catch { Fail "Could not parse Python version from '$verRaw'." }
if ($major -lt 3 -or ($major -eq 3 -and $minor -lt 10)) {
    Fail "Need Python >= 3.10, found $verStr."
}
Info "Python $verStr OK"

# --- Step 2: non-editable pip install (editable masks the portable log path) ---
Info "Installing package (non-editable)..."
& $python -m pip install .
if ($LASTEXITCODE -ne 0) { Fail "pip install failed (exit $LASTEXITCODE). Try: $python -m pip install --user ." }
Info "Package installed"

# --- Step 3: resolve console-script dir + verify the .exe exists ---
$scriptsDir = & $python -c "import sysconfig;print(sysconfig.get_path('scripts'))"
$exe = Join-Path $scriptsDir 'rtt-mcp-server.exe'
if (-not (Test-Path $exe)) {
    Warn "Console script not found at $exe."
    Warn "This usually means pip installed scripts to a dir not on PATH (--user)."
    Warn "Check the pip output above; you may need to add $scriptsDir to PATH."
    Fail "rtt-mcp-server.exe missing after install"
}
Info "Console script: $exe"

# --- Step 4: pylink health check (DLL loads lazily — never hard-gate on SEGGER) ---
try {
    & $python -c "import pylink; pylink.JLink()" 2>$null
    if ($LASTEXITCODE -eq 0) { Info "pylink-square import OK" } else { Warn "pylink import failed — install SEGGER J-Link software before connecting hardware" }
} catch { Warn "pylink health check skipped: $_" }

# --- Step 5: VSCode extension (best-effort) ---
$vsix = Join-Path $PSScriptRoot 'vscode-rtt-mcp\vscode-rtt-mcp-0.1.0.vsix'
$code = Get-Command code -ErrorAction SilentlyContinue
if ($code -and (Test-Path $vsix)) {
    Info "Installing VSCode extension..."
    code --uninstall-extension local.vscode-rtt-mcp 2>$null | Out-Null
    code --install-extension $vsix --force 2>$null | Out-Null
    Info "Extension installed (reload VSCode to activate)"
} elseif (-not $code) {
    Warn "`code` CLI not found — install the extension manually: drag $vsix into VSCode's Extensions panel"
} else {
    Warn "VSIX not found at $vsix — build it first with `vsce package` in vscode-rtt-mcp/"
}

# --- Step 6: register MCP at Claude Code user scope (absolute .exe — spawn doesn't search PATHEXT on Windows) ---
$claude = Get-Command claude -ErrorAction SilentlyContinue
if ($claude) {
    Info "Registering `rtt` at Claude Code user scope (absolute path, no cwd)..."
    claude mcp remove --scope user rtt 2>$null | Out-Null
    claude mcp add --scope user rtt $exe
    if ($LASTEXITCODE -ne 0) { Fail "claude mcp add failed" }
    Info "Registered. Verify with: claude mcp list"
} else {
    Warn "`claude` CLI not found. Add this to ~/.claude.json manually (mcpServers):"
    Warn ('  "rtt": { "type": "stdio", "command": "' + ($exe -replace '\\','\\') + '" }')
}

# --- Step 7: smoke test ---
Info "Smoke test: rtt-mcp-daemon --help"
& $exe -replace 'rtt-mcp-server.exe','rtt-mcp-daemon.exe' --help 2>&1 | Select-Object -First 2 | ForEach-Object { Write-Host "    $_" }

Info ""
Info "Done. RTT-MCP is now available in every Claude Code session (any workspace)."
Info "To target a specific MCU, override per project:"
Info "  claude mcp remove --scope user rtt; claude mcp add --scope user rtt `"$exe`" -e JLINK_DEVICE=HC32L19x"
