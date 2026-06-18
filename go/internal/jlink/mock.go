package jlink

import (
	"fmt"
	"sync"
	"time"
)

// mockBackend simulates a probe for exercising the monitor/log/ring/tools
// plumbing without hardware. Enabled when RTT_MOCK is set in the environment.
// It emits a synthetic heartbeat line on each RTTRead so the triple-sink path
// (log file + stderr + ring buffer) can be verified end-to-end.
type mockBackend struct {
	mu       sync.Mutex
	opened   bool
	tick     int
	product  string
	core     string
	started  bool
	devices  []string
}

// NewMockBackend returns a backend that needs no SEGGER library or hardware.
func NewMockBackend() RTTBackend {
	return &mockBackend{
		product: "J-LINK Mock (software)",
		core:    "Cortex-M0+",
		devices: []string{"J-Link Mock #0"},
	}
}

func (m *mockBackend) Open(serial string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opened = true
	return nil
}

func (m *mockBackend) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opened = false
	m.started = false
}

func (m *mockBackend) SetTifSWD()      {}
func (m *mockBackend) SetSpeed(int)    {}
func (m *mockBackend) ConnectDevice(d string) error { return nil }

func (m *mockBackend) CoreName() string    { return m.core }
func (m *mockBackend) ProductName() string { return m.product }

func (m *mockBackend) MemoryRead32(addr uint32, count int) ([]uint32, error) {
	out := make([]uint32, count)
	return out, nil
}

func (m *mockBackend) RTTStart(addr int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = true
	return nil
}

func (m *mockBackend) RTTStop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = false
}

func (m *mockBackend) RTTRead(channel, max int) ([]byte, error) {
	m.mu.Lock()
	m.tick++
	n := m.tick
	product := m.product
	opened := m.opened && m.started
	m.mu.Unlock()
	if !opened {
		return nil, nil
	}
	// One heartbeat line per poll, ~matching a firmware task printing to RTT.
	return []byte(fmt.Sprintf("[mock] heartbeat #%d (%s) t=%s\n", n, product, time.Now().Format("15:04:05"))), nil
}

func (m *mockBackend) RTTWrite(channel int, data []byte) (int, error) { return len(data), nil }
func (m *mockBackend) RTTNumUpBuffers() (int, error)                  { return 2, nil }
func (m *mockBackend) RTTNumDownBuffers() (int, error)                { return 2, nil }
func (m *mockBackend) ListDevices() []string                          { return m.devices }

func (m *mockBackend) Opened() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opened
}
