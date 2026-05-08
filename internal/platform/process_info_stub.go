//go:build !linux && !darwin && !windows

package platform

import "fmt"

func ProcessCommand(pid int) (string, error) {
	return "", fmt.Errorf("process command lookup not supported on this platform")
}
