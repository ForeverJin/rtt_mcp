package jlink

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/ebitengine/purego"
)

// SEGGER J-Link interface constants (from pylink.enums).
const (
	jlinkTifSWD = 1 // JLinkInterfaces.SWD
)

// JLINK_RTTERMINAL_Control commands (pylink.enums.JLinkRTTCommand).
const (
	rttCmdStart     = 0
	rttCmdStop      = 1
	rttCmdGetNumBuf = 3
)

// RTT directions for the GETNUMBUF command (pylink.enums.JLinkRTTDirection).
const (
	rttDirUp   = 0
	rttDirDown = 1
)

// DLL function variables. Each is registered against a SEGGER symbol via
// purego.RegisterLibFunc once the library is loaded. The Go signatures mirror
// the C prototypes (C int → int32, U32 → uint32, pointers → *byte / uintptr).
var (
	jlinkSelectByUSBSN func(serial int32) int32
	jlinkSelectUSB     func(usb int32) int32
	jlinkOpenEx        func(errHandler, logHandler uintptr) *byte
	jlinkClose         func()
	jlinkTIFSelect     func(iface int32) int32
	jlinkSetSpeed      func(speed uint32)
	jlinkConnect       func() int32
	jlinkExecCommand   func(cmd *byte) int32
	jlinkReadMemEx     func(addr uint32, numBytes uint32, buf *byte, accessWidth uint32, handle uintptr) int32
	jlinkCore2CoreName func(cpu int32, buf *byte, bufSize int32)
	jlinkEMUProdName   func(buf *byte, bufSize int32)
	jlinkEMUNumDevices func() int32

	rttControl func(cmd int32, p uintptr) int32
	rttRead    func(bufIdx int32, buf *byte, n int32) int32
	rttWrite   func(bufIdx int32, buf *byte, n int32) int32
)

var (
	libOnce      sync.Once
	libHandle    uintptr
	libLoadErr   error
	missingSyms  []string // symbols absent from this build of the SEGGER lib (informational)
)

// Load opens (or returns the already-open) SEGGER library handle. The explicit
// path overrides auto-detection. Returns the load error if the library cannot
// be opened; missing individual symbols are tolerated and reported separately.
func Load(explicit string) error {
	libOnce.Do(func() {
		libHandle, libLoadErr = loadJLinkLib(explicit)
		if libLoadErr != nil {
			return
		}
		registerSymbols()
	})
	return libLoadErr
}

// registerSymbols binds each SEGGER export. RegisterLibFunc panics on a missing
// symbol, so each is attempted independently; essential symbols that fail to
// register are recorded and surfaced when the relevant operation is used.
func registerSymbols() {
	type entry struct {
		fptr any
		name string
		ess  bool // essential: hard error if absent
	}
	entries := []entry{
		{&jlinkSelectByUSBSN, "JLINKARM_EMU_SelectByUSBSN", false},
		{&jlinkSelectUSB, "JLINKARM_SelectUSB", false},
		{&jlinkOpenEx, "JLINKARM_OpenEx", true},
		{&jlinkClose, "JLINKARM_Close", true},
		{&jlinkTIFSelect, "JLINKARM_TIF_Select", true},
		{&jlinkSetSpeed, "JLINKARM_SetSpeed", true},
		{&jlinkConnect, "JLINKARM_Connect", true},
		{&jlinkExecCommand, "JLINKARM_ExecCommand", true},
		{&jlinkReadMemEx, "JLINKARM_ReadMemEx", true},
		{&jlinkCore2CoreName, "JLINKARM_Core2CoreName", false},
		{&jlinkEMUProdName, "JLINKARM_EMU_GetProductName", false},
		{&jlinkEMUNumDevices, "JLINKARM_EMU_GetNumDevices", false},
		{&rttControl, "JLINK_RTTERMINAL_Control", true},
		{&rttRead, "JLINK_RTTERMINAL_Read", true},
		{&rttWrite, "JLINK_RTTERMINAL_Write", true},
	}
	for _, e := range entries {
		if !tryRegister(e.fptr, e.name) {
			if e.ess {
				missingSyms = append(missingSyms, e.name+" (essential)")
			} else {
				missingSyms = append(missingSyms, e.name)
			}
		}
	}
}

func tryRegister(fptr any, name string) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[jlink] RegisterLibFunc(%s) panic: %v\n", name, r)
			ok = false
		}
	}()
	purego.RegisterLibFunc(fptr, libHandle, name)
	return true
}

// essentialMissing reports whether any essential SEGGER symbol failed to load.
func essentialMissing() error {
	var essential []string
	for _, m := range missingSyms {
		if strings.Contains(m, "essential") {
			essential = append(essential, m)
		}
	}
	if len(essential) > 0 {
		return fmt.Errorf("SEGGER library loaded but missing essential symbol(s): %s",
			strings.Join(essential, ", "))
	}
	return nil
}
