package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// RotatingFile is a size-bounded, single-file log sink with N rotated backups.
//
// The log file is as sensitive as the database: it contains the target list and
// the egress identity. It is created 0600 in a 0700 directory.
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

// NewRotatingFile opens (creating if needed) path. keep is the number of rotated
// files retained in addition to the active one.
func NewRotatingFile(path string, maxMB, keep int) (*RotatingFile, error) {
	if keep < 1 {
		keep = 1
	}
	maxBytes := int64(maxMB) * 1024 * 1024
	if maxBytes <= 0 {
		maxBytes = 50 << 20
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create log directory: %w", err)
		}
	}
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat log file: %w", err)
	}
	return &RotatingFile{path: path, maxBytes: maxBytes, keep: keep, f: f, size: st.Size()}, nil
}

func openAppend(path string) (*os.File, error) {
	// #nosec G304 -- the path is operator-supplied configuration, not input.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return f, nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotateLocked(); err != nil {
			// Keep logging to the current file rather than losing the line.
			// Rotation failure is surfaced by the caller's error counter.
			_ = err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) rotateLocked() error {
	if err := r.f.Close(); err != nil {
		return err
	}

	// Drop the oldest, then shift each backup up by one.
	if err := os.Remove(backupName(r.path, r.keep)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := r.keep - 1; i >= 1; i-- {
		if err := os.Rename(backupName(r.path, i), backupName(r.path, i+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(r.path, backupName(r.path, 1)); err != nil && !os.IsNotExist(err) {
		return err
	}

	f, err := openAppend(r.path)
	if err != nil {
		return err
	}
	r.f = f
	r.size = 0
	return nil
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

func backupName(path string, i int) string {
	return path + "." + strconv.Itoa(i)
}
