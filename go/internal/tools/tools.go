// Package tools registers the RTT MCP tools on a server and dispatches
// them to the rttcore singleton. Tool names, argument schemas and result text
// are kept byte-for-byte compatible with the Python server so the VSCode
// extension and Claude Code see an identical surface (rtt_wait, jlink_read_mem
// and jlink_reset are Go-only additions).
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"rtt-mcp-server/internal/rttcore"
)

// Register installs every tool on the given server.
func Register(s *mcp.Server) {
	core := rttcore.Get()
	_ = core // referenced via rttcore.Get() in handlers to reflect singleton state

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "jlink_connect",
			Description: "Connect to J-Link debugger and start RTT monitoring. Must be called before reading or writing RTT data. Pass log_file to redirect the broadcast RTT log to a per-project file (empty keeps the current/default path).",
		}, handleConnect)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "jlink_disconnect",
			Description: "Disconnect from J-Link debugger and stop RTT monitoring.",
		}, handleDisconnect)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_read",
			Description: "Read accumulated RTT data from the ring buffer. Data is continuously collected by the background monitor thread. Use this to get output from the target device. Returns up to max_bytes (default 8192) of the most recent buffered data; the read drains the buffer.",
		}, handleRead)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_read_log",
			Description: "Read the tail of the complete RTT log file. This is an independent broadcast sink (not drained by other clients), so it returns the full RTT output even while another client (e.g. the VSCode extension) is streaming via rtt_read. Prefer this over rtt_read when another consumer is active.",
		}, handleReadLog)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_read_raw",
			Description: `Read new bytes from the broadcast log starting at a byte offset. Non-draining and multi-consumer safe: ideal for a continuous monitor that must coexist with other readers without stealing their data. Pass the returned next_offset as 'offset' on the next call; if the log rotated (next_offset > file size), pass offset=0. Returns JSON: {"data": "...", "next_offset": N, "connected": bool}. When "connected" is false the J-Link is NOT connected — stop polling and call jlink_connect first.`,
		}, handleReadRaw)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_wait",
			Description: "Block until new RTT output appears (or a regex matches it) or the timeout elapses, then return what arrived. Reads the non-draining broadcast log, so it never steals bytes from other consumers (rtt_read, the VSCode monitor). Use this instead of repeated rtt_read_raw polling when you expect a reply soon — e.g. rtt_write a command then rtt_wait for its response. Args: pattern (optional Go/RE2 regex; omit to wake on ANY new output), timeout_ms (default 5000, max 60000), max_bytes (default 8192), offset (optional log start; omit for auto). By default the wait starts at the LAST rtt_write's position — a device replies in milliseconds while this call starts seconds later, so starting at the live log end would miss the reply; pass offset to pin the start (rtt_read_raw convention). Binary frames without a trailing newline (e.g. a Modbus reply) appear as their own log line after a short stream silence. Returns JSON: {\"matched\": bool, \"timed_out\": bool, \"data\": \"...\", \"connected\": bool}. matched=false with timed_out=true means nothing (or no match) arrived in time.",
		}, handleWait)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_write",
			Description: "Write data to RTT down-buffer (host -> device). C-style escapes are interpreted so control bytes can be sent: \\r \\n \\t \\0 \\\\ and \\xNN (two hex digits). If the data does not end with \\r or \\n, \\r\\n is appended automatically (matches the VSCode panel's webview input path) so a bare command like 'AT' reaches the device's line parser — pass a string already terminated to suppress. The target device must be running RTT with a down-buffer listener. Each write stamps a '>>> TX ch<N> <hex bytes>' marker into the broadcast log, and the following rtt_wait starts at this write by default, so request/response round-trips are captured end-to-end.",
		}, handleWrite)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_list_devices",
			Description: "List available J-Link debuggers connected to the host.",
		}, handleListDevices)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_list_supported_devices",
			Description: "List device names in the loaded J-Link device database (probe-less; works before connecting). Pass `query` for a case-insensitive substring filter. Use an exact returned name (e.g. STM32F407VE) as the `device` argument to jlink_connect.",
		}, handleListSupportedDevices)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_check_device",
			Description: "Check whether a device name exists in the loaded J-Link device database (probe-less). Returns supported (yes/no) and the J-Link index.",
		}, handleCheckDevice)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "jlink_read_mem",
			Description: "Read 32-bit words from target memory or memory-mapped peripheral registers over the debug interface. Non-intrusive: the core keeps running and an active RTT session is unaffected (ARM peripheral registers live at 0x40000000+). addr is hex with or without the 0x prefix (e.g. \"0x20000000\" or \"40021000\"); count is the number of 32-bit words (default 16, max 4096 — use larger counts to dump big regions in fewer round-trips). Returns a hex dump, 4 words per line. Core registers (R0-PC/SP) are deliberately not exposed — reading them would halt the core.",
		}, handleReadMem)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "jlink_reset",
			Description: "Reset the target MCU and let it resume running (reset-no-halt). Non-halting: the RTT session stays attached across the reset — the control-block address is fixed by firmware layout and is re-initialized as the target boots, so boot output flows into the RTT log. A '=== target reset ===' marker is stamped into the broadcast log to mark the boundary. To capture boot output, follow with rtt_wait.",
		}, handleReset)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "jlink_status",
			Description: "Get current J-Link connection status and RTT buffer information.",
		}, handleStatus)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_clear",
			Description: "Clear the RTT ring buffer. This does not clear the actual RTT buffers on the device.",
		}, handleClear)
}

