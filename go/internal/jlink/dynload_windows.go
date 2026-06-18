//go:build windows

package jlink

import "syscall"

// openHandle loads a DLL on Windows via syscall.LoadLibrary. The returned
// syscall.Handle is an HMODULE, which purego's loadSymbol (Windows path) accepts
// for GetProcAddress resolution — identical in purpose to a dlopen handle.
func openHandle(path string) (uintptr, bool) {
	handle, err := syscall.LoadLibrary(path)
	if err != nil {
		return 0, false
	}
	return uintptr(handle), true
}
