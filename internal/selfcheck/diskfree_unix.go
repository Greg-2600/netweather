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
	total := uint64(st.Blocks) * uint64(st.Bsize)
	if total == 0 {
		return 1, nil
	}
	return float64(uint64(st.Bavail)*uint64(st.Bsize)) / float64(total), nil
}
