// Package startup holds the invariants that must hold before netweather serves
// anything. Each one is deliberately a small, testable function rather than a
// side effect buried in main.
package startup

import "errors"

// ErrRunningAsRoot is returned when the process is running with euid 0.
//
// The release image runs as uid 65532 and refuses to start as root: the whole
// hardening story (read-only rootfs, cap_drop ALL, no-new-privileges) assumes an
// unprivileged process, and a root process silently invalidates it.
var ErrRunningAsRoot = errors.New("refusing to run as root: run as an unprivileged user (the release image uses uid 65532)")

// RefuseIfRoot reports ErrRunningAsRoot when euid is 0. The euid is passed in so
// the rule is testable on every platform, including those without a uid concept.
func RefuseIfRoot(euid int) error {
	if euid == 0 {
		return ErrRunningAsRoot
	}
	return nil
}
