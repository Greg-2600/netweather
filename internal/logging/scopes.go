package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Scope is one time-boxed verbose session. Empty selector fields are wildcards.
type Scope struct {
	ID         string    `json:"id"`
	Target     string    `json:"target,omitempty"`
	Vantage    string    `json:"vantage,omitempty"`
	Component  string    `json:"component,omitempty"`
	ErrorClass string    `json:"error_class,omitempty"`
	Level      string    `json:"level"` // "debug" or "trace"
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// MaxScopeTTL is the hard ceiling. There is no "for infinity": a debug session
// left on is how logging becomes a disk-filling incident.
const MaxScopeTTL = time.Hour

// Selectors returns true when the scope names at least one selector, which is
// what stops "trace everything" from being expressible.
func (s Scope) Selectors() bool {
	return s.Target != "" || s.Vantage != "" || s.Component != "" || s.ErrorClass != ""
}

// level maps the scope's level name to a slog level.
func (s Scope) level() (slog.Level, error) {
	switch s.Level {
	case "debug":
		return slog.LevelDebug, nil
	case "trace":
		return LevelTrace, nil
	default:
		return 0, fmt.Errorf("level must be debug or trace, not %q", s.Level)
	}
}

// Scopes is the registry of active debug sessions. It is safe for concurrent
// use and persists to a file so a restart does not silently keep one enabled.
type Scopes struct {
	mu      sync.Mutex
	path    string
	now     func() time.Time
	active  map[string]Scope
	loaded  bool
	changed bool
}

// NewScopes returns an empty registry. path may be empty to disable persistence.
func NewScopes(path string) *Scopes {
	return &Scopes{
		path:   path,
		now:    time.Now,
		active: make(map[string]Scope),
	}
}

// SetClock replaces the clock. It exists for tests and never runs in production.
func (s *Scopes) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Load reads persisted scopes, dropping any that expired while the process was
// down. A missing file is not an error. Returned scopes are the expired ones, so
// the caller can log them.
func (s *Scopes) Load() ([]Scope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.loaded = true
	if s.path == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(s.path) // #nosec G304 -- operator-configured data directory.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("logging: debug scopes: %w", err)
	}

	var stored []Scope
	if err := json.Unmarshal(raw, &stored); err != nil {
		// A corrupt debug file must not stop monitoring; start clean instead.
		return nil, fmt.Errorf("logging: parse debug scopes: %w", err)
	}

	var expired []Scope
	now := s.now()
	for _, sc := range stored {
		if !now.Before(sc.ExpiresAt) {
			expired = append(expired, sc)
			continue
		}
		s.active[sc.ID] = sc
	}
	if len(expired) > 0 {
		s.changed = true
		_ = s.saveLocked()
	}
	return expired, nil
}

// Add validates and records a scope. The TTL is mandatory and capped.
func (s *Scopes) Add(sc Scope, ttl time.Duration) (Scope, error) {
	if !sc.Selectors() {
		return Scope{}, errors.New("a scope needs at least one of target, vantage, component, or error_class")
	}
	if _, err := sc.level(); err != nil {
		return Scope{}, err
	}
	if ttl <= 0 {
		return Scope{}, errors.New("ttl is required")
	}
	if ttl > MaxScopeTTL {
		return Scope{}, fmt.Errorf("ttl must be %s or less", MaxScopeTTL)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	sc.ID = newScopeID()
	sc.CreatedAt = now.UTC()
	sc.ExpiresAt = now.Add(ttl).UTC()
	s.active[sc.ID] = sc
	s.changed = true
	if err := s.saveLocked(); err != nil {
		delete(s.active, sc.ID)
		return Scope{}, err
	}
	return sc, nil
}

// Remove ends a scope early. Unknown ids are false, not an error.
func (s *Scopes) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.active[id]; !ok {
		return false
	}
	delete(s.active, id)
	s.changed = true
	_ = s.saveLocked()
	return true
}

