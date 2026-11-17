package web

import (
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Deps is what the UI needs. Source is required; Watcher is optional, and
// without it the page simply does not receive live deltas.
type Deps struct {
	Logger        *slog.Logger
	Source        board.Source
	Watcher       board.Watcher
	Token         string
	SecureCookies bool
	Version       string
	StartedAt     time.Time
}

func loadTemplates() (*template.Template, error) {
	return template.ParseFS(templateFS, "templates/*.html")
}

var errNoSource = errors.New("web: Source is required")
