// Package jlink provides access to a SEGGER J-Link probe and its RTT
// capability, loaded dynamically (no cgo) from JLinkARM.dll / libjlinkarm.so /
// libjlinkarm.dylib via purego.
//
// RTTBackend is the abstraction the rest of the server programs against; the
// concrete puregoBackend talks to the SEGGER shared library. A mock backend
// (mock.go) lets the non-hardware parts of the system be exercised without a
// probe.
package jlink

// RTTBackend abstracts the subset of the SEGGER J-Link / RTT API the server
// needs. Every method maps to a pylink method used by the Python server's
// jlink_rtt.py, so behaviour stays 1:1.
type RTTBackend interface {
	// Open acquires the probe. serial == "" selects the first available.
	// Returns an error string describing the failure (Python surfaces a full
	// traceback; here we return the SEGGER error).
	Open(serial string) error

	// Close releases the probe. Idempotent.
	Close()

	// SetTifSWD forces the SWD interface.
	SetTifSWD()

	// SetSpeed configures the SWD/JTAG speed in kHz.
	SetSpeed(kHz int)

	// ConnectDevice selects the target device by name and connects. The
	// underlying mechanism is JLINKARM_ExecCommand("Device = <name>") followed
	// by JLINKARM_Connect().
	ConnectDevice(device string) error

	// CoreName returns the connected core's name (best-effort, may be "").
	CoreName() string

	// ProductName returns the emulator product name (best-effort, may be "").
	ProductName() string

	// MemoryRead32 reads count 32-bit words starting at addr (used to scan RAM
	// for the RTT control block).
	MemoryRead32(addr uint32, count int) ([]uint32, error)

	// RTTStart starts RTT. addr < 0 lets SEGGER auto-locate the control block;
	// addr >= 0 pins it.
	RTTStart(addr int) error

	// RTTStop stops RTT.
	RTTStop()

	// RTTRead reads up to max bytes from the given channel's up-buffer.
	RTTRead(channel, max int) ([]byte, error)

	// RTTWrite writes data to the given channel's down-buffer; returns bytes
	// written.
	RTTWrite(channel int, data []byte) (int, error)

	// RTTNumUpBuffers / RTTNumDownBuffers report configured buffer counts.
	RTTNumUpBuffers() (int, error)
	RTTNumDownBuffers() (int, error)

	// ListDevices enumerates connected probes (best-effort; may be empty).
	ListDevices() []string

	// SupportedDeviceCount returns the number of devices in the loaded J-Link
	// device database. Works without a connected probe (just needs the DLL).
	SupportedDeviceCount() int

	// SupportedDeviceName returns the name of device at the given index, or ""
	// if the index is out of range / unsupported by this DLL build.
	SupportedDeviceName(index int) string

	// SupportedDeviceIndex returns the database index of the named device, or
	// a value <= 0 if it is not supported (mirrors pylink get_device_index).
	SupportedDeviceIndex(name string) int

	// Opened reports whether a probe has been successfully opened.
	Opened() bool
}
