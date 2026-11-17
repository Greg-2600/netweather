package logging

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// Logger bundles the structured logger with the in-memory ring and the sinks it
// owns, so shutdown is a single call.
type Logger struct {
	*slog.Logger

	Ring   *Ring
	scoped *scopedHandler
	Scopes *Scopes

	cfg     Config
	console *AsyncWriter
	file    *AsyncWriter
	rot     *RotatingFile
}

// Diagnostics is the operator-facing view of the logging setup. It exists
// because the release image has no shell: level, sinks, and backpressure have
// to be readable from the UI (LOGGING.md §9).
type Diagnostics struct {
	Level        string
	Format       string
	File         string
	RingRecords  int
	RingCapacity int
	Dropped      uint64
	WriteErrors  uint64
	Scopes       int
	// Streams is how many live SSE readers the ring is feeding. A number that
	// keeps climbing without a browser open is a leak.
	Streams int
}

// Diagnostics reports the current logging setup and its backpressure counters.
func (l *Logger) Diagnostics() Diagnostics {
	if l == nil {
		return Diagnostics{}
	}
	format := l.cfg.Format
	d := Diagnostics{
		Level:  LevelName(l.cfg.Level),
		Format: format,
		File:   l.cfg.File,
		Scopes: 0,
	}
	if l.Ring != nil {
		d.RingRecords = l.Ring.Len()
		d.RingCapacity = l.Ring.Capacity()
		d.Streams = l.Ring.Subscribers()
	}
	if l.rot != nil {
		d.File = l.rot.path
	}
	if l.Scopes != nil {
		d.Scopes = len(l.Scopes.List())
	}
	d.Dropped = l.Dropped()
	d.WriteErrors = l.WriteErrors()
	return d
}

// LogRecords reads the ring with the same filters the API uses. Nil ring means
// no records, which the caller states rather than rendering an empty table.
func (l *Logger) LogRecords(minLevel slog.Level, contains string, limit int) []Record {
	if l == nil || l.Ring == nil {
		return nil
	}
	return l.Ring.Query(minLevel, contains, limit)
}

// ActiveScopes returns the time-boxed debug scopes in force. The UI lists them
// so an operator can see a scope that is still on without reading a file.
func (l *Logger) ActiveScopes() []Scope {
	if l == nil || l.Scopes == nil {
		return nil
	}
	return l.Scopes.List()
}

// Subscribe feeds the SSE log stream. With no ring there is nothing to stream,
// so the channel is closed immediately and the caller's read returns at once.
func (l *Logger) Subscribe() (<-chan Record, func()) {
	if l == nil || l.Ring == nil {
		ch := make(chan Record)
		close(ch)
		return ch, func() {}
	}
	return l.Ring.Subscribe()
}

// New builds a logger for cfg writing to stdout. Format "auto" is text on a TTY
// and JSON otherwise, because a container's stdout is read by a machine.
func New(cfg Config, stdout io.Writer) (*Logger, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	opts := handlerOptions(cfg)
	format := cfg.Format
	if format == "auto" {
		format = detectFormat(stdout)
	}

	handlers := make([]slog.Handler, 0, 3)
	out := &Logger{cfg: cfg}

	out.console = NewAsyncWriter(stdout, 4096)
	if format == "json" {
		handlers = append(handlers, slog.NewJSONHandler(out.console, opts))
	} else {
		handlers = append(handlers, slog.NewTextHandler(out.console, opts))
	}

	if cfg.File != "" {
		rot, err := NewRotatingFile(cfg.File, cfg.MaxMB, cfg.Keep)
		if err != nil {
			_ = out.console.Close()
			return nil, err
		}
		out.rot = rot
		out.file = NewAsyncWriter(rot, 4096)
		handlers = append(handlers, slog.NewJSONHandler(out.file, opts))
	}

	if cfg.RingRecords > 0 {
		out.Ring = NewRing(cfg.RingRecords, cfg.RingMaxKB)
		handlers = append(handlers, out.Ring)
	}

	// The sinks are built at TRACE and never filter; scopedHandler is the only
	// level decision. That is what lets an active scope pass debug records
	// through a logger configured at INFO.
	out.scoped = &scopedHandler{min: cfg.Level, next: multiHandler(handlers)}
	out.Logger = slog.New(out.scoped)
	return out, nil
}

// UseScopes attaches the debug-scope registry. It is called once at startup,
// before the logging goroutines begin.
func (l *Logger) UseScopes(s *Scopes) {
	if l == nil || l.scoped == nil {
		return
	}
	l.Scopes = s
	l.scoped.scopes = s
}

// Close flushes sinks in order: console, file writer, file handle.
func (l *Logger) Close() error {
	var errs []error
	if l.file != nil {
		errs = append(errs, l.file.Close())
	}
	if l.rot != nil {
		errs = append(errs, l.rot.Close())
	}
	if l.console != nil {
		errs = append(errs, l.console.Close())
	}
	return errors.Join(errs...)
}

// Dropped reports lines discarded under backpressure. It is surfaced by
// self-health: a non-zero value means the log sink is not keeping up, and that
// is itself worth reporting once.
func (l *Logger) Dropped() uint64 {
	var n uint64
	if l.console != nil {
		n += l.console.Dropped()
	}
	if l.file != nil {
		n += l.file.Dropped()
	}
	return n
}

// WriteErrors reports sink write failures.
func (l *Logger) WriteErrors() uint64 {
	var n uint64
	if l.console != nil {
		n += l.console.WriteErrors()
	}
	if l.file != nil {
		n += l.file.WriteErrors()
	}
	return n
}

func detectFormat(w io.Writer) string {
	f, ok := w.(*os.File)
	if !ok {
		return "text"
	}
	st, err := f.Stat()
	if err != nil {
		return "text"
	}
	if st.Mode()&os.ModeCharDevice != 0 {
		return "text"
	}
	return "json"
}

func handlerOptions(cfg Config) *slog.HandlerOptions {
	return &slog.HandlerOptions{
		// TRACE is the floor: scopedHandler decides what actually passes, so a
		// sink-level filter here would silently drop scoped verbose records.
		Level:     LevelTrace,
		AddSource: cfg.AddSource,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			a = RedactAttr(groups, a)
			switch a.Key {
			case slog.LevelKey:
				if lv, ok := a.Value.Any().(slog.Level); ok {
					return slog.String(slog.LevelKey, LevelName(lv))
				}
			case slog.TimeKey:
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.String(slog.TimeKey, t.UTC().Format(timeFormat))
				}
			case slog.SourceKey:
				if src, ok := a.Value.Any().(*slog.Source); ok {
					return slog.String(slog.SourceKey, filepath.Base(src.File)+":"+strconv.Itoa(src.Line))
				}
			}
			return a
		},
	}
}

// multiHandler fans a record out to every sink. A failing sink does not stop the
// others: losing the file copy must not lose the console copy.
type multiHandler []slog.Handler

func (m multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, rec slog.Record) error {
	var errs []error
	for _, h := range m {
		if !h.Enabled(ctx, rec.Level) {
			continue
		}
		if err := h.Handle(ctx, rec.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (m multiHandler) WithGroup(name string) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(name)
	}
	return out
}
