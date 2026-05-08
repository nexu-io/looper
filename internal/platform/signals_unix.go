//go:build !windows

package platform

import "syscall"

const (
	SIGTERM = syscall.SIGTERM
	SIGKILL = syscall.SIGKILL
	ESRCH   = syscall.ESRCH
)
