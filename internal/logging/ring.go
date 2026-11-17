package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Record is one retained log line, flattened for display and filtering. It is
// deliberately a value type: callers cannot mutate the ring's contents.
type Record struct {
	Time      time.Time
	Level     slog.Level
	LevelName string
	Message   string
	Attrs     map[string]string
}

// Ring is an slog.Handler that keeps the last N records in memory. It exists
// because the release image has no shell: without it, the only way to see logs
// is `docker logs` on the host. The UI and /api/v1/logs read from here.
type Ring struct {
	state  *ringState
	base   []qualifiedAttr
	groups []string
}

// qualifiedAttr remembers the group path in force when the attribute was added,
// so WithAttrs before WithGroup does not retroactively nest it (slog semantics).
type qualifiedAttr struct {
	groups []string
	attr   slog.Attr
}

type ringState struct {
	mu         sync.Mutex
	buf        []Record
	start      int // index of the oldest record once the buffer is full
	n          int
	bytes      int
	maxRecords int
	maxBytes   int
}

// NewRing creates a ring bounded by both record count and approximate bytes.
// maxBytes <= 0 means "records only".
func NewRing(maxRecords, maxKB int) *Ring {
	if maxRecords <= 0 {
		maxRecords = 1
	}
	maxBytes := 0
	if maxKB > 0 {
		maxBytes = maxKB * 1024
	}
	return &Ring{state: &ringState{
		buf:        make([]Record, 0, min(maxRecords, 256)),
		maxRecords: maxRecords,
		maxBytes:   maxBytes,
	}}
}

// Enabled accepts everything; the logger's own level already filtered.
func (r *Ring) Enabled(context.Context, slog.Level) bool { return true }

// Handle flattens the record into the ring, applying the same redaction as the
// other sinks.
func (r *Ring) Handle(_ context.Context, rec slog.Record) error {
	attrs := make(map[string]string, len(r.base)+4)
	for _, q := range r.base {
		addFlat(attrs, q.groups, q.attr)
	}
	rec.Attrs(func(a slog.Attr) bool {
		addFlat(attrs, r.groups, a)
		return true
	})

	t := rec.Time
	if t.IsZero() {
		t = time.Now()
	}

	r.push(Record{
		Time:      t.UTC(),
		Level:     rec.Level,
		LevelName: LevelName(rec.Level),
		Message:   rec.Message,
		Attrs:     attrs,
	})
	return nil
}

// WithAttrs returns a derived handler sharing the same storage.
func (r *Ring) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return r
	}
	base := make([]qualifiedAttr, 0, len(r.base)+len(attrs))
	base = append(base, r.base...)
	groups := append([]string(nil), r.groups...)
	for _, a := range attrs {
		base = append(base, qualifiedAttr{groups: groups, attr: a})
	}
	return &Ring{state: r.state, base: base, groups: r.groups}
}

// WithGroup returns a derived handler sharing the same storage.
func (r *Ring) WithGroup(name string) slog.Handler {
	if name == "" {
		return r
	}
	groups := make([]string, 0, len(r.groups)+1)
	groups = append(groups, r.groups...)
	groups = append(groups, name)
	return &Ring{state: r.state, base: r.base, groups: groups}
}

func (r *Ring) push(rec Record) {
	size := recordBytes(rec)
	st := r.state

	st.mu.Lock()
	defer st.mu.Unlock()

	// A single record larger than the whole ring would evict everything and
	// still not fit; drop it rather than emptying the buffer.
	if st.maxBytes > 0 && size > st.maxBytes {
		return
	}

	for st.n > 0 && (st.n >= st.maxRecords || (st.maxBytes > 0 && st.bytes+size > st.maxBytes)) {
		st.evictOldest()
	}

	if st.n == len(st.buf) {
		st.grow()
	}
	idx := (st.start + st.n) % len(st.buf)
	st.buf[idx] = rec
	st.n++
	st.bytes += size
}

func (st *ringState) grow() {
	newCap := len(st.buf) * 2
	if newCap < 8 {
		newCap = 8
	}
	if newCap > st.maxRecords {
		newCap = st.maxRecords
	}
	if newCap <= len(st.buf) {
		return
	}
	next := make([]Record, newCap)
	for i := 0; i < st.n; i++ {
		next[i] = st.buf[(st.start+i)%len(st.buf)]
	}
	st.buf = next
	st.start = 0
}

func (st *ringState) evictOldest() {
	old := st.buf[st.start]
	st.bytes -= recordBytes(old)
	st.buf[st.start] = Record{}
	st.start = (st.start + 1) % len(st.buf)
	st.n--
}

// Snapshot returns a copy of every retained record, oldest first.
func (r *Ring) Snapshot() []Record { return r.Query(LevelTrace, "", 0) }

// Query filters by minimum level and a case-insensitive substring over the
// message and attribute values. limit <= 0 means "all". Results stay oldest-first.
func (r *Ring) Query(minLevel slog.Level, contains string, limit int) []Record {
	st := r.state
	st.mu.Lock()
	defer st.mu.Unlock()

	needle := strings.ToLower(contains)
	out := make([]Record, 0, st.n)
	for i := 0; i < st.n; i++ {
		rec := st.buf[(st.start+i)%len(st.buf)]
		if rec.Level < minLevel {
			continue
		}
		if needle != "" && !recordContains(rec, needle) {
			continue
		}
		out = append(out, rec)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Len reports how many records are retained.
func (r *Ring) Len() int {
	st := r.state
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.n
}

// Capacity reports the configured record limit.
func (r *Ring) Capacity() int { return r.state.maxRecords }

func recordContains(rec Record, needle string) bool {
	if strings.Contains(strings.ToLower(rec.Message), needle) {
		return true
	}
	for k, v := range rec.Attrs {
		if strings.Contains(strings.ToLower(k), needle) || strings.Contains(strings.ToLower(v), needle) {
			return true
		}
	}
	return false
}

func recordBytes(rec Record) int {
	n := len(rec.Message) + 24
	for k, v := range rec.Attrs {
		n += len(k) + len(v) + 4
	}
	return n
}

func addFlat(dst map[string]string, groups []string, a slog.Attr) {
	a = RedactAttr(groups, a)
	v := a.Value.Resolve()

	if v.Kind() == slog.KindGroup {
		next := groups
		if a.Key != "" {
			next = append(append([]string(nil), groups...), a.Key)
		}
		for _, ga := range v.Group() {
			addFlat(dst, next, ga)
		}
		return
	}

	key := a.Key
	if key == "" {
		return
	}
	if len(groups) > 0 {
		key = strings.Join(groups, ".") + "." + key
	}
	dst[key] = valueString(v)
}

func valueString(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(v.Any())
	}
}
