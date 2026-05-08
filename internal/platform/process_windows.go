//go:build windows

package platform

import (
	"syscall"

	"golang.org/x/sys/windows"
)

const haveProcessGroups = false

func ProcAttr() *syscall.SysProcAttr {
	return nil
}

func DaemonProcAttr() *syscall.SysProcAttr {
	return nil
}

func KillProcessGroup(pid int) error {
	return killProcess(pid)
}

func TerminateProcessGroup(pid int) error {
	return terminateProcess(pid)
}

func SignalProcessGroup(pid int, signal syscall.Signal) error {
	return signalProcess(pid, signal)
}

func KillProcess(pid int) error {
	return killProcess(pid)
}

func TerminateProcess(pid int) error {
	return terminateProcess(pid)
}

func SignalProcess(pid int, signal syscall.Signal) error {
	return signalProcess(pid, signal)
}

func killProcess(pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.TerminateProcess(handle, 1)
}

func terminateProcess(pid int) error {
	return killProcess(pid)
}

func signalProcess(pid int, signal syscall.Signal) error {
	return killProcess(pid)
}
