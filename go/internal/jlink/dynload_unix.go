//go:build !windows

package jlink

import "github.com/ebitengine/purego"

// openHandle loads a shared object on Unix via purego.Dlopen. Returns the
// handle on success.
func openHandle(path string) (uintptr, bool) {
	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return 0, false
	}
	return handle, true
}
