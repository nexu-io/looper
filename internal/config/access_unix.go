//go:build unix

package config

import (
	"errors"
	"syscall"
)

func init() {
	checkWriteAccess = func(path string) error {
		return syscall.Access(path, 0x2)
	}
	isNotDirError = func(err error) bool {
		return errors.Is(err, syscall.ENOTDIR)
	}
}
