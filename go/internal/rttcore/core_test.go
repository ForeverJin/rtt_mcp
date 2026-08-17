package rttcore

import (
	"path/filepath"
	"strings"
	"testing"

	"rtt-mcp-server/internal/config"
	"rtt-mcp-server/internal/jlink"
)

// testConfig builds a config whose log lives in the test's temp dir and whose
// idle watchdog is disabled, so Connect/Disconnect are driven explicitly.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Device:         "Cortex-M0+",
		Speed:          4000,
		Channel:        0,
		RingBufferSize: 100,
		PollIntervalMs: 10,
		RAMStart:       0x20000000,
		RAMSize:        0x8000,
		LogFile:        filepath.Join(t.TempDir(), "rtt.log"),
		IdleTimeoutSec: 0,
	}
}

// Before any connect, EnsureConnected must report that the probe was never
// connected rather than pretending success.
func TestEnsureConnected_NeverConnected(t *testing.T) {
	c := NewCore(jlink.NewMockBackend(), testConfig(t))
	if err := c.EnsureConnected(); err == nil {
		t.Fatal("EnsureConnected before any connect: want error, got nil")
	}
}

// The core fix: after the idle watchdog releases the probe, a read/write via
// EnsureConnected transparently re-establishes it instead of failing.
func TestEnsureConnected_ReconnectsAfterIdle(t *testing.T) {
	c := NewCore(jlink.NewMockBackend(), testConfig(t))
	defer c.Disconnect()

	if err := c.Connect("", "", 0, ""); err != nil {
		t.Fatalf("initial Connect: %v", err)
	}
	if !c.IsConnected() {
		t.Fatal("not connected after Connect")
	}

	// Simulate the idle watchdog firing.
	c.Disconnect()
	if c.IsConnected() {
		t.Fatal("still connected after Disconnect")
	}

	// This is the line that used to surface as "J-Link is not connected".
	if err := c.EnsureConnected(); err != nil {
		t.Fatalf("EnsureConnected after idle: want nil, got %v", err)
	}
	if !c.IsConnected() {
		t.Fatal("not connected after EnsureConnected (lazy reconnect failed)")
	}
}

// After a successful connect then an idle release, if the probe can no longer
// be reached (simulated unplug), EnsureConnected must fail once and then serve
// the cooldown error WITHOUT paying another failing bring-up, so polling read
// tools stop busy-looping. Once the backend recovers and the cooldown elapses,
// EnsureConnected reconnects again.
func TestEnsureConnected_ReconnectCooldown(t *testing.T) {
	mb := jlink.NewConfigurableMock()
	c := NewCore(mb, testConfig(t))
	defer c.Disconnect()

	if err := c.Connect("", "", 0, ""); err != nil {
		t.Fatalf("initial Connect: %v", err)
	}
	// Simulate the idle watchdog releasing the probe, then an unplug.
	c.Disconnect()
	mb.SetFailConnect(true)

	// First reconnect attempt actually tries and fails.
	err1 := c.EnsureConnected()
	if err1 == nil {
		t.Fatal("EnsureConnected after unplug: want error, got nil")
	}
	if err1 == errReconnectCooldown {
		t.Fatalf("first attempt should try (not cooldown), got %v", err1)
	}

	// Immediate retry must be gated by the cooldown (no second failing bring-up).
	if err2 := c.EnsureConnected(); err2 != errReconnectCooldown {
		t.Fatalf("second attempt: want cooldown error, got %v", err2)
	}
}

// EnsureConnected on an already-connected probe is a no-op (no double bring-up).
func TestEnsureConnected_NoOpWhenConnected(t *testing.T) {
	c := NewCore(jlink.NewMockBackend(), testConfig(t))
	defer c.Disconnect()
	if err := c.Connect("", "", 0, ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := c.EnsureConnected(); err != nil {
		t.Fatalf("EnsureConnected when connected: want nil, got %v", err)
	}
	if !c.IsConnected() {
		t.Fatal("disconnected by EnsureConnected no-op path")
	}
}

// Connect is idempotent: calling it twice does not error or double-open.
func TestConnect_Idempotent(t *testing.T) {
	c := NewCore(jlink.NewMockBackend(), testConfig(t))
	defer c.Disconnect()
	if err := c.Connect("", "", 0, ""); err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	if err := c.Connect("", "", 0, ""); err != nil {
		t.Fatalf("second Connect: want nil, got %v", err)
	}
	if !c.IsConnected() {
		t.Fatal("not connected after double Connect")
	}
}

// A lazy reconnect must not wipe the broadcast log: content captured during a
// session must survive the transparent re-establishment after idle.
func TestReconnect_PreservesLog(t *testing.T) {
	cfg := testConfig(t)
	c := NewCore(jlink.NewMockBackend(), cfg)
	defer c.Disconnect()

	if err := c.Connect("", "", 0, ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// Stamp a marker line into the broadcast log while connected.
	c.ingest([]byte("before-idle-line\n"))
	c.Disconnect()

	// Reconnect via EnsureConnected (reconnect == true �?append mode, no truncate).
	if err := c.EnsureConnected(); err != nil {
		t.Fatalf("EnsureConnected: %v", err)
	}
	got := c.ReadLogTail(65536)
	if !strings.Contains(got, "before-idle-line") {
		t.Fatalf("log wiped on reconnect; tail = %q", got)
	}
}

// ReadMem returns the mock's synthetic words (each word's own address as its
// value), honours an explicit word count, and applies the runaway-count cap.
func TestReadMem_MockWords(t *testing.T) {
	c := NewCore(jlink.NewMockBackend(), testConfig(t))
	defer c.Disconnect()
	if err := c.Connect("", "", 0, ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	words, err := c.ReadMem(0x20000000, 8)
	if err != nil {
		t.Fatalf("ReadMem: %v", err)
	}
	if len(words) != 8 {
		t.Fatalf("len(words) = %d, want 8", len(words))
	}
	for i, w := range words {
		if want := uint32(0x20000000 + i*4); w != want {
			t.Fatalf("words[%d] = %#X, want %#X", i, w, want)
		}
	}
}

func TestReadMem_DefaultAndCappedCount(t *testing.T) {
	c := NewCore(jlink.NewMockBackend(), testConfig(t))
	defer c.Disconnect()
	if err := c.Connect("", "", 0, ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	words, err := c.ReadMem(0x20000000, 0)
	if err != nil {
		t.Fatalf("ReadMem(default): %v", err)
	}
	if len(words) != 16 {
		t.Fatalf("default count: len(words) = %d, want 16", len(words))
	}
	words, err = c.ReadMem(0x20000000, 10_000_000)
	if err != nil {
		t.Fatalf("ReadMem(capped): %v", err)
	}
	if len(words) != maxMemWords {
		t.Fatalf("capped count: len(words) = %d, want %d", len(words), maxMemWords)
	}
}
