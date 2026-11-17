package config

import (
	"sync"
	"time"
)

// ReloadState records the outcome of the most recent config reload. It exists so
// a rejected edit is visible in the UI, not only in the log: THREAT_MODEL.md
// T36 promises that a typo neither stops the monitor nor disappears silently.
//
// The zero value is usable and reports "no failure yet", which is what a
// process that has never reloaded should show.
type ReloadState struct {
	mu        sync.RWMutex
	failedAt  time.Time
	lastError string
}

// Failed records a rejected reload. The error text is kept verbatim because the
// operator needs it to fix the file; it is rendered only behind auth.
func (s *ReloadState) Failed(err error, at time.Time) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failedAt = at
	s.lastError = err.Error()
}

// Succeeded clears the failure. A later good edit means the board no longer
// needs to warn about the earlier one.
func (s *ReloadState) Succeeded() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failedAt = time.Time{}
	s.lastError = ""
}

// LastReloadFailure returns the newest rejected reload, or false when the last
// reload succeeded (or none has run).
func (s *ReloadState) LastReloadFailure() (at time.Time, detail string, failed bool) {
	if s == nil {
		return time.Time{}, "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.failedAt.IsZero() || s.lastError == "" {
		return time.Time{}, "", false
	}
	return s.failedAt, s.lastError, true
}
