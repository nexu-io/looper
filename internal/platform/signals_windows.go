//go:build windows

package platform

import "syscall"

const (
	SIGTERM = syscall.Signal(0x15)
	SIGKILL = syscall.Signal(0x14)
	ESRCH   = syscall.Errno(syscall.ERROR_PROC_NOT_FOUND)
)
