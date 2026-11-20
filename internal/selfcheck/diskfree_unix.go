//go:build !windows

package selfcheck

import "syscall"

// DiskFreeShare reports the share of the volume holding dir that is available
// to an unprivileged process. It is a separate function from the write check so
// the same measurement can back both the warning and any future gate.
func DiskFreeShare(dir string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	// statfs reports non-negative block counts on the supported filesystems.
	total := uint64(st.Blocks) * uint64(st.Bsize) // #nosec G115 -- statfs blocks are non-negative here.
	if total == 0 {
		return 1, nil
	}
	available := uint64(st.Bavail) * uint64(st.Bsize) // #nosec G115 -- statfs avail is non-negative here.
	return float64(available) / float64(total), nil
}