// ---- input structs (pointer fields => optional in the derived schema) ----

type connectIn struct {
	Serial  *string `json:"serial,omitempty"`
	Device  *string `json:"device,omitempty"`
	Speed   *int    `json:"speed,omitempty"`
	LogFile *string `json:"log_file,omitempty"`
}

type readIn struct {
	Channel  *int `json:"channel,omitempty"`
	MaxBytes *int `json:"max_bytes,omitempty"`
}

type readLogIn struct {
	MaxBytes *int `json:"max_bytes,omitempty"`
}

type readRawIn struct {
	Offset   *int64 `json:"offset,omitempty"`
	MaxBytes *int   `json:"max_bytes,omitempty"`
}

type waitIn struct {
	Pattern   *string `json:"pattern,omitempty"`
	TimeoutMs *int    `json:"timeout_ms,omitempty"`
	MaxBytes  *int    `json:"max_bytes,omitempty"`
	Offset    *int64  `json:"offset,omitempty"`
}

type writeIn struct {
	Channel *int    `json:"channel,omitempty"`
	Data    *string `json:"data,omitempty"`
}

type readMemIn struct {
	Addr  *string `json:"addr,omitempty"`
	Count *int    `json:"count,omitempty"`
}

// ---- handlers ----

func handleConnect(ctx context.Context, req *mcp.CallToolRequest, in connectIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	defer c.TouchIdle() // refresh idle watchdog on any connect attempt that keeps the probe held
	serial := derefStr(in.Serial)
	device := derefStr(in.Device)
	speed := derefInt(in.Speed)
	logFile := derefStr(in.LogFile)

	// If already connected and the caller passed any parameter that conflicts
	// with the live connection, force a reconnect so the new value actually
	// takes effect. Empty / zero means "leave alone" (use default). A log_file
	// that resolves to a different path than the active one is a conflict too —
	// this is how a second workspace redirects the shared daemon's broadcast log
	// to its own per-project file.
	if c.IsConnected() {
		st := c.Status()
		logConflict := false
		if logFile != "" {
			if p, err := rttcore.ResolveLogPath(logFile); err == nil && p != st.LogFile {
				logConflict = true
			}
		}
		conflict := (device != "" && device != st.DeviceName) ||
			(speed != 0 && speed != st.Speed) ||
			(serial != "" && serial != st.Serial) ||
			logConflict
		if !conflict {
			return text(fmt.Sprintf(
				"Already connected to J-Link device '%s' (serial: %s, speed: %d kHz)\nRTT monitoring active on channel %d (shared connection)",
				st.DeviceName, st.Serial, st.Speed, st.Channel)), nil, nil
		}
		// Reconnect to honor the new parameters.
		c.Disconnect()
	}

	if err := c.Connect(serial, device, speed, logFile); err != nil {
		return text("Failed to connect to J-Link.\n\nError:\n" + err.Error()), nil, nil
	}
	st := c.Status()
	return text(fmt.Sprintf(
		"Connected to J-Link device '%s' (serial: %s, speed: %d kHz)\nRTT monitoring started on channel %d",
		st.DeviceName, st.Serial, st.Speed, st.Channel)), nil, nil
}

func handleDisconnect(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	// No TouchIdle: this releases the probe; the watchdog is stopped by Disconnect itself.
	if !c.IsConnected() {
		return text("J-Link is not connected."), nil, nil
	}
	c.Disconnect()
	return text("J-Link disconnected successfully."), nil, nil
}

