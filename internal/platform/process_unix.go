//go:build !windows

package platform

import (
	"syscall"
)

const haveProcessGroups = true

func ProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func DaemonProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func KillProcessGroup(pid int) error {
	return syscall.Kill(-pid, SIGKILL)
}

func TerminateProcessGroup(pid int) error {
	return syscall.Kill(-pid, SIGTERM)
}

func SignalProcessGroup(pid int, signal syscall.Signal) error {
	return syscall.Kill(-pid, signal)
}

func KillProcess(pid int) error {
	return syscall.Kill(pid, SIGKILL)
}

func TerminateProcess(pid int) error {
	return syscall.Kill(pid, SIGTERM)
}

func SignalProcess(pid int, signal syscall.Signal) error {
	return syscall.Kill(pid, signal)
}
