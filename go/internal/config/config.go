// Package config centralises every environment knob the RTT MCP server reads.
//
// Values and defaults mirror the Python implementation exactly (see
// mcp-rtt-server/.env.example and jlink_rtt.get_instance). The JLINK_RAM_START /
// JLINK_RAM_SIZE values are parsed as hexadecimal (matching int(x, 16)); the
// rest are decimal.
package config

import (
	"os"
	"strconv"
)

// Config holds all runtime configuration sourced from the environment.
type Config struct {
	Serial         string // JLINK_SERIAL (empty → first available probe)
	Device         string // JLINK_DEVICE (default Cortex-M0+)
	Speed          int    // JLINK_SPEED in kHz (default 4000)
	Channel        int    // RTT_CHANNEL (default 0)
	RingBufferSize int    // RTT_RING_BUFFER_SIZE entries (default 100)
	PollIntervalMs int    // RTT_POLL_INTERVAL_MS (default 10)
	RAMStart       uint32 // JLINK_RAM_START hex (default 0x20000000)
	RAMSize        uint32 // JLINK_RAM_SIZE hex (default 0x8000)
	LogFile        string // RTT_LOG_FILE (empty → per-platform default, resolved in rttcore)
	DaemonURL      string // RTT_DAEMON_URL (default http://127.0.0.1:8765/sse)
	AuthToken      string // RTT_AUTH_TOKEN (empty → auth disabled)
	LibPath        string // JLINK_LIB_PATH / RTT_LIB_PATH (empty → auto-detect SEGGER lib)
}

// Load reads configuration from the environment, applying the documented
// defaults for anything unset.
func Load() *Config {
	return &Config{
		Serial:         os.Getenv("JLINK_SERIAL"),
		Device:         envStr("JLINK_DEVICE", "Cortex-M0+"),
		Speed:          envInt("JLINK_SPEED", 4000),
		Channel:        envInt("RTT_CHANNEL", 0),
		RingBufferSize: envInt("RTT_RING_BUFFER_SIZE", 100),
		PollIntervalMs: envInt("RTT_POLL_INTERVAL_MS", 10),
		RAMStart:       envHex("JLINK_RAM_START", 0x20000000),
		RAMSize:        envHex("JLINK_RAM_SIZE", 0x8000),
		LogFile:        os.Getenv("RTT_LOG_FILE"),
		DaemonURL:      envStr("RTT_DAEMON_URL", "http://127.0.0.1:8765/sse"),
		AuthToken:      os.Getenv("RTT_AUTH_TOKEN"),
		LibPath:        firstNonEmpty(os.Getenv("JLINK_LIB_PATH"), os.Getenv("RTT_LIB_PATH")),
	}
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envHex parses an integer that may be written as decimal or 0x-prefixed hex,
// matching Python's int(value, 16) behaviour for the RAM window knobs while
// still accepting plain hex without a prefix (e.g. "20000000").
func envHex(key string, def uint32) uint32 {
	if v := os.Getenv(key); v != "" {
		// strconv.ParseUint with base 0 handles "0x.." and plain decimal.
		if n, err := strconv.ParseUint(v, 0, 64); err == nil {
			return uint32(n)
		}
		// Fall back to base-16 for unprefixed hex like "20000000".
		if n, err := strconv.ParseUint(v, 16, 64); err == nil {
			return uint32(n)
		}
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
