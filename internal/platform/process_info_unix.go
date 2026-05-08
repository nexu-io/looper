//go:build linux || darwin

package platform

import (
	"bytes"
	"fmt"
	"os"
)

func ProcessCommand(pid int) (string, error) {
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	return string(bytes.ReplaceAll(cmdline, []byte{0}, []byte(" "))), nil
}