// Clear ends every scope.
func (s *Scopes) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.active = make(map[string]Scope)
	s.changed = true
	_ = s.saveLocked()
}

// List returns the unexpired scopes, newest first.
func (s *Scopes) List() []Scope {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked()
	out := make([]Scope, 0, len(s.active))
	for _, sc := range s.active {
		out = append(out, sc)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Prune drops expired scopes and returns them so the caller can log the expiry.
func (s *Scopes) Prune() []Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked()
}

func (s *Scopes) pruneLocked() []Scope {
	now := s.now()
	var expired []Scope
	for id, sc := range s.active {
		if now.Before(sc.ExpiresAt) {
			continue
		}
		expired = append(expired, sc)
		delete(s.active, id)
	}
	if len(expired) > 0 {
		s.changed = true
		_ = s.saveLocked()
	}
	return expired
}

// Active reports whether any scope is currently enabled. The handler calls it
// from Enabled, so it must stay cheap.
func (s *Scopes) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked()
	return len(s.active) > 0
}

// Match reports whether rec belongs to an active scope at its level.
func (s *Scopes) Match(rec slog.Record, attrs []slog.Attr) bool {
	s.mu.Lock()
	scopes := make([]Scope, 0, len(s.active))
	now := s.now()
	for _, sc := range s.active {
		if !now.Before(sc.ExpiresAt) {
			continue
		}
		scopes = append(scopes, sc)
	}
	s.mu.Unlock()

	if len(scopes) == 0 {
		return false
	}

	values := make(map[string]string, len(attrs)+rec.NumAttrs())
	for _, a := range attrs {
		values[a.Key] = a.Value.String()
	}
	rec.Attrs(func(a slog.Attr) bool {
		values[a.Key] = a.Value.String()
		return true
	})

	for _, sc := range scopes {
		lvl, err := sc.level()
		if err != nil || rec.Level < lvl {
			continue
		}
		if sc.Target != "" && values["target"] != sc.Target {
			continue
		}
		if sc.Vantage != "" && values["vantage"] != sc.Vantage {
			continue
		}
		if sc.Component != "" && values["component"] != sc.Component {
			continue
		}
		if sc.ErrorClass != "" && values["error_class"] != sc.ErrorClass {
			continue
		}
		return true
	}
	return false
}

// saveLocked writes the registry atomically. The caller holds the mutex.
func (s *Scopes) saveLocked() error {
	if s.path == "" || !s.changed {
		return nil
	}

	out := make([]Scope, 0, len(s.active))
	for _, sc := range s.active {
		out = append(out, sc)
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("logging: encode debug scopes: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("logging: write debug scopes: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("logging: replace debug scopes: %w", err)
	}
	s.changed = false
	return nil
}

func newScopeID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("dbg-%d", time.Now().UnixNano())
	}
	return "dbg-" + hex.EncodeToString(b[:])
}

// scopedHandler enables records below the configured level when they match an
// active scope. The sink handlers are built at TRACE so they never re-filter;
// this wrapper is the only level decision.
type scopedHandler struct {
	min    slog.Level
	next   slog.Handler
	scopes *Scopes
	attrs  []slog.Attr
}

func (h *scopedHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= h.min {
		return true
	}
	return h.scopes != nil && h.scopes.Active()
}

func (h *scopedHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level < h.min {
		if h.scopes == nil || !h.scopes.Match(rec, h.attrs) {
			return nil
		}
	}
	return h.next.Handle(ctx, rec)
}

func (h *scopedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &scopedHandler{min: h.min, next: h.next.WithAttrs(attrs), scopes: h.scopes, attrs: merged}
}

func (h *scopedHandler) WithGroup(name string) slog.Handler {
	return &scopedHandler{min: h.min, next: h.next.WithGroup(name), scopes: h.scopes, attrs: h.attrs}
}
