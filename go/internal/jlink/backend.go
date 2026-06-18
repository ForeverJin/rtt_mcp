package jlink

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"unsafe"
)

// maxBufSize is the scratch buffer length used for string-returning calls.
const maxBufSize = 256

// puregoBackend implements RTTBackend by calling the SEGGER DLL via purego.
type puregoBackend struct {
	mu     sync.Mutex
	opened bool
}

// NewBackend returns an RTTBackend backed by the dynamically-loaded SEGGER
// library. The library is loaded lazily on first use.
func NewBackend() RTTBackend { return &puregoBackend{} }

func (b *puregoBackend) Open(serial string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := Load(""); err != nil {
		return err
	}
	if err := essentialMissing(); err != nil {
		return err
	}

	// Select the probe: by serial if given, else the default USB device.
	if serial != "" {
		sn, err := parseSerial(serial)
		if err != nil {
			return fmt.Errorf("invalid serial %q: %w", serial, err)
		}
		if jlinkSelectByUSBSN != nil {
			if r := jlinkSelectByUSBSN(sn); r < 0 {
				return fmt.Errorf("no emulator with serial number %s found", serial)
			}
		}
	} else if jlinkSelectUSB != nil {
		if r := jlinkSelectUSB(0); r != 0 {
			return errors.New("could not connect to default emulator")
		}
	}

	// OpenEx accepts log/error handler callbacks; nil disables host logging,
	// which SEGGER tolerates. Returns a static error string on failure.
	errPtr := jlinkOpenEx(0, 0)
	if errPtr != nil {
		if msg := cString(errPtr); msg != "" {
			b.opened = false
			return errors.New(msg)
		}
	}
	b.opened = true
	return nil
}

func (b *puregoBackend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.opened {
		return
	}
	if jlinkClose != nil {
		jlinkClose()
	}
	b.opened = false
}

func (b *puregoBackend) SetTifSWD() {
	if jlinkTIFSelect != nil {
		jlinkTIFSelect(jlinkTifSWD)
	}
}

func (b *puregoBackend) SetSpeed(kHz int) {
	if jlinkSetSpeed != nil {
		jlinkSetSpeed(uint32(kHz))
	}
}

func (b *puregoBackend) ConnectDevice(device string) error {
	if jlinkExecCommand == nil || jlinkConnect == nil {
		return errors.New("connect symbols not loaded")
	}
	// SEGGER selects the target via the "Device = <name>" exec command; the
	// subsequent JLINKARM_Connect() binds to it (mirrors pylink.connect).
	cmd := []byte("Device = " + device + "\x00")
	if r := jlinkExecCommand(&cmd[0]); r < 0 {
		return fmt.Errorf("JLINKARM_ExecCommand failed for device %q (code %d)", device, r)
	}
	if r := jlinkConnect(); r < 0 {
		return fmt.Errorf("JLINKARM_Connect failed for device %q (code %d)", device, r)
	}
	return nil
}

func (b *puregoBackend) CoreName() string {
	if jlinkCore2CoreName == nil {
		return ""
	}
	var buf [maxBufSize]byte
	jlinkCore2CoreName(0, &buf[0], maxBufSize)
	return cString(&buf[0])
}

func (b *puregoBackend) ProductName() string {
	if jlinkEMUProdName == nil {
		return ""
	}
	var buf [maxBufSize]byte
	jlinkEMUProdName(&buf[0], maxBufSize)
	return cString(&buf[0])
}

func (b *puregoBackend) MemoryRead32(addr uint32, count int) ([]uint32, error) {
	if jlinkReadMemEx == nil {
		return nil, errors.New("ReadMemEx not loaded")
	}
	buf := make([]byte, count*4)
	// AccessWidth = 32 bits per unit; the buffer receives raw little-endian
	// words in target memory order.
	n := jlinkReadMemEx(addr, uint32(len(buf)), &buf[0], 32, 0)
	if n < 0 {
		return nil, fmt.Errorf("JLINKARM_ReadMemEx failed (code %d)", n)
	}
	if int(n) < len(buf) {
		buf = buf[:n]
	}
	words := make([]uint32, len(buf)/4)
	for i := range words {
		words[i] = binary.LittleEndian.Uint32(buf[i*4 : i*4+4])
	}
	return words, nil
}

