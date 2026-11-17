//go:build unix

package startup

import "os"

// EUID returns the effective uid, or -1 where the concept does not apply.
func EUID() int { return os.Geteuid() }
