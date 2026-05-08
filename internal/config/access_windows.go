//go:build windows

package config

import (
	"errors"
	"os"
	"syscall"
)

func init() {
	checkWriteAccess = func(path string) error {
		_, err := os.Stat(path)
		return err
	}
	isNotDirError = func(err error) bool {
		return errors.Is(err, syscall.ENOTDIR)
	}
}
