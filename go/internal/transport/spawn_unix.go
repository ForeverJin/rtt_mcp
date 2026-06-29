//go:build !windows

package transport

import "syscall"

// sysProcAttrDetached puts the child in its own process group so the parent's
// signals (SIGINT/SIGTERM) do not propagate to it. The daemon stays resident
// after the parent exits and can be reused by a later client; teardown is via
// POST /shutdown or SIGTERM directed at the daemon itself.
func sysProcAttrDetached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
