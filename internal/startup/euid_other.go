//go:build !unix

package startup

// EUID returns -1 on platforms without a POSIX uid, which makes RefuseIfRoot a
// no-op there rather than a false refusal.
func EUID() int { return -1 }
