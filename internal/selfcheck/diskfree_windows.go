//go:build windows

package selfcheck

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// DiskFreeShare reports the share of the volume holding dir that is available
// to an unprivileged process. It is a separate function from the write check so
// the same measurement can back both the warning and any future gate.
func DiskFreeShare(dir string) (float64, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return 0, err
	}
	path, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return 0, err
	}
	var freeAvailable, total, freeTotal uint64
	if err := windows.GetDiskFreeSpaceEx(path, &freeAvailable, &total, &freeTotal); err != nil {
		return 0, err
	}
	if total == 0 {
		return 1, nil
	}
	return float64(freeAvailable) / float64(total), nil
}
