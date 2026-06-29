// Package rttcore is the single-owner RTT engine: it owns the J-Link probe,
// runs the background monitor that is the sole reader of the physical RTT
// up-buffer, and fans every complete line out to three sinks — the rotating
// broadcast log, stderr, and a bounded in-memory ring. It is a faithful port of
// the Python server's jlink_rtt.py singleton.
package rttcore

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"rtt-mcp-server/internal/config"
	"rtt-mcp-server/internal/jlink"
)

// maxLineBuf caps the partial-line accumulator before a newline arrives; on
// overflow the tail half is retained (matching Python's _monitor_loop).
const maxLineBuf = 4096

// rttMagic is the control-block signature scanned for in RAM.
const rttMagic = "SEGGER RTT"

// Status is the cached connection snapshot returned by the jlink_status tool.
type Status struct {
	Connected      bool
	RTTStarted     bool
	DeviceName     string
	Serial         string
	Speed          int
	Channel        int
	RingBufferSize int // current ring entries
}

// Core holds the singleton RTT engine state.
type Core struct {
	mu         sync.Mutex // guards connect/disconnect lifecycle + state fields
	cfg        *config.Config
	backend    jlink.RTTBackend
	ring       *ring
	log        *logSink
	lineMu     sync.Mutex // guards lineBuf during monitor flushes
	lineBuf    string
	running    bool
	rttStarted bool
	device     string // device actually connected (reported by Status)
	speed      int    // speed actually used
	serial     string // serial actually used
	stopCh     chan struct{}
	wg         sync.WaitGroup

	// Idle watchdog: releases the probe when no active tool call has touched
	// it for idleTimeout. Disabled when idleTimeout == 0.
	idleMu      sync.Mutex
	idleTimer   *time.Timer
	idleTimeout time.Duration
}

var (
	coreOnce sync.Once
	coreInst *Core
)

// Get returns the process-global Core, lazily creating it from the environment.
// The backend is the real purego-backed probe unless RTT_MOCK is set.
func Get() *Core {
	coreOnce.Do(func() {
		coreInst = newCoreFromEnv()
	})
	return coreInst
}

func newCoreFromEnv() *Core {
	cfg := config.Load()
	var backend jlink.RTTBackend
	if os.Getenv("RTT_MOCK") != "" {
		backend = jlink.NewMockBackend()
	} else {
		backend = jlink.NewBackend()
	}
	return &Core{
		cfg:     cfg,
		backend: backend,
		ring:    newRing(cfg.RingBufferSize),
	}
}

// NewCore constructs a Core with an explicit backend and config (for tests).
func NewCore(backend jlink.RTTBackend, cfg *config.Config) *Core {
	return &Core{
		cfg:     cfg,
		backend: backend,
		ring:    newRing(cfg.RingBufferSize),
	}
}

// Config exposes the active configuration (tools read device defaults from it).
func (c *Core) Config() *config.Config { return c.cfg }

// IsConnected reports whether the probe is open and the monitor is running,
// matching Python's is_connected().
func (c *Core) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.backend.Opened() && c.running
}

// Connect runs the full probe bring-up: open → SWD → device connect → RTT
// control-block discovery → start monitor. Returns nil on success.
func (c *Core) Connect(serial, device string, speed int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if device == "" {
		device = c.cfg.Device
	}
	if speed == 0 {
		speed = c.cfg.Speed
	}
	if serial == "" {
		serial = c.cfg.Serial
	}

	// 1. Open the probe.
	if err := c.backend.Open(serial); err != nil {
		return fmt.Errorf("open J-Link: %w", err)
	}

	// 2. SWD, then connect to the target (fall back to Cortex-M0+).
	c.backend.SetTifSWD()
	var lastDevErr error
	connected := false
	for _, dev := range deviceCandidates(device, c.backend.SupportedDeviceIndex) {
		c.backend.SetSpeed(speed)
		if err := c.backend.ConnectDevice(dev); err != nil {
			lastDevErr = err
			continue
		}
		connected = true
		c.device = dev // track the device that actually connected
		c.speed = speed
		c.serial = serial
		break
	}
	if !connected {
		c.backend.Close()
		if lastDevErr != nil {
			return fmt.Errorf("connect device: %w", lastDevErr)
		}
		return fmt.Errorf("connect device failed")
	}

	// 3. Force SWD again (mirrors the Python sequence).
	c.backend.SetTifSWD()

	// 4. Discover the RTT control block in RAM; start RTT (auto on miss).
	addr := c.findRTTAddress()
	if addr >= 0 {
		if err := c.backend.RTTStart(int(addr)); err != nil {
			c.backend.Close()
			return fmt.Errorf("rtt_start(0x%X): %w", addr, err)
		}
	} else {
		if err := c.backend.RTTStart(-1); err != nil {
			c.backend.Close()
			return fmt.Errorf("rtt_start (auto): %w", err)
		}
	}
	c.rttStarted = true

	// 5. Let RTT settle, then probe buffers and drain any initial data.
	time.Sleep(500 * time.Millisecond)
	if n, err := c.backend.RTTNumUpBuffers(); err == nil {
		fmt.Fprintf(os.Stderr, "[JLink] up buffers: %d\n", n)
	}
	if n, err := c.backend.RTTNumDownBuffers(); err == nil {
		fmt.Fprintf(os.Stderr, "[JLink] down buffers: %d\n", n)
	}
	if data, err := c.backend.RTTRead(c.cfg.Channel, 512); err == nil && len(data) > 0 {
		c.ingest(data)
	}

	// 6. (Re)open the broadcast log, truncating any prior content.
	path, err := resolveLogPath(c.cfg.LogFile)
	if err != nil {
		c.backend.RTTStop()
		c.backend.Close()
		return fmt.Errorf("resolve log path: %w", err)
	}
	sink, err := openLog(path)
	if err != nil {
		c.backend.RTTStop()
		c.backend.Close()
		return fmt.Errorf("open log: %w", err)
	}
	c.log = sink

	// 7. Start the monitor goroutine.
	c.running = true
	c.stopCh = make(chan struct{})
	c.wg.Add(1)
	go c.monitorLoop()

	// 8. Arm the idle watchdog so the probe is released when no client uses it.
	c.idleTimeout = time.Duration(c.cfg.IdleTimeoutSec) * time.Second
	c.startIdleTimer()

	fmt.Fprintf(os.Stderr, "[JLink] Connected (%s), RTT on channel %d\n", c.backend.ProductName(), c.cfg.Channel)
	return nil
}