func handleRead(ctx context.Context, req *mcp.CallToolRequest, in readIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	defer c.TouchIdle()
	// Transparently re-establish the probe if the idle watchdog released it, so a
	// read after an idle gap resumes the live stream instead of reporting "not
	// connected".
	if err := c.EnsureConnected(); err != nil {
		return text(err.Error()), nil, nil
	}
	data := c.Read(derefInt(in.MaxBytes))
	if data == "" {
		return text("(no RTT data)"), nil, nil
	}
	return text(data), nil, nil
}

func handleReadLog(ctx context.Context, req *mcp.CallToolRequest, in readLogIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	defer c.TouchIdle()
	data := c.ReadLogTail(derefInt(in.MaxBytes))
	if data == "" {
		if !c.IsConnected() {
			return text("J-Link is not connected — call jlink_connect first (nothing to poll; stop polling until connected)."), nil, nil
		}
		return text("(no RTT log yet)"), nil, nil
	}
	return text(data), nil, nil
}

func handleReadRaw(ctx context.Context, req *mcp.CallToolRequest, in readRawIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	defer c.TouchIdle()
	var offset int64
	if in.Offset != nil {
		offset = *in.Offset
	}
	data, next := c.ReadLogRaw(offset, derefInt(in.MaxBytes))
	// Surface live connection state so a polling monitor/agent can stop when the
	// probe is not connected instead of busy-looping on empty reads.
	out, _ := json.Marshal(map[string]any{"data": data, "next_offset": next, "connected": c.IsConnected()})
	return text(string(out)), nil, nil
}

// handleWait blocks until new RTT output (optionally matching a regex) arrives
// or the timeout elapses, collapsing an N-poll wait-for-reply loop into one
// call. It reads the non-draining broadcast log so it never steals bytes from
// rtt_read or the VSCode monitor. req.Context() propagation lets the client
// cancel a long wait.
func handleWait(ctx context.Context, req *mcp.CallToolRequest, in waitIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	defer c.TouchIdle()
	// Transparently re-establish the probe if idle-released; if it can't connect,
	// there is nothing to wait for, so report it rather than blocking pointlessly.
	if err := c.EnsureConnected(); err != nil {
		out, _ := json.Marshal(map[string]any{"matched": false, "timed_out": false, "data": "", "connected": false, "error": err.Error()})
		return text(string(out)), nil, nil
	}
	var re *regexp.Regexp
	if in.Pattern != nil && *in.Pattern != "" {
		r, err := regexp.Compile(*in.Pattern)
		if err != nil {
			return text("Invalid regex pattern: " + err.Error()), nil, nil
		}
		re = r
	}
	timeoutMs := derefInt(in.TimeoutMs)
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	if timeoutMs > 60000 {
		timeoutMs = 60000
	}
	// Start offset: an explicit `offset` pins it (same convention as
	// rtt_read_raw); the default resumes from the last rtt_write's pre-write
	// position, so a reply that arrived during the write→wait round-trip gap —
	// the normal case for request/response protocols — is still captured.
	var offset int64 = -1
	if in.Offset != nil && *in.Offset >= 0 {
		offset = *in.Offset
	}
	matched, data := c.WaitForFrom(ctx, re, time.Duration(timeoutMs)*time.Millisecond, derefInt(in.MaxBytes), offset)
	out, _ := json.Marshal(map[string]any{
		"matched":   matched,
		"timed_out": !matched,
		"data":      data,
		"connected": c.IsConnected(),
	})
	return text(string(out)), nil, nil
}

func handleWrite(ctx context.Context, req *mcp.CallToolRequest, in writeIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	defer c.TouchIdle()
	// Transparently re-establish the probe if the idle watchdog released it: a
	// write that follows a successful connect must not fail with "not connected"
	// just because the idle window elapsed. This is the fix for the connect→write
	// state mismatch.
	if err := c.EnsureConnected(); err != nil {
		return text(err.Error()), nil, nil
	}
	if in.Data == nil || *in.Data == "" {
		return text("No data provided to write."), nil, nil
	}
	raw, err := unescape(*in.Data)
	if err != nil {
		return text("Invalid escape in data: " + err.Error() +
			"\nSupported: \\r \\n \\t \\0 \\\\ \\xNN"), nil, nil
	}
	channel := c.Config().Channel
	if in.Channel != nil {
		channel = *in.Channel
	}
	raw = appendLineEnding(raw)
	n := c.Write(channel, string(raw))
	if n < 0 {
		return text("Failed to write to RTT."), nil, nil
	}
	return text(fmt.Sprintf("Wrote %d bytes to RTT channel %d", n, channel)), nil, nil
}

