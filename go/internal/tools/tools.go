// Package tools registers the nine RTT MCP tools on a server and dispatches
// them to the rttcore singleton. Tool names, argument schemas and result text
// are kept byte-for-byte compatible with the Python server so the VSCode
// extension and Claude Code see an identical surface.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

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
			Description: "Write data to RTT down-buffer (host -> device). The target device must be running RTT with a down-buffer listener.",
		}, handleWrite)

	mcp.AddTool(s,
		&mcp.Tool{
			Name:        "rtt_list_devices",
			Description: "List available J-Link debuggers connected to the host.",
		}, handleListDevices)

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
	if c.IsConnected() {
		st := c.Status()
		return text(fmt.Sprintf(
			"Already connected to J-Link device '%s' (serial: %s, speed: %d kHz)\nRTT monitoring active on channel %d (shared connection)",
			st.DeviceName, st.Serial, st.Speed, st.Channel)), nil, nil
	}
	serial := derefStr(in.Serial)
	device := derefStr(in.Device)
	speed := derefInt(in.Speed)
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
	if !c.IsConnected() {
		return text("J-Link is not connected."), nil, nil
	}
	c.Disconnect()
	return text("J-Link disconnected successfully."), nil, nil
}

func handleRead(ctx context.Context, req *mcp.CallToolRequest, in readIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	if !c.IsConnected() {
		return text("J-Link is not connected. Call jlink_connect first."), nil, nil
	}
	data := c.Read(derefInt(in.MaxBytes))
	if data == "" {
		return text("(no RTT data)"), nil, nil
	}
	return text(data), nil, nil
}

func handleReadLog(ctx context.Context, req *mcp.CallToolRequest, in readLogIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	data := c.ReadLogTail(derefInt(in.MaxBytes))
	if data == "" {
		return text("(no RTT log yet — connect first)"), nil, nil
	}
	return text(data), nil, nil
}

func handleReadRaw(ctx context.Context, req *mcp.CallToolRequest, in readRawIn) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
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
	if !c.IsConnected() {
		return text("J-Link is not connected. Call jlink_connect first."), nil, nil
	}
	if in.Data == nil || *in.Data == "" {
		return text("No data provided to write."), nil, nil
	}
	channel := c.Config().Channel
	if in.Channel != nil {
		channel = *in.Channel
	}
	n := c.Write(channel, *in.Data)
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

func handleStatus(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
	c := rttcore.Get()
	if !c.IsConnected() {
		return text("J-Link is not connected."), nil, nil
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
	if !c.IsConnected() {
		return text("J-Link is not connected. Call jlink_connect first."), nil, nil
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