// Disconnect stops the monitor, flushes any partial line, closes the log, and
// releases the probe. Idempotent.
func (c *Core) Disconnect() {
	// Stop the idle watchdog first so its fire callback can't race this call.
	c.stopIdleTimer()
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	c.running = false
	close(c.stopCh)
	c.mu.Unlock()

	c.wg.Wait()

	c.lineMu.Lock()
	if c.lineBuf != "" {
		line := c.lineBuf
		c.lineBuf = ""
		c.lineMu.Unlock()
		c.flushLine(line)
	} else {
		c.lineMu.Unlock()
	}

	if c.log != nil {
		c.log.close()
		c.log = nil
	}
	c.backend.RTTStop()
	c.backend.Close()
	c.rttStarted = false
	fmt.Fprintln(os.Stderr, "[JLink] Disconnected")
}

// TouchIdle resets the idle watchdog. Called by every "active" tool handler
// (read/write/status/connect/disconnect) so the probe is released only when no
// client has used it for idleTimeout. No-op when not connected or disabled.
func (c *Core) TouchIdle() {
	c.idleMu.Lock()
	defer c.idleMu.Unlock()
	if c.idleTimer != nil {
		c.idleTimer.Reset(c.idleTimeout)
	}
}

// startIdleTimer arms the idle watchdog after a successful Connect. On fire it
// releases the probe (Disconnect) but leaves the daemon process running, so the
// next jlink_connect reconnects. No-op when disabled.
func (c *Core) startIdleTimer() {
	c.idleMu.Lock()
	defer c.idleMu.Unlock()
	if c.idleTimeout <= 0 {
		return
	}
	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}
	c.idleTimer = time.AfterFunc(c.idleTimeout, func() {
		fmt.Fprintln(os.Stderr, "[JLink] idle timeout — releasing probe (daemon stays up)")
		c.Disconnect()
	})
}

// stopIdleTimer disarms the watchdog (called from Disconnect to avoid racing an
// in-flight fire callback).
func (c *Core) stopIdleTimer() {
	c.idleMu.Lock()
	defer c.idleMu.Unlock()
	if c.idleTimer != nil {
		c.idleTimer.Stop()
		c.idleTimer = nil
	}
}

// monitorLoop is the sole reader of the physical RTT up-buffer. It line-buffers
// incoming bytes and flushes each complete line to the triple sink. Other reads
// (rtt_read) consume the ring, so they never steal device bytes.
func (c *Core) monitorLoop() {
	defer c.wg.Done()
	interval := time.Duration(c.cfg.PollIntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}
		if !c.backend.Opened() {
			select {
			case <-c.stopCh:
				return
			case <-time.After(interval):
			}
			continue
		}
		data, err := c.backend.RTTRead(c.cfg.Channel, 512)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[monitor] read error: %v\n", err)
			select {
			case <-c.stopCh:
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		if len(data) > 0 {
			c.ingest(data)
		}
		select {
		case <-c.stopCh:
			return
		case <-time.After(interval):
		}
	}
}

// ingest appends raw bytes to the line buffer and flushes every complete line.
func (c *Core) ingest(data []byte) {
	c.lineMu.Lock()
	defer c.lineMu.Unlock()
	c.lineBuf += string(data)
	if len(c.lineBuf) > maxLineBuf {
		c.lineBuf = c.lineBuf[len(c.lineBuf)-maxLineBuf/2:]
	}
	for {
		idx := strings.IndexByte(c.lineBuf, '\n')
		if idx < 0 {
			break
		}
		line := c.lineBuf[:idx]
		c.lineBuf = c.lineBuf[idx+1:]
		c.flushLine(line)
	}
}