func handleListDevices(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	devices := c.ListDevices()
	if len(devices) == 0 {
		return text("No J-Link devices found. Make sure J-Link is connected."), nil, nil
	}
	var b []byte
	b = append(b, "Available J-Link devices:\n"...)
	for _, d := range devices {
		b = append(b, "  - "...)
		b = append(b, d...)
		b = append(b, '\n')
	}
	return text(string(b)), nil, nil
}

type listSupportedIn struct {
	Query *string `json:"query,omitempty"`
	Limit *int    `json:"limit,omitempty"`
}

func handleListSupportedDevices(ctx context.Context, req *mcp.CallToolRequest, in listSupportedIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	n := c.SupportedDeviceCount()
	query := strings.ToLower(strings.TrimSpace(derefStr(in.Query)))
	limit := derefInt(in.Limit)
	if limit <= 0 {
		limit = 20000
	}
	var b []byte
	b = append(b, fmt.Sprintf("%d device(s) in the J-Link database.\n", n)...)
	matched := 0
	for i := 0; i < n && matched < limit; i++ {
		name := c.SupportedDeviceName(i)
		if name == "" {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(name), query) {
			continue
		}
		b = append(b, name...)
		b = append(b, '\n')
		matched++
	}
	if matched >= limit && n > limit {
		b = append(b, fmt.Sprintf("... truncated at %d matches; pass a more specific `query`.\n", limit)...)
	}
	return text(string(b)), nil, nil
}

type checkDeviceIn struct {
	Device *string `json:"device,omitempty"`
}

func handleCheckDevice(ctx context.Context, req *mcp.CallToolRequest, in checkDeviceIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	dev := strings.TrimSpace(derefStr(in.Device))
	if dev == "" {
		return text("No device name given."), nil, nil
	}
	idx := c.SupportedDeviceIndex(dev)
	if idx > 0 {
		return text(fmt.Sprintf("Supported: %q is in the J-Link device database (index %d).", dev, idx)), nil, nil
	}
	return text(fmt.Sprintf("Not supported: %q is NOT in the J-Link device database. Use rtt_list_supported_devices to find the exact name J-Link expects.", dev)), nil, nil
}

// handleReadMem serves jlink_read_mem: a non-intrusive debug-interface memory
// read for inspecting variables and memory-mapped peripheral registers. It
// follows the same connection pattern as read/write (transparent reconnect on
// idle release), so it composes with a live RTT session instead of racing it.
func handleReadMem(ctx context.Context, req *mcp.CallToolRequest, in readMemIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	defer c.TouchIdle()
	if err := c.EnsureConnected(); err != nil {
		return text(err.Error()), nil, nil
	}
	addrStr := strings.TrimSpace(derefStr(in.Addr))
	if addrStr == "" {
		return text("No address provided. Pass addr as hex, e.g. addr=\"0x20000000\" (RAM) or \"40021000\" (peripheral register)."), nil, nil
	}
	addr, err := parseHexAddr(addrStr)
	if err != nil {
		return text(fmt.Sprintf("Invalid address %q: %v", addrStr, err)), nil, nil
	}
	words, err := c.ReadMem(addr, derefInt(in.Count))
	if err != nil {
		return text(fmt.Sprintf("Memory read at 0x%08X failed: %v", addr, err)), nil, nil
	}
	return text(hexDump(addr, words)), nil, nil
}

// handleReset serves jlink_reset: reset the target without halting so the
// live RTT session rides through the reboot and captures the boot banner.
func handleReset(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	defer c.TouchIdle()
	if err := c.EnsureConnected(); err != nil {
		return text(err.Error()), nil, nil
	}
	if err := c.ResetTarget(); err != nil {
		return text("Reset failed: " + err.Error()), nil, nil
	}
	return text("Target reset (no halt). RTT session continues — use rtt_wait to capture boot output."), nil, nil
}

func handleStatus(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	defer c.TouchIdle()
	// Transparently re-establish the probe if the idle watchdog released it, so a
	// status check after an idle gap reports a live connection instead of leaking
	// the internal "idle-disconnected" state. Consistent with read/write/clear.
	if err := c.EnsureConnected(); err != nil {
		return text(err.Error()), nil, nil
	}
	st := c.Status()
	lastLine := "never"
	if !st.LastLineAt.IsZero() {
		lastLine = fmt.Sprintf("%s (%s ago)", st.LastLineAt.Format("15:04:05.000"), time.Since(st.LastLineAt).Round(time.Millisecond))
	}
	return text(fmt.Sprintf(`J-Link Status:
  Connected: %v
  RTT Started: %v
  Device: %s
  Serial: %s
  Speed: %d kHz
  Channel: %d
  Buffer Size: %d entries
  Log File: %s
  Lines since connect: %d
  Bytes read: %d
  Last line: %s
  Dropped lines (ring overflow): %d
  Saturated reads (possible device loss): %d
`, st.Connected, st.RTTStarted, st.DeviceName, st.Serial, st.Speed, st.Channel, st.RingBufferSize, st.LogFile,
		st.LinesSinceConnect, st.BytesRead, lastLine, st.DroppedLines, st.SaturatedReads)), nil, nil
}

