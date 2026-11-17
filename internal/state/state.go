// Package state is the shared per-target state vocabulary. It exists separately
// from board so the classifier does not have to import the UI model.
package state

// State is the per-target state from DESIGN.md §3.2, lower-cased for CSS and
// wire use.
type State string

const (
	Up             State = "up"
	UpReachability State = "up_reachability"
	Degraded       State = "degraded"
	Down           State = "down"
	Blocked        State = "blocked"
	Unknown        State = "unknown"
	ConfigError    State = "config_error"
	Unmeasurable   State = "unmeasurable"
)

// Text is the human label. It is also the aria-label suffix, so colour is never
// the only carrier of meaning.
func (s State) Text() string {
	switch s {
	case Up:
		return "Up"
	case UpReachability:
		return "Up (reachability only)"
	case Degraded:
		return "Degraded"
	case Down:
		return "Down"
	case Blocked:
		return "Blocked"
	case ConfigError:
		return "Config error"
	case Unmeasurable:
		return "Unmeasurable"
	default:
		return "Unknown"
	}
}

// Glyph is the non-colour marker required by DESIGN.md §10.7.
func (s State) Glyph() string {
	switch s {
	case Up:
		return "●"
	case UpReachability:
		return "◍"
	case Degraded:
		return "◐"
	case Down:
		return "✕"
	case Blocked:
		return "▨"
	case ConfigError:
		return "!"
	case Unmeasurable:
		return "–"
	default:
		return "?"
	}
}

// CSSClass is the class suffix used by the stylesheet.
func (s State) CSSClass() string { return "state-" + string(s) }
