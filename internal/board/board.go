// Package board is the read model behind the UI: what the grid shows, and what
// changed. The store will implement Source; the in-memory implementation exists
// so the UI and its browser tests do not need a database or a network.
package board

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/Greg-2600/netweather/internal/state"
)

// State is the shared state vocabulary, re-exported so existing callers,
// templates and tests keep their names.
type State = state.State

const (
	StateUp             = state.Up
	StateUpReachability = state.UpReachability
	StateDegraded       = state.Degraded
	StateDown           = state.Down
	StateBlocked        = state.Blocked
	StateUnknown        = state.Unknown
	StateConfigError    = state.ConfigError
	StateUnmeasurable   = state.Unmeasurable
)

// Target is one tile.
type Target struct {
	ID      string
	Name    string
	Host    string
	Class   string
	Sector  string
	Country string
	State   State
	Since   time.Time
	// Last observation, for the tile's corner badge.
	Stage      string
	ErrorClass string
	HTTPStatus int
	// Metadata shown in the target drawer.
	Port          int
	Probe         string
	Tier          int
	IntervalS     int
	ASN           int
	Prefix        string
	CDN           string
	ASCountry     string
	ASName        string
	IXNames       string
	Measurability string
	// Leaf certificate from the last TLS observation.
	CertIssuer   string
	CertNotAfter time.Time
}

// Source returns the current board. It must be safe for concurrent use.
type Source interface {
	Snapshot(ctx context.Context) ([]Target, error)
}

// Watcher notifies subscribers that the board changed. The channel is a signal,
// not data: receivers take a fresh snapshot.
type Watcher interface {
	Subscribe() (<-chan struct{}, func())
}

// Memory is a Source and Watcher backed by a map. It is used by tests, by the
// login/demo path, and until the store lands.
type Memory struct {
	mu    sync.RWMutex
	order []string
	byID  map[string]Target
	subs  map[chan struct{}]struct{}
}

// NewMemory builds a board preserving the given order.
func NewMemory(targets ...Target) *Memory {
	m := &Memory{
		byID: make(map[string]Target, len(targets)),
		subs: make(map[chan struct{}]struct{}),
	}
	for _, t := range targets {
		m.order = append(m.order, t.ID)
		m.byID[t.ID] = t
	}
	return m
}

// Snapshot returns a copy in insertion order. Mutation of the result is safe.
func (m *Memory) Snapshot(context.Context) ([]Target, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Target, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.byID[id])
	}
	return out, nil
}

// Set changes one target's state and notifies watchers. It reports whether the
// target existed and whether the state actually changed.
func (m *Memory) Set(id string, state State) (found, changed bool) {
	m.mu.Lock()
	t, ok := m.byID[id]
	if !ok {
		m.mu.Unlock()
		return false, false
	}
	if t.State == state {
		m.mu.Unlock()
		return true, false
	}
	t.State = state
	t.Since = time.Now().UTC()
	m.byID[id] = t
	m.broadcastLocked()
	m.mu.Unlock()
	return true, true
}

// Subscribe returns a change signal channel and a cancel function. The channel
// carries no payload; coalesced notifications are intentional.
func (m *Memory) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()

	cancel := func() {
		m.mu.Lock()
		if _, ok := m.subs[ch]; ok {
			delete(m.subs, ch)
			close(ch)
		}
		m.mu.Unlock()
	}
	return ch, cancel
}

func (m *Memory) broadcastLocked() {
	for ch := range m.subs {
		select {
		case ch <- struct{}{}:
		default: // a pending signal already covers this change
		}
	}
}

// SortedByHost returns a snapshot ordered by host, for deterministic output.
func SortedByHost(targets []Target) []Target {
	out := append([]Target(nil), targets...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}
