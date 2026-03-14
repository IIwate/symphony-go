//go:build windows

package orchestrator

import (
	"time"

	"golang.org/x/sys/windows"
)

func replaceDurableStateFileAtomically(tmpPath string, path string) error {
	from, err := windows.UTF16PtrFromString(tmpPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		lastErr = windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
		if lastErr == nil {
			return nil
		}
		if lastErr != windows.ERROR_ACCESS_DENIED && lastErr != windows.ERROR_SHARING_VIOLATION {
			return lastErr
		}
		time.Sleep(50 * time.Millisecond)
	}
	return lastErr
}
