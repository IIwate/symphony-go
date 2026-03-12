//go:build !windows

package envfile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func replaceFileAtomically(tmpPath string, path string) error {
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return syncParentDirectory(path)
}

func syncParentDirectory(path string) error {
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
