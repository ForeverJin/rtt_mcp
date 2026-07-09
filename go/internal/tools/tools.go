// Package tools registers the nine RTT MCP tools on a server and dispatches
// them to the rttcore singleton. Tool names, argument schemas and result text
// are kept byte-for-byte compatible with the Python server so the VSCode
// extension and Claude Code see an identical surface.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
			Description: "Connect to J-Link debugger and start RTT monitoring. Must be called before reading or writing RTT data.",
		}, handleConnect)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "jlink_disconnect",
			Description: "Disconnect from J-Link debugger and stop RTT monitoring.",
		}, handleDisconnect)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_read",
			Description: "Read accumulated RTT data from the ring buffer. Data is continuously collected by the background monitor thread. Use this to get output from the target device.",
		}, handleRead)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_read_log",
			Description: "Read the tail of the complete RTT log file. This is an independent broadcast sink (not drained by other clients), so it returns the full RTT output even while another client (e.g. the VSCode extension) is streaming via rtt_read. Prefer this over rtt_read when another consumer is active.",
		}, handleReadLog)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_read_raw",
			Description: `Read new bytes from the broadcast log starting at a byte offset. Non-draining and multi-consumer safe: ideal for a continuous monitor that must coexist with other readers without stealing their data. Pass the returned next_offset as 'offset' on the next call; if the log rotated (next_offset > file size), pass offset=0. Returns JSON: {"data": "...", "next_offset": N}.`,
		}, handleReadRaw)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_write",
			Description: "Write data to RTT down-buffer (host -> device). C-style escapes are interpreted so control bytes can be sent: \\r \\n \\t \\0 \\\\ and \\xNN (two hex digits) — e.g. pass AT\\r\\n to send 'AT' followed by CR/LF (4 bytes), not the 6 literal characters. A literal backslash is sent with \\\\. Unknown escapes are rejected. The target device must be running RTT with a down-buffer listener.",
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
	Serial *string `json:"serial,omitempty"`
	Device *string `json:"device,omitempty"`
	Speed  *int    `json:"speed,omitempty"`
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

type writeIn struct {
	Channel *int    `json:"channel,omitempty"`
	Data    *string `json:"data,omitempty"`
}

// ---- handlers ----

func handleConnect(ctx context.Context, req *mcp.CallToolRequest, in connectIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	defer c.TouchIdle() // refresh idle watchdog on any connect attempt that keeps the probe held
	serial := derefStr(in.Serial)
	device := derefStr(in.Device)
	speed := derefInt(in.Speed)

	// If already connected and the caller passed any parameter that conflicts
	// with the live connection, force a reconnect so the new value actually
	// takes effect. Empty / zero means "leave alone" (use default).
	if c.IsConnected() {
		st := c.Status()
		conflict := (device != "" && device != st.DeviceName) ||
			(speed != 0 && speed != st.Speed) ||
			(serial != "" && serial != st.Serial)
		if !conflict {
			return text(fmt.Sprintf(
				"Already connected to J-Link device '%s' (serial: %s, speed: %d kHz)\nRTT monitoring active on channel %d (shared connection)",
				st.DeviceName, st.Serial, st.Speed, st.Channel)), nil, nil
		}
		// Reconnect to honor the new parameters.
		c.Disconnect()
	}

	if err := c.Connect(serial, device, speed); err != nil {
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
		return text("(no RTT log yet — connect first)"), nil, nil
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
	out, _ := json.Marshal(map[string]any{"data": data, "next_offset": next})
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
	return text(fmt.Sprintf(`J-Link Status:
  Connected: %v
  RTT Started: %v
  Device: %s
  Serial: %s
  Speed: %d kHz
  Channel: %d
  Buffer Size: %d entries
`, st.Connected, st.RTTStarted, st.DeviceName, st.Serial, st.Speed, st.Channel, st.RingBufferSize)), nil, nil
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
