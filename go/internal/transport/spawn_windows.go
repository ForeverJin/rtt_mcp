//go:build windows

package transport

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// sysProcAttrDetached detaches the child so it runs as a resident, independent
// daemon: no console at all (DETACHED_PROCESS), no window (CREATE_NO_WINDOW),
// and insulated from the parent's Ctrl+C (CREATE_NEW_PROCESS_GROUP). The parent
// serve process exiting — including a Ctrl+C from Claude Code — does not take
// the daemon down, so a later extension or serve can reuse the same owner.
//
// DETACHED_PROCESS is not exported by the standard syscall package, so the
// constants come from golang.org/x/sys/windows.
func sysProcAttrDetached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP |
			windows.DETACHED_PROCESS | windows.CREATE_NO_WINDOW,
	}
}
