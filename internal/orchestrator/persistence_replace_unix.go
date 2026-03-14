//go:build !windows

package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func replaceDurableStateFileAtomically(tmpPath string, path string) error {
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return syncDurableStateParentDirectory(path)
}

func syncDurableStateParentDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()

	if err := dir.Sync(); err != nil &&
		!errors.Is(err, syscall.EINVAL) &&
		!errors.Is(err, syscall.ENOTSUP) &&
		!errors.Is(err, syscall.EBADF) {
		return err
	}
	return nil
}