// flushLine timestamps the line and writes it to the three sinks: log, stderr,
// ring. Empty lines are dropped (matching Python).
func (c *Core) flushLine(line string) {
	if line == "" {
		return
	}
	stamped := time.Now().Format("[15:04:05.000]") + " " + line + "\n"
	if c.log != nil {
		c.log.write(stamped)
	}
	fmt.Fprint(os.Stderr, stamped)
	c.ring.append(stamped)
}

// findRTTAddress scans RAM in 4 KiB chunks for the RTT control-block magic,
// reading 32-bit words and packing little-endian. Returns the byte address, or
// -1 if not found (then RTT starts in auto mode).
func (c *Core) findRTTAddress() int64 {
	const chunkWords = 1024
	totalWords := int(c.cfg.RAMSize / 4)
	for off := 0; off < totalWords; off += chunkWords {
		n := chunkWords
		if off+n > totalWords {
			n = totalWords - off
		}
		addr := c.cfg.RAMStart + uint32(off*4)
		words, err := c.backend.MemoryRead32(addr, n)
		if err != nil {
			continue
		}
		chunk := make([]byte, len(words)*4)
		for i, w := range words {
			chunk[i*4+0] = byte(w)
			chunk[i*4+1] = byte(w >> 8)
			chunk[i*4+2] = byte(w >> 16)
			chunk[i*4+3] = byte(w >> 24)
		}
		if idx := bytes.Index(chunk, []byte(rttMagic)); idx >= 0 {
			return int64(addr) + int64(idx)
		}
	}
	return -1
}

// deviceCandidates returns the list of device names to try, in order. Only
// names that the loaded J-Link DLL recognises are included — names absent
// from the device database are silently skipped so JLINKARM_ExecCommand never
// gets handed an unknown string (which would pop a system dialog). Falls back
// to "Cortex-M0+" only if the generic CPU is in the database.
func deviceCandidates(device string, isValid func(string) int) []string {
	var out []string
	if device != "" && isValid(device) > 0 {
		out = append(out, device)
	}
	if device != "Cortex-M0+" && isValid("Cortex-M0+") > 0 {
		out = append(out, "Cortex-M0+")
	}
	if len(out) == 0 {
		// Last resort: include whatever the user asked for so the caller
		// surfaces a clear "device not supported" error instead of a silent
		// empty list.
		if device != "" {
			out = append(out, device)
		} else {
			out = append(out, "Cortex-M0+")
		}
	}
	return out
}

// Read drains and returns the in-memory ring, keeping the last maxBytes.
func (c *Core) Read(maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 512
	}
	return c.ring.drain(maxBytes)
}

// ReadLogTail returns the tail of the broadcast log (non-draining).
func (c *Core) ReadLogTail(maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 8192
	}
	if c.log == nil {
		return ""
	}
	return c.log.readTail(maxBytes)
}

// ReadLogRaw returns bytes from offset in the broadcast log, with rotation
// detection (non-draining, multi-consumer safe).
func (c *Core) ReadLogRaw(offset int64, maxBytes int) (string, int64) {
	if maxBytes <= 0 {
		maxBytes = 8192
	}
	if c.log == nil {
		return "", 0
	}
	return c.log.readRaw(offset, maxBytes)
}

// Write sends data to the down-buffer, returning bytes written (-1 on error).
func (c *Core) Write(channel int, data string) int {
	if channel < 0 {
		channel = c.cfg.Channel
	}
	n, err := c.backend.RTTWrite(channel, []byte(data))
	if err != nil {
		return -1
	}
	return n
}

// ListDevices enumerates connected probes (best-effort).
func (c *Core) ListDevices() []string {
	return c.backend.ListDevices()
}

// SupportedDeviceCount / SupportedDeviceName / SupportedDeviceIndex expose the
// J-Link device database to the list/validate tools. They are probe-less: only
// the loaded SEGGER DLL is consulted, so they work before jlink_connect.
func (c *Core) SupportedDeviceCount() int             { return c.backend.SupportedDeviceCount() }
func (c *Core) SupportedDeviceName(i int) string      { return c.backend.SupportedDeviceName(i) }
func (c *Core) SupportedDeviceIndex(name string) int  { return c.backend.SupportedDeviceIndex(name) }

// Clear empties the in-memory ring (does not touch device buffers).
func (c *Core) Clear() {
	c.ring.clear()
}

// Status returns a cached snapshot of connection state.
func (c *Core) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	device := c.device
	if device == "" {
		device = c.cfg.Device
	}
	serial := c.serial
	if serial == "" {
		serial = c.cfg.Serial
	}
	speed := c.speed
	if speed == 0 {
		speed = c.cfg.Speed
	}
	return Status{
		Connected:      c.backend.Opened() && c.running,
		RTTStarted:     c.rttStarted,
		DeviceName:     device,
		Serial:         serial,
		Speed:          speed,
		Channel:        c.cfg.Channel,
		RingBufferSize: c.ring.len(),
	}
}