func handleClear(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	defer c.TouchIdle()
	// Transparently re-establish the probe if the idle watchdog released it, so a
	// clear after an idle gap works instead of reporting "not connected". This
	// keeps read/write/clear/status uniform: all four auto-reconnect on idle.
	if err := c.EnsureConnected(); err != nil {
		return text(err.Error()), nil, nil
	}
	c.Clear()
	return text("RTT buffer cleared."), nil, nil
}

// ---- helpers ----

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: s}},
	}
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// parseHexAddr parses a 32-bit address written in hex, with or without the 0x
// prefix ("0x20000000" and "20000000" are equivalent). Hex-only is deliberate:
// addresses are conventionally hex, and accepting decimal too would silently
// reinterpret a bare "20000000" (decimal 0x1312D00) instead of erroring.
func parseHexAddr(s string) (uint32, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("want hex like \"0x20000000\" or \"20000000\"")
	}
	return uint32(v), nil
}

// hexDump formats words as 4-per-line with a running address prefix, the shape
// agents and humans expect from a memory dump:
//
//	20000000: 20000000 20000004 20000008 2000000C
func hexDump(base uint32, words []uint32) string {
	var b bytes.Buffer
	for i := 0; i < len(words); i += 4 {
		end := i + 4
		if end > len(words) {
			end = len(words)
		}
		fmt.Fprintf(&b, "%08X:", base+uint32(i*4))
		for _, w := range words[i:end] {
			fmt.Fprintf(&b, " %08X", w)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// unescape interprets C-style escape sequences in s and returns the raw bytes.
// Supported: \r \n \t \0 \\ \xNN (exactly two hex digits). Unknown or malformed
// escapes return an error so a typo can't silently send the wrong bytes — a
// literal backslash must be written as \\. This lets a caller send AT\r\n and
// get 'A','T',CR,LF (4 bytes) instead of the 6 literal characters.
func unescape(s string) ([]byte, error) {
	b := []byte(s)
	var out []byte
	for i := 0; i < len(b); {
		if b[i] != '\\' {
			out = append(out, b[i])
			i++
			continue
		}
		if i+1 >= len(b) {
			return nil, fmt.Errorf("dangling '\\' at end of input")
		}
		switch b[i+1] {
		case 'r':
			out = append(out, '\r')
			i += 2
		case 'n':
			out = append(out, '\n')
			i += 2
		case 't':
			out = append(out, '\t')
			i += 2
		case '0':
			out = append(out, 0)
			i += 2
		case '\\':
			out = append(out, '\\')
			i += 2
		case 'x':
			if i+3 >= len(b) {
				return nil, fmt.Errorf("\\x must be followed by exactly two hex digits")
			}
			hi, ok1 := hexVal(b[i+2])
			lo, ok2 := hexVal(b[i+3])
			if !ok1 || !ok2 {
				return nil, fmt.Errorf("invalid hex digit in %q", s[i:i+4])
			}
			out = append(out, byte(hi<<4|lo))
			i += 4
		default:
			return nil, fmt.Errorf("unknown escape sequence \\%c", b[i+1])
		}
	}
	return out, nil
}

// hexVal maps an ASCII hex digit to its 0–15 value (ok=false if not hex).
func hexVal(b byte) (int, bool) {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0'), true
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10, true
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10, true
	}
	return 0, false
}

// appendLineEnding appends \r\n to raw unless it already ends with \r or \n,
// so callers can pass a bare command like "AT" and have the device's line
// parser process it. This mirrors the VSCode panel's webview send path
// (extension.ts: provider.write(`${msg.text}\r\n`)) and resolves the
// "MCP send → no reply" asymmetry where the panel auto-appended a terminator
// but the MCP tool did not. Data already terminated is sent as-is, and empty
// input is left empty.
func appendLineEnding(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	last := raw[len(raw)-1]
	if last == '\r' || last == '\n' {
		return raw
	}
	return append(raw, '\r', '\n')
}