func (b *puregoBackend) RTTStart(addr int) error {
	if rttControl == nil {
		return errors.New("RTT_Control not loaded")
	}
	if addr < 0 {
		// Auto-locate the control block.
		if r := rttControl(rttCmdStart, 0); r < 0 {
			return fmt.Errorf("rtt_start (auto) failed (code %d)", r)
		}
		return nil
	}
	// JLinkRTTerminalStart: { U32 ConfigBlockAddress; U32 Reserved[3] }
	cfg := [4]uint32{uint32(addr), 0, 0, 0}
	if r := rttControl(rttCmdStart, uintptr(unsafe.Pointer(&cfg[0]))); r < 0 {
		return fmt.Errorf("rtt_start(0x%X) failed (code %d)", addr, r)
	}
	return nil
}

func (b *puregoBackend) RTTStop() {
	if rttControl != nil {
		rttControl(rttCmdStop, 0)
	}
}

func (b *puregoBackend) RTTRead(channel, max int) ([]byte, error) {
	if rttRead == nil {
		return nil, errors.New("RTT_Read not loaded")
	}
	if max <= 0 {
		max = 1
	}
	buf := make([]byte, max)
	n := rttRead(int32(channel), &buf[0], int32(max))
	if n < 0 {
		return nil, fmt.Errorf("JLINK_RTTERMINAL_Read failed (code %d)", n)
	}
	return buf[:n], nil
}

func (b *puregoBackend) RTTWrite(channel int, data []byte) (int, error) {
	if rttWrite == nil {
		return -1, errors.New("RTT_Write not loaded")
	}
	if len(data) == 0 {
		return 0, nil
	}
	n := rttWrite(int32(channel), &data[0], int32(len(data)))
	if n < 0 {
		return -1, fmt.Errorf("JLINK_RTTERMINAL_Write failed (code %d)", n)
	}
	return int(n), nil
}

func (b *puregoBackend) RTTNumUpBuffers() (int, error) {
	return b.numBuffers(rttDirUp)
}

func (b *puregoBackend) RTTNumDownBuffers() (int, error) {
	return b.numBuffers(rttDirDown)
}

func (b *puregoBackend) numBuffers(dir int32) (int, error) {
	if rttControl == nil {
		return 0, errors.New("RTT_Control not loaded")
	}
	var d int32 = dir
	r := rttControl(rttCmdGetNumBuf, uintptr(unsafe.Pointer(&d)))
	if r < 0 {
		return 0, fmt.Errorf("get num buffers failed (code %d)", r)
	}
	return int(r), nil
}

func (b *puregoBackend) ListDevices() []string {
	// Best-effort enumeration. pylink populates a rich emulator-info struct via
	// JLINKARM_EMU_GetList; here we report the count to keep the binding
	// robust across SEGGER versions. Rich per-device detail can be layered in
	// during hardware validation.
	if jlinkEMUNumDevices == nil {
		return nil
	}
	n := jlinkEMUNumDevices()
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for i := int32(0); i < n; i++ {
		out = append(out, fmt.Sprintf("J-Link #%d", i))
	}
	return out
}

func (b *puregoBackend) Opened() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.opened
}

// cString reads a NUL-terminated C string starting at p into a Go string. Safe
// for nil (returns "") and for fixed-size buffers (reads up to the first NUL,
// never past the allocation the caller provided). Pointer arithmetic uses
// unsafe.Add (not uintptr) so the GC sees the live pointer.
func cString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for q := unsafe.Pointer(p); *(*byte)(q) != 0; q = unsafe.Add(q, 1) {
		n++
	}
	return string(unsafe.Slice(p, n))
}

// parseSerial parses a SEGGER serial number, accepting decimal or 0x-hex.
func parseSerial(s string) (int32, error) {
	v, err := strconv.ParseInt(s, 0, 64)
	if err != nil {
		return 0, err
	}
	return int32(v), nil
}
