package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/ihi"
	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/streamlimit"
	"github.com/Greg-2600/netweather/internal/verdict"
)

type server struct {
	deps    Deps
	auth    *authenticator
	tmpl    *template.Template
	now     func() time.Time
	streams *streamlimit.Limiter
}

// New builds the browser-facing handler: login, board, target detail, and the
// SSE stream. The API is mounted separately; the two share the read model.
func New(deps Deps) (http.Handler, error) {
	if deps.Source == nil {
		return nil, errNoSource
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	tmpl, err := loadTemplates()
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}

	s := &server{
		deps:    deps,
		auth:    newAuthenticator(deps.Token, deps.SecureCookies),
		tmpl:    tmpl,
		now:     time.Now,
		streams: deps.StreamLimit,
	}
	if s.streams == nil {
		s.streams = streamlimit.New(streamlimit.DefaultTotal, streamlimit.DefaultPerIP)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "User-agent: *\nDisallow: /\n")
	})
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.requireSession(http.HandlerFunc(s.handleLogout)).ServeHTTP)
	mux.Handle("/static/", http.FileServer(http.FS(staticFS)))
	mux.Handle("/events", s.requireSession(http.HandlerFunc(s.handleEvents)))
	mux.HandleFunc("/targets/", s.requireSession(http.HandlerFunc(s.handleTarget)).ServeHTTP)
	mux.HandleFunc("/incidents", s.requireSession(http.HandlerFunc(s.handleIncidents)).ServeHTTP)
	mux.HandleFunc("/incidents/", s.requireSession(http.HandlerFunc(s.handleIncident)).ServeHTTP)
	mux.HandleFunc("/coverage", s.requireSession(http.HandlerFunc(s.handleCoverage)).ServeHTTP)
	mux.HandleFunc("/world", s.requireSession(http.HandlerFunc(s.handleWorld)).ServeHTTP)
	mux.HandleFunc("/corridors", s.requireSession(http.HandlerFunc(s.handleCorridors)).ServeHTTP)
	mux.HandleFunc("/ihi", s.requireSession(http.HandlerFunc(s.handleIHI)).ServeHTTP)
	mux.HandleFunc("/diagnostics", s.requireSession(http.HandlerFunc(s.handleDiagnostics)).ServeHTTP)
	mux.HandleFunc("/icon/", s.requireSession(http.HandlerFunc(s.handleIcon)).ServeHTTP)
	mux.HandleFunc("/pause", s.requireSession(http.HandlerFunc(s.handlePause)).ServeHTTP)
	mux.HandleFunc("/resume", s.requireSession(http.HandlerFunc(s.handleResume)).ServeHTTP)
	mux.Handle("/", s.requireSession(http.HandlerFunc(s.handleBoard)))
	return mux, nil
}

// --- login -----------------------------------------------------------------

type loginView struct {
	// Insecure marks a plaintext browser connection so the page can say so
	// (NETWORKING.md 7.1, THREAT_MODEL.md T8).
	Insecure bool
	// NoAuth marks an instance running without a token. The page says so on
	// every render: an unauthenticated board is readable and actionable by
	// anyone who can reach it (NETWORKING.md 7.1).
	NoAuth bool
	Error  string
	CSRF   string
}

// maxActionBody caps a UI POST body. A login form, an annotation, or a trace
// request is a few hundred bytes; anything larger is not a form this handler
// understands, and parsing it costs memory for no benefit (THREAT_MODEL.md T10).
const maxActionBody = 8 << 10

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	now := s.now()

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if _, _, ok := s.auth.sessionFrom(r); ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		csrf := randomToken()
		s.auth.setCSRFCookie(w, csrf, now)
		s.renderLogin(w, r, http.StatusOK, loginView{CSRF: csrf})

	case http.MethodPost:
		// A login form is a few hundred bytes. Cap the body so a large POST
		// cannot make the handler parse megabytes; the same 8 KiB ceiling the
		// API action bodies use applies here (THREAT_MODEL.md T10).
		r.Body = http.MaxBytesReader(w, r.Body, maxActionBody)
		if err := r.ParseForm(); err != nil {
			s.renderLogin(w, r, http.StatusBadRequest, loginView{Error: "Malformed form."})
			return
		}
		if !s.auth.limit.Allow(s.clientIP(r)) || !s.auth.global.Allow("login") {
			w.Header().Set("Retry-After", "60")
			s.renderLogin(w, r, http.StatusTooManyRequests, loginView{Error: "Too many attempts. Wait a minute."})
			return
		}
		if !s.auth.csrfMatches(r, r.PostFormValue("csrf")) {
			csrf := randomToken()
			s.auth.setCSRFCookie(w, csrf, now)
			s.renderLogin(w, r, http.StatusBadRequest, loginView{Error: "Session expired. Try again.", CSRF: csrf})
			return
		}
		if !s.auth.checkToken(r.PostFormValue("token")) {
			s.deps.Logger.Warn("login failed",
				"event", "login_failed", "component", "http", "client", s.clientIP(r))
			s.renderLogin(w, r, http.StatusUnauthorized, loginView{
				Error: "That token is not correct.",
				CSRF:  r.PostFormValue("csrf"),
			})
			return
		}
		s.auth.setSessionCookie(w, s.auth.issueSession(now, now), now)
		s.deps.Logger.Info("login ok", "event", "login_ok", "component", "http", "client", s.clientIP(r))
		http.Redirect(w, r, "/", http.StatusSeeOther)

	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) renderLogin(w http.ResponseWriter, r *http.Request, status int, view loginView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view.Insecure = s.insecure(r)
	view.NoAuth = s.auth.disabled()
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_ = s.tmpl.ExecuteTemplate(w, "login.html", view)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil || !s.auth.csrfMatches(r, r.PostFormValue("csrf")) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.auth.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- probe control ---------------------------------------------------------

func (s *server) handlePause(w http.ResponseWriter, r *http.Request)  { s.setPaused(w, r, true) }
func (s *server) handleResume(w http.ResponseWriter, r *http.Request) { s.setPaused(w, r, false) }

func (s *server) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Control == nil {
		http.Error(w, "probe control is not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil || !s.auth.csrfMatches(r, r.PostFormValue("csrf")) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if paused {
		s.deps.Control.Pause()
	} else {
		s.deps.Control.Resume()
	}

	s.deps.Logger.Info("probe control",
		"event", "probe_control", "component", "http", "paused", paused)

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- board -----------------------------------------------------------------

type tileView struct {
	ID                   string
	Name                 string
	Host                 string
	Sector               string
	State                string
	Text                 string
	Glyph                string
	Class                string
	Since                string
	Aria                 string
	Badge                string
	BadgeTitle           string
	Icon                 string
	Port                 int
	Probe                string
	Tier                 int
	IntervalS            int
	ASN                  int
	Prefix               string
	CDN                  string
	ASCountry            string
	ASName               string
	IXNames              string
	Measurability        string
	CertIssuer           string
	CertNotAfter         string
	CertPinned           bool
	CertChainLen         int
	CertIntermediateSPKI string
	CertLeafFingerprint  string
	// DNS divergence is the newest advisory verdict. It never changes State.
	DNSVerdict    string
	DNSConfidence string
	DNSDetail     string
	DNSAt         string
	// Spark is the 24-hour healthy-share shape, rendered only in comfortable
	// density. Empty means "no history", which the tile shows as nothing rather
	// than a fabricated line.
	Spark string
	// Local marks a DOWN tile while the global verdict says the monitor's own
	// path is the problem. The target keeps its confirmed state; only the
	// presentation is muted (DESIGN.md §3.3).
	Local bool
}

type countView struct {
	State string
	Label string
	Glyph string
	Class string
	Count int
}

type boardView struct {
	// Insecure marks a plaintext browser connection so the page can say so
	// (NETWORKING.md 7.1, THREAT_MODEL.md T8).
	Insecure bool
	// NoAuth marks an instance running without a token. The page says so on
	// every render: an unauthenticated board is readable and actionable by
	// anyone who can reach it (NETWORKING.md 7.1).
	NoAuth  bool
	Version string
	CSRF    string
	Groups  []groupView
	Counts  []countView
	Paused  bool
	Control bool
	// Group is the current grouping key, and GroupOptions is the toggle. The
	// board is about "airlines are down" only when the reader chose that lens.
	Group        string
	GroupOptions []toggleOption
	// Density is the tile-size lens, and DensityOptions is its toggle.
	Density        string
	DensityOptions []toggleOption
	// Country is the active country filter, uppercase ISO-3166-1 when set.
	// The clear link drops it without dropping focus or the grouping lens.
	Country    string
	CountryOff string
	// Focus hides everything that is not DOWN or DEGRADED. The counts still
	// describe the whole fleet, so the summary does not lie about what is
	// hidden.
	Focus bool
	// Feeds is the staleness strip. A dead feed is visible rather than blank.
	Feeds       []feedView
	FeedNote    string
	FeedWarning bool
	// DNSObservations is the collapsed DNS divergence strip. A reader that
	// fails says so; an unconfigured one simply omits the strip.
	DNSObservations []dnsObservationView
	DNSNote         string
	// VantageNote states a recent egress move and what it did to the
	// baselines (DESIGN.md §7.5). Empty means no change in the last day or no
	// reader configured.
	VantageNote string
	// BudgetNote is set only while the daily egress budget is spent. It is a
	// hard stop, so the board names the reason rather than showing old tiles as
	// if they were fresh (ACCEPTABLE_USE.md §4).
	BudgetNote string
	// TorBudget is the Tor vantage's daily volume line, or empty when Tor is
	// not configured. It is always shown while Tor is on, because a paused
	// corroboration source explains why regional evidence went quiet.
	TorBudget string
	// Health is the Internet Health Index summary (DESIGN.md §11). OK is false
	// when nothing could be scored, and the chip then states that instead of
	// rendering a zero as health.
	Health healthView
	// Verdict is the one global verdict from DESIGN.md §3.3 and its sentence.
	// An empty Verdict means the source is not wired, and the badge is omitted.
	Verdict verdict.Result
	// CoverageNote is set when the detector reports a coverage collapse. It is
	// an advisory, not a tile colour: a mass bot-wall rollout must not read as
	// a mass outage (DESIGN.md §7.2).
	CoverageNote string
	// Fleet is the overview line: judged health, its learned band, and the
	// counters that make the denominator visible (DESIGN.md §10.1).
	Fleet fleetView
	// Incidents is the "what's happening" summary: the newest open incidents,
	// headline and link only (DESIGN.md §10.1). HappeningNote states why the
	// list is empty when the reader is missing or the query failed.
	Incidents     []happeningView
	HappeningNote string
}

// happeningView is one open incident on the board summary. It is deliberately
// shallow: the incidents page carries attribution and evidence.
type happeningView struct {
	ID       string
	Headline string
	Scope    string
	OpenedAt string
}

// fleetView is the overview line as the board renders it. The percentages are
// strings so the template does no arithmetic.
type fleetView struct {
	OK      bool
	Health  string
	Band    string
	Widened bool
	Note    string
	Detail  string
}

// toFleetView formats one fleet reading. A scope with no judged targets says so
// rather than rendering 0%; a short baseline says "learning" rather than
// drawing an invented band.
func toFleetView(f store.FleetHealth, now time.Time) fleetView {
	view := fleetView{}
	if f.Judged == 0 {
		view.Note = "No judgeable targets yet."
		return view
	}
	view.OK = true
	view.Health = fmt.Sprintf("%.1f%%", f.Healthy*100)

	if f.Band {
		view.Band = fmt.Sprintf("%.1f-%.1f%%", f.Lower*100, f.Upper*100)
		view.Widened = f.Widened
	} else {
		view.Note = fmt.Sprintf("learning %d/%d baseline samples", f.Samples, f.Window)
	}

	last := "no probe yet"
	if !f.LastProbe.IsZero() {
		age := now.Sub(f.LastProbe)
		if age < 0 {
			age = 0
		}
		last = "last cycle " + age.Round(time.Second).String() + " ago"
	}
	view.Detail = fmt.Sprintf("%s · %d targets · %d unmeasurable",
		last, f.Total, f.Unmeasurable)
	return view
}

// healthView is the index as a page renders it. Weights are percentages and
// timestamps are short, because the number is a summary and the detail belongs
// on /ihi.
type healthView struct {
	OK      bool
	Score   int
	Parts   []healthPartView
	Dropped []healthDroppedView
	Note    string
}

type healthPartView struct {
	Name   string
	Score  int
	Weight string
	At     string
}

type healthDroppedView struct {
	Name   string
	Reason string
}

// toHealthView formats one index for both the board chip and the page.
func toHealthView(index ihi.Index) healthView {
	view := healthView{OK: len(index.Parts) > 0, Score: int(index.Score + 0.5)}
	if !view.OK {
		view.Note = "No component could be scored yet."
	}
	for _, part := range index.Parts {
		view.Parts = append(view.Parts, healthPartView{
			Name:   part.Name,
			Score:  int(part.Score + 0.5),
			Weight: fmt.Sprintf("%.0f%%", part.Weight*100),
			At:     part.At.UTC().Format("15:04"),
		})
	}
	for _, dropped := range index.Dropped {
		view.Dropped = append(view.Dropped, healthDroppedView{
			Name: dropped.Name, Reason: dropped.Reason,
		})
	}
	return view
}

// feedView is one feed's health on the board. Status is text, not only colour.
type feedView struct {
	Name   string
	Status string
	Age    string
	Detail string
	Stale  bool
}

// dnsObservationView is one divergence verdict in the board strip.
type dnsObservationView struct {
	Host       string
	Verdict    string
	Confidence string
	Detail     string
	At         string
}

// groupView is one slice of the board. Sectors are the default unit because
// "airlines are down" is a different claim from "one airline is down", and a
// flat wall of icons hides the difference. The same view serves every grouping
// key so the summary, collapse behaviour, and ordering cannot diverge.
type groupView struct {
	Name     string
	Label    string
	Count    int
	Worst    string
	WorstCls string
	Glyph    string
	Healthy  bool
	// Muted marks the unmeasurable bucket, which sits at the bottom and is
	// styled quietly: it is a visibility fact, not a failure (DESIGN.md §10.2).
	Muted   bool
	Targets []tileView
	// Hidden is how many of the group's tiles are behind a page link. Zero
	// means everything is rendered. MoreHref and AllHref are the paging links.
	Hidden   int
	MoreHref string
	AllHref  string
}

// toggleOption is one entry in a board toggle (grouping or density).
type toggleOption struct {
	Key    string
	Label  string
	Active bool
	// Href switches this control while preserving the rest of the view state.
	// Built here so the template does not have to assemble query strings.
	Href string
}

// Board density modes from DESIGN.md §10.2.
const (
	densityCompact     = "compact"
	densityComfortable = "comfortable"
)

// boardParams is the view state every toggle link must preserve. Building hrefs
// from one struct is what stops the grouping toggle from dropping the country
// filter, or the density toggle from dropping focus mode.
type boardParams struct {
	Group   string
	Focus   bool
	Country string
	Density string
	// Show is how many tiles each group may render, keyed by group name. It is
	// only meaningful in comfortable density, where the documented rule is
	// "paginated by group, never all at once".
	Show map[string]int
}

// boardPageSize is the first page of tiles in one group. Compact never pages
// (it is designed to fit ~760 tiles); comfortable pages because the tiles are
// large.
const boardPageSize = 60

func (p boardParams) href() string {
	href := "/?group=" + p.Group
	if p.Focus {
		href += "&focus=problems"
	}
	if p.Country != "" {
		href += "&country=" + url.QueryEscape(p.Country)
	}
	if p.Density != "" && p.Density != densityCompact {
		href += "&density=" + p.Density
		// Show only travels with the density that uses it, so switching back to
		// compact cannot leave a stale paging parameter in a link.
		if len(p.Show) > 0 {
			keys := make([]string, 0, len(p.Show))
			for name := range p.Show {
				keys = append(keys, name)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, name := range keys {
				parts = append(parts, fmt.Sprintf("%s:%d", name, p.Show[name]))
			}
			href += "&show=" + url.QueryEscape(strings.Join(parts, ","))
		}
	}
	return href
}

// parseShow reads the per-group page sizes. A malformed entry is ignored: the
// board still renders its first page rather than returning an error for a URL
// a human typed.
func parseShow(raw string) map[string]int {
	out := make(map[string]int)
	for _, part := range strings.Split(raw, ",") {
		name, number, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		n, err := strconv.Atoi(strings.TrimSpace(number))
		if name == "" || err != nil || n <= 0 {
			continue
		}
		out[name] = n
	}
	return out
}

// pageGroups truncates each group to its page size and fills the paging links.
// Compact density is left alone: it is the lens that is meant to fit.
func pageGroups(groups []groupView, params boardParams) {
	for gi := range groups {
		total := len(groups[gi].Targets)
		limit := boardPageSize
		if n, ok := params.Show[groups[gi].Name]; ok && n > limit {
			limit = n
		}
		if total <= limit {
			continue
		}

		groups[gi].Targets = groups[gi].Targets[:limit]
		groups[gi].Hidden = total - limit

		more := params
		more.Show = cloneShow(params.Show)
		more.Show[groups[gi].Name] = limit + boardPageSize
		groups[gi].MoreHref = more.href()

		all := params
		all.Show = cloneShow(params.Show)
		all.Show[groups[gi].Name] = total
		groups[gi].AllHref = all.href()
	}
}

func cloneShow(in map[string]int) map[string]int {
	out := make(map[string]int, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// normaliseDensity maps an unknown or empty value to the default. Compact is
// the documented default: at 750 targets the wall of tiles must fit.
func normaliseDensity(density string) string {
	if strings.EqualFold(strings.TrimSpace(density), densityComfortable) {
		return densityComfortable
	}
	return densityCompact
}

// densityOptions builds the density toggle with the active entry marked.
func densityOptions(active string, params boardParams) []toggleOption {
	modes := []struct{ key, label string }{
		{densityCompact, "Compact"},
		{densityComfortable, "Comfortable"},
	}
	out := make([]toggleOption, 0, len(modes))
	for _, m := range modes {
		next := params
		next.Density = m.key
		out = append(out, toggleOption{
			Key: m.key, Label: m.label, Active: m.key == active, Href: next.href(),
		})
	}
	return out
}

// groupModes is the documented toggle order (DESIGN.md §10.2): sector is the
// default, then the lenses that answer "is it one host, one network, one
// country, or one broken stage?".
var groupModes = []struct {
	Key   string
	Label string
}{
	{"sector", "Sector"},
	{"state", "State"},
	{"cdn", "CDN"},
	{"asn", "ASN"},
	{"country", "Country"},
	{"stage", "Stage"},
}

// normaliseGroupMode maps an unknown or empty value to the default. An invalid
// value is a typo, not a 400: the board still renders.
func normaliseGroupMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	for _, m := range groupModes {
		if m.Key == mode {
			return mode
		}
	}
	return "sector"
}

// groupBy groups severity-ordered targets by mode, preserving the incoming
// order inside each group and putting the group with the worst state first.
func groupBy(targets []board.Target, mode string) []groupView {
	mode = normaliseGroupMode(mode)

	order := make([]string, 0, 8)
	labels := make(map[string]string, 8)
	byName := make(map[string][]board.Target)

	for _, t := range targets {
		name, label := groupKey(t, mode)
		if _, ok := byName[name]; !ok {
			order = append(order, name)
			labels[name] = label
		}
		byName[name] = append(byName[name], t)
	}

	groups := make([]groupView, 0, len(order))
	for _, name := range order {
		members := board.Order(byName[name])
		worst := members[0].State // members are already severity-ordered

		healthy := true
		for _, t := range members {
			if t.State != board.StateUp && t.State != board.StateUpReachability {
				healthy = false
				break
			}
		}

		group := groupView{
			Name:     name,
			Label:    labels[name],
			Count:    len(members),
			Worst:    worst.Text(),
			WorstCls: worst.CSSClass(),
			Glyph:    worst.Glyph(),
			Healthy:  healthy,
			Targets:  make([]tileView, 0, len(members)),
		}
		for _, t := range members {
			group.Targets = append(group.Targets, toTile(t))
		}
		groups = append(groups, group)
	}

	sort.SliceStable(groups, func(i, j int) bool {
		ri, rj := worstRank(groups[i]), worstRank(groups[j])
		if ri != rj {
			return ri < rj
		}
		return groups[i].Name < groups[j].Name
	})
	return groups
}

// groupKey returns the stable identity and the human label for one target under
// a grouping mode. An empty value is a fact ("not learned yet"), so it gets its
// own bucket rather than being folded into a named one.
// unmeasurableGroup is the quiet bucket at the bottom of the board. It is
// marked healthy so the template leaves it collapsed: a target the monitor
// cannot see is not an outage to expand.
func unmeasurableGroup(targets []board.Target) groupView {
	members := board.Order(targets)
	state := board.StateUnmeasurable
	group := groupView{
		Name: "unmeasurable", Label: "Unmeasurable", Count: len(members),
		Worst: state.Text(), WorstCls: state.CSSClass(), Glyph: state.Glyph(),
		Healthy: true, Muted: true,
		Targets: make([]tileView, 0, len(members)),
	}
	for _, t := range members {
		group.Targets = append(group.Targets, toTile(t))
	}
	return group
}

func groupKey(t board.Target, mode string) (name, label string) {
	switch mode {
	case "state":
		return string(t.State), t.State.Text()
	case "cdn":
		if t.CDN == "" {
			return "none", "No CDN learned"
		}
		return t.CDN, t.CDN
	case "asn":
		if t.ASN == 0 {
			return "0", "ASN not learned yet"
		}
		return strconv.Itoa(t.ASN), "AS" + strconv.Itoa(t.ASN)
	case "country":
		if t.Country == "" {
			return "none", "Country unknown"
		}
		return t.Country, t.Country
	case "stage":
		if t.Stage == "" {
			return "none", "No stage recorded"
		}
		return t.Stage, t.Stage
	default:
		if t.Sector == "" {
			return "other", "Other"
		}
		return t.Sector, t.Sector
	}
}

// groupOptions builds the toggle with the active entry marked. Focus mode and
// the country filter are preserved when switching lenses, because the controls
// answer different questions and silently resetting one is hostile.
func groupOptions(active string, params boardParams) []toggleOption {
	out := make([]toggleOption, 0, len(groupModes))
	for _, m := range groupModes {
		next := params
		next.Group = m.Key
		out = append(out, toggleOption{
			Key: m.Key, Label: m.Label, Active: m.Key == active, Href: next.href(),
		})
	}
	return out
}

func worstRank(g groupView) int {
	for _, st := range []board.State{
		board.StateDown, board.StateDegraded, board.StateBlocked,
		board.StateUnknown, board.StateConfigError, board.StateUnmeasurable,
		board.StateUpReachability, board.StateUp,
	} {
		if g.WorstCls == st.CSSClass() {
			return board.SeverityRank(st)
		}
	}
	return 99
}

func (s *server) handleBoard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	targets, err := s.deps.Source.Snapshot(r.Context())
	if err != nil {
		s.deps.Logger.Error("board snapshot failed", "event", "board_failed", "component", "http", "error", err)
		http.Error(w, "board unavailable", http.StatusServiceUnavailable)
		return
	}
	// Failures first: the grid answers "what is down?", not "what is alphabetical".
	targets = board.Order(targets)

	// Counts describe the whole fleet even in focus mode: the summary must not
	// claim there are no healthy targets just because they are hidden. A target
	// the calibration has marked unmeasurable is counted that way even when its
	// last confirmed state is BLOCKED, so the summary agrees with the quiet
	// section it is rendered in (DESIGN.md §3.2).
	counts := make(map[board.State]int)
	for _, t := range targets {
		if t.Measurability == "unmeasurable" {
			counts[board.StateUnmeasurable]++
			continue
		}
		counts[t.State]++
	}

	focus := false
	switch r.URL.Query().Get("focus") {
	case "problems", "problem":
		focus = true
		filtered := make([]board.Target, 0, len(targets))
		for _, t := range targets {
			if t.State == board.StateDown || t.State == board.StateDegraded {
				filtered = append(filtered, t)
			}
		}
		targets = filtered
	}

	// A country filter is what a world-view tile links to. It is applied after
	// the counts so the summary still describes the whole fleet.
	country := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("country")))
	if country != "" {
		filtered := make([]board.Target, 0, len(targets))
		for _, t := range targets {
			if strings.EqualFold(t.Country, country) {
				filtered = append(filtered, t)
			}
		}
		targets = filtered
	}

	csrf, _ := s.auth.csrfFrom(r)
	params := boardParams{
		Group:   normaliseGroupMode(r.URL.Query().Get("group")),
		Focus:   focus,
		Country: country,
		Density: normaliseDensity(r.URL.Query().Get("density")),
		Show:    parseShow(r.URL.Query().Get("show")),
	}
	// Unmeasurable targets are a visibility fact, not a failure: give them their
	// own quiet section at the bottom instead of letting them sit among a
	// sector's problems (DESIGN.md §10.2).
	measurable := make([]board.Target, 0, len(targets))
	var unmeasurable []board.Target
	for _, t := range targets {
		// The confirmed state says what the last probe saw; measurability says
		// whether the target can be measured at all. A permanently bot-walled
		// target is collapsed even while its last state is BLOCKED
		// (DESIGN.md §3.2).
		if t.State == board.StateUnmeasurable || t.Measurability == "unmeasurable" {
			unmeasurable = append(unmeasurable, t)
			continue
		}
		measurable = append(measurable, t)
	}
	groups := groupBy(measurable, params.Group)
	if len(unmeasurable) > 0 {
		groups = append(groups, unmeasurableGroup(unmeasurable))
	}

	// Comfortable tiles are large, so each group shows one page at a time
	// (DESIGN.md §10.2). The paging links carry every other view control.
	if params.Density == densityComfortable {
		pageGroups(groups, params)
	}

	view := boardView{
		Insecure:       s.insecure(r),
		Version:        s.deps.Version,
		CSRF:           csrf,
		Groups:         groups,
		Control:        s.deps.Control != nil,
		Focus:          focus,
		Group:          params.Group,
		GroupOptions:   groupOptions(params.Group, params),
		Density:        params.Density,
		DensityOptions: densityOptions(params.Density, params),
		Country:        country,
	}
	if country != "" {
		clear := params
		clear.Country = ""
		view.CountryOff = clear.href()
	}
	if s.deps.Control != nil {
		view.Paused = s.deps.Control.Paused()
	}

	// Comfortable density shows a 24-hour shape per tile (DESIGN.md §10.2).
	// One query for the visible set, not one per tile; without a batch reader
	// the tiles simply omit it rather than paying a read per target.
	if params.Density == densityComfortable && s.deps.SeriesBatch != nil {
		vantage := s.deps.Vantage
		if vantage == "" {
			vantage = "local"
		}
		ids := make([]string, 0, len(targets))
		for _, group := range view.Groups {
			for _, tile := range group.Targets {
				ids = append(ids, tile.ID)
			}
		}
		now := time.Now().UTC()
		series, err := s.deps.SeriesBatch.SeriesForTargets(
			r.Context(), ids, vantage, "1h", now.Add(-24*time.Hour), now)
		if err != nil {
			s.deps.Logger.Warn("board sparkline history failed",
				"event", "board_spark_failed", "component", "http", "error", err)
		} else {
			for gi := range view.Groups {
				for ti := range view.Groups[gi].Targets {
					if points := series[view.Groups[gi].Targets[ti].ID]; len(points) > 0 {
						view.Groups[gi].Targets[ti].Spark = rollupSpark(points)
					}
				}
			}
		}
	}

	for _, st := range []board.State{
		board.StateDown, board.StateDegraded, board.StateBlocked,
		board.StateUnknown, board.StateConfigError, board.StateUnmeasurable,
		board.StateUpReachability, board.StateUp,
	} {
		if n := counts[st]; n > 0 {
			view.Counts = append(view.Counts, countView{
				State: string(st), Label: st.Text(), Glyph: st.Glyph(), Class: st.CSSClass(), Count: n,
			})
		}
	}

	// A dead feed is a failure mode, not a blank space (DESIGN.md P5). The strip
	// says "stale" and "never succeeded" explicitly; a missing source says so
	// rather than looking healthy.
	if s.deps.Feeds == nil {
		view.FeedNote = "Feed health is not configured on this instance."
	} else {
		states, err := s.deps.Feeds.FeedStates(r.Context())
		switch {
		case err != nil:
			s.deps.Logger.Warn("feed health failed",
				"event", "feed_health_failed", "component", "http", "error", err)
			view.FeedNote = "Feed health is temporarily unavailable."
		case len(states) == 0:
			view.FeedNote = "No external feeds are enabled."
		default:
			now := time.Now().UTC()
			for _, f := range states {
				row := feedView{Name: f.Feed, Status: "ok"}
				if f.LastOKAt.IsZero() {
					row.Status = "never"
					row.Stale = true
				} else {
					age := now.Sub(f.LastOKAt)
					row.Age = age.Round(time.Minute).String()
					if age > 30*time.Minute {
						row.Status = "stale"
						row.Stale = true
					}
				}
				if f.ConsecutiveFailures > 0 {
					row.Detail = fmt.Sprintf("%d consecutive failure(s): %s",
						f.ConsecutiveFailures, f.LastErr)
					if !row.Stale {
						row.Status = "failing"
					}
				}
				if row.Stale {
					view.FeedWarning = true
				}
				view.Feeds = append(view.Feeds, row)
			}
		}
	}

	// A spent egress budget is a hard stop (ACCEPTABLE_USE.md §4). Say so on
	// the board: stale tiles with no explanation read as a broken monitor.
	if s.deps.Budget != nil && s.deps.Budget.Exhausted() {
		view.BudgetNote = fmt.Sprintf(
			"Daily egress budget spent (%s of %s). Probing stops until 00:00 UTC; the tiles show the last measurements, not fresh ones.",
			humanBytes(s.deps.Budget.Used()), humanBytes(s.deps.Budget.Allowance()))
	}

	// The Tor vantage has its own allowance; when it is spent the corroboration
	// path is paused until UTC midnight, and the board should say so rather
	// than leave a silent gap in regional evidence (DESIGN.md §5.7).
	if s.deps.TorBudget != nil && s.deps.TorBudget.Allowance() > 0 {
		used, allowance := s.deps.TorBudget.Used(), s.deps.TorBudget.Allowance()
		view.TorBudget = fmt.Sprintf("Tor budget: %s of %s used (%s left)",
			humanBytes(used), humanBytes(allowance), humanBytes(allowance-used))
		if s.deps.TorBudget.Exhausted() {
			view.TorBudget = fmt.Sprintf(
				"Tor budget spent (%s of %s). The Tor vantage is paused until 00:00 UTC.",
				humanBytes(used), humanBytes(allowance))
		}
	}

	// A coverage collapse is an advisory, not a tile colour: a mass bot-wall
	// rollout must not read as a mass outage (DESIGN.md §7.2). The newest one
	// inside six hours gets a line; older advisories stay in the API.
	if s.deps.Anomalies != nil {
		anomalies, err := s.deps.Anomalies.Anomalies(r.Context(), 20)
		if err != nil {
			s.deps.Logger.Warn("anomalies failed",
				"event", "anomalies_failed", "component", "http", "error", err)
		}
		now := s.now()
		for _, a := range anomalies {
			if a.Kind != "coverage_drop" {
				continue
			}
			if now.Sub(a.TS) > 6*time.Hour {
				break
			}
			view.CoverageNote = fmt.Sprintf(
				"Coverage for %s fell to %.0f%% from a %.0f%% baseline at %s. That is a visibility loss, not an outage; the tiles keep their confirmed states.",
				scopeLabel(a), a.Observed*100, a.Baseline*100, formatTimestamp(a.TS))
			break
		}
	}

	// The DNS divergence strip is collapsed by design: an advisory is not a
	// board colour, and a failure here must not hide the board.
	if s.deps.DNSObservations != nil {
		records, err := s.deps.DNSObservations.RecentDNSDivergences(r.Context(), 10)
		switch {
		case err != nil:
			s.deps.Logger.Warn("dns observations failed",
				"event", "dns_observations_failed", "component", "http", "error", err)
			view.DNSNote = "DNS observations are temporarily unavailable."
		default:
			for _, record := range records {
				view.DNSObservations = append(view.DNSObservations, dnsObservationView{
					Host: record.Host, Verdict: record.Verdict,
					Confidence: record.Confidence, Detail: record.Detail,
					At: formatTimestamp(record.TS),
				})
			}
		}
	}

	// The index is a summary with its parts attached. A missing builder omits
	// the chip; a builder with nothing scoreable says so instead of showing 0.
	if s.deps.Health != nil {
		view.Health = toHealthView(s.deps.Health.Build(r.Context()))
	}

	// The one global verdict (DESIGN.md §3.3). It is a summary of signals that
	// are already on the page, so it is computed after they are read.
	if s.deps.Verdict != nil {
		view.Verdict = s.deps.Verdict.Build(r.Context())
	}

	// The overview line (DESIGN.md §10.1): judged health, its learned band, and
	// the counters that explain the denominator. A failed read is a stated
	// line, not a blank that reads as calm.
	if s.deps.Fleet != nil {
		fleetVantage := s.deps.Vantage
		if fleetVantage == "" {
			fleetVantage = "local"
		}
		now := s.now()
		fleet, err := s.deps.Fleet.FleetHealth(r.Context(), fleetVantage, now)
		if err != nil {
			s.deps.Logger.Warn("fleet health failed",
				"event", "fleet_health_failed", "component", "http", "error", err)
			view.Fleet = fleetView{Note: "Fleet health is temporarily unavailable."}
		} else {
			view.Fleet = toFleetView(fleet, now)
		}
	}

	// "What's happening" is the incident summary from the overview sketch
	// (DESIGN.md §10.1): headline and link, never a second incidents page.
	// A missing reader, a failed query, and no open incidents are three
	// different statements and each says its own.
	if s.deps.Incidents == nil {
		view.HappeningNote = "The incident log is not configured on this instance."
	} else {
		open, err := s.deps.Incidents.Incidents(r.Context(), true, 3)
		switch {
		case err != nil:
			s.deps.Logger.Warn("board incidents failed",
				"event", "board_incidents_failed", "component", "http", "error", err)
			view.HappeningNote = "The incident log is temporarily unavailable."
		case len(open) == 0:
			view.HappeningNote = "No open incidents."
		default:
			for _, inc := range open {
				scope := inc.Scope
				if inc.ScopeValue != "" {
					scope += " " + inc.ScopeValue
				}
				view.Incidents = append(view.Incidents, happeningView{
					ID:       inc.ID,
					Headline: inc.Headline,
					Scope:    scope,
					OpenedAt: formatTimestamp(inc.OpenedAt),
				})
			}
		}
	}

	// LOCAL suppresses per-target red (DESIGN.md §3.3): the targets keep their
	// confirmed DOWN state, but a red grid while the monitor's own path is the
	// problem is the false alarm this rule exists to prevent. The mute is
	// presentation, not a state write.
	if view.Verdict.Verdict == verdict.Local {
		for gi := range view.Groups {
			for ti := range view.Groups[gi].Targets {
				if view.Groups[gi].Targets[ti].State == "down" {
					view.Groups[gi].Targets[ti].Local = true
				}
			}
		}
	}

	// A recent egress move invalidates every latency and anomaly baseline; the
	// board says so plainly rather than leaving the reader to wonder why the
	// numbers moved (DESIGN.md §7.5). The note expires after a day.
	if s.deps.VantageChanges != nil {
		change, ok, err := s.deps.VantageChanges.LatestVantageChange(r.Context())
		switch {
		case err != nil:
			s.deps.Logger.Warn("vantage change read failed",
				"event", "vantage_change_read_failed", "component", "http", "error", err)
		case ok && s.now().Sub(change.At) <= 24*time.Hour:
			view.VantageNote = vantageSentence(change)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view.Insecure = s.insecure(r)
	view.NoAuth = s.auth.disabled()
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "board.html", view)
}

func toTile(t board.Target) tileView {
	since := ""
	if !t.Since.IsZero() {
		since = t.Since.UTC().Format("15:04 MST")
	}

	aria := fmt.Sprintf("%s (%s): %s", t.Name, t.Host, t.State.Text())
	if since != "" {
		aria += ", since " + since
	}

	badge, badgeTitle := badgeFor(t)
	if badge != "" {
		aria += ", " + badgeTitle
	}

	return tileView{
		ID: t.ID, Name: t.Name, Host: t.Host, Sector: t.Sector,
		State: string(t.State), Text: t.State.Text(), Glyph: t.State.Glyph(),
		Class: t.State.CSSClass(), Since: since, Aria: aria,
		Badge: badge, BadgeTitle: badgeTitle, Icon: iconURL(t.ID),
		Port: t.Port, Probe: t.Probe, Tier: t.Tier, IntervalS: t.IntervalS,
		ASN: t.ASN, Prefix: t.Prefix, CDN: t.CDN, Measurability: t.Measurability,
		ASCountry: t.ASCountry, ASName: t.ASName, IXNames: t.IXNames,
		CertIssuer: t.CertIssuer, CertNotAfter: formatCertExpiry(t.CertNotAfter),
		CertPinned:           t.CertPinned,
		CertChainLen:         t.CertChainLen,
		CertIntermediateSPKI: t.CertIntermediateSPKI,
		CertLeafFingerprint:  t.CertLeafFingerprint,
		DNSVerdict:           t.DNSVerdict,
		DNSConfidence:        t.DNSConfidence,
		DNSDetail:            t.DNSDetail,
		DNSAt:                formatTimestamp(t.DNSAt),
	}
}

// formatCertExpiry prints a date rather than a timestamp: "when does this
// lapse" is a day-scale question, and an empty value means the last probe was
// not TLS.
func formatCertExpiry(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

// formatTimestamp prints a minute-resolution timestamp, which is what an
// observation time needs. Empty means "never observed".
func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// scopeLabel names one anomaly's scope in a sentence. Global has no value, so
// it says "the fleet" rather than an empty string.
func scopeLabel(a store.Anomaly) string {
	if a.ScopeValue == "" {
		return "the fleet"
	}
	return a.Scope + " " + a.ScopeValue
}

// pinLabel says whether an issuer baseline exists. "Learned" is not "safe": the
// pin flags a change once, it does not certify the certificate (SCHEMA.md §8).
func pinLabel(known bool) string {
	if known {
		return "issuer learned"
	}
	return "not learned yet"
}

// chainLabel describes the learned chain shape. It is informational: a chain
// change is reported, never used as a verdict (CLASSIFIER.md §3.5).
func chainLabel(length int, intermediate string) string {
	if length <= 0 {
		return "not learned yet"
	}
	label := fmt.Sprintf("%d certificate(s)", length)
	if intermediate != "" {
		short := intermediate
		if len(short) > 16 {
			short = short[:16]
		}
		label += ", intermediate " + short + "…"
	}
	return label
}

// leafLabel shows a short leaf fingerprint. It is informational: leaves rotate
// on a schedule, so a change is not a verdict (CLASSIFIER.md §3.5).
func leafLabel(fingerprint string) string {
	if fingerprint == "" {
		return "not learned yet"
	}
	short := fingerprint
	if len(short) > 16 {
		short = short[:16]
	}
	return "sha256 " + short + "…"
}

// dnsLabel states the newest divergence verdict, or that none was observed. It
// is advisory: it never changes the tile's state (CLASSIFIER.md §3.2).
func dnsLabel(verdict, confidence, detail, at string) string {
	if verdict == "" {
		return "none observed"
	}
	label := verdict
	if confidence != "" {
		label += " (" + confidence + ")"
	}
	if at != "" {
		label += " at " + at
	}
	if detail != "" {
		label += ": " + detail
	}
	return label
}

// badgeFor is the corner marker from DESIGN.md §10.2: the stage or status that
// produced the current state, so a tile says "TLS" or "403" at a glance.
func badgeFor(t board.Target) (string, string) {
	if t.ErrorClass != "" {
		switch {
		case strings.HasPrefix(t.ErrorClass, "dns_"):
			return "DNS", t.ErrorClass
		case strings.HasPrefix(t.ErrorClass, "tcp_"):
			return "TCP", t.ErrorClass
		case strings.HasPrefix(t.ErrorClass, "tls_"):
			return "TLS", t.ErrorClass
		case strings.HasPrefix(t.ErrorClass, "http_"):
			return "HTTP", t.ErrorClass
		default:
			return "ERR", t.ErrorClass
		}
	}
	if t.HTTPStatus > 0 {
		return strconv.Itoa(t.HTTPStatus), fmt.Sprintf("HTTP %d", t.HTTPStatus)
	}
	if t.Stage != "" {
		return strings.ToUpper(t.Stage), "stage " + t.Stage
	}
	return "", ""
}

// toStrip turns hourly buckets into a 24-hour strip. The bar conveys the share
// of healthy observations; the title carries the exact numbers and the EWMA
// latency, because an EWMA is not a percentile and the UI must not imply it is.
func toStrip(points []store.Rollup) []stripCell {
	out := make([]stripCell, 0, len(points))
	for _, p := range points {
		// The rollup's "up" column already folds in up_reachability
		// (store.counters), so only DEGRADED needs adding: it is not healthy,
		// but it is not an outage either.
		healthy := p.Up + p.Degraded

		share := 0.0
		if p.N > 0 {
			share = float64(healthy) / float64(p.N)
		}
		idx := int(share * float64(len(trendBlocks)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(trendBlocks) {
			idx = len(trendBlocks) - 1
		}

		class := "strip-ok"
		switch {
		case p.Down > 0 && share < 0.5:
			class = "strip-down"
		case share < 0.9:
			class = "strip-warn"
		}

		latency := "no latency sample"
		if p.Ewma50MS > 0 {
			latency = fmt.Sprintf("EWMA %d ms", p.Ewma50MS)
		}
		out = append(out, stripCell{
			Char:  string(trendBlocks[idx]),
			Class: class,
			Title: fmt.Sprintf("%s UTC: %d/%d healthy, %s",
				p.Bucket.UTC().Format("15:04"), healthy, p.N, latency),
		})
	}
	return out
}

type metaRow struct {
	Label string
	Value string
}

type changeRow struct {
	TS     string
	From   string
	To     string
	Reason string
}

// displayOr keeps empty enrichment honest instead of rendering an empty cell.
func displayOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// humanBytes formats a byte count for the budget banner. MB and GB are the only
// units the setting uses, and a decimal keeps 2 GB from rendering as "2048 MB".
// vantageSentence renders DESIGN.md §7.5's admission. The stored tuple is
// "ip,AS<number>,<CC>"; the literal address is deliberately left off the board,
// while the ASN and country are what a reader can act on.
func vantageSentence(c store.VantageChange) string {
	prevASN, prevCC := egressLabels(c.Previous)
	newASN, newCC := egressLabels(c.Current)

	var parts []string
	if prevASN != "" && newASN != "" && prevASN != newASN {
		parts = append(parts, prevASN+" → "+newASN)
	}
	if prevCC != "" && newCC != "" && prevCC != newCC {
		parts = append(parts, prevCC+" → "+newCC)
	}
	if len(parts) == 0 {
		return "Your egress changed. Baselines reset; expect about 24h of reduced anomaly sensitivity."
	}
	return fmt.Sprintf("Your egress changed (%s). Baselines reset; expect about 24h of reduced anomaly sensitivity.",
		strings.Join(parts, ", "))
}

// egressLabels extracts the ASN and country from a stored egress tuple. A
// failed self-check stores an empty value, which yields empty labels rather
// than a made-up one.
func egressLabels(tuple string) (asn, country string) {
	for _, part := range strings.Split(tuple, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "AS"):
			asn = part
		case len(part) == 2 && part == strings.ToUpper(part):
			country = part
		}
	}
	return asn, country
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func asnLabel(asn int) string {
	if asn <= 0 {
		return "not learned yet"
	}
	return fmt.Sprintf("AS%d", asn)
}

type targetView struct {
	// Insecure marks a plaintext browser connection so the page can say so
	// (NETWORKING.md 7.1, THREAT_MODEL.md T8).
	Insecure   bool
	NoAuth     bool
	Tile       tileView
	Probes     []probeView
	History    bool
	Strip      []stripCell
	StripNote  string
	Meta       []metaRow
	Changes    []changeRow
	ChangeNote string
	// Corroboration is feed evidence for this target's country or ASN. It is
	// never presented as the provider's own status (DESIGN.md §10.5).
	Corroboration     []targetCorroborationView
	CorroborationNote string
	// Provider is evidence from the operator's declared status page for this
	// target, and StatusURL is that page. Empty ProviderNote means the target
	// declared no page, so the panel is omitted rather than guessed.
	Provider     []targetCorroborationView
	ProviderNote string
	StatusURL    string
	// Compare is the "you vs official" sentence from DESIGN.md §8.4: when the
	// target is failing but its declared status page reports no incident, the
	// asymmetry is the most useful thing the drawer can say.
	Compare string
	// Why is the drawer's "Why?" panel: the newest log lines that name this
	// target (LOGGING.md §9). The state-change reasons above are the other half
	// of that answer.
	Why     []diagnosticsRecord
	WhyNote string
	// Vantages is every vantage's confirmed state for this target. A
	// disagreement is what makes REGIONAL distinct from a service outage
	// (DESIGN.md §7.1); RegionalNote names it in words.
	Vantages     []vantageStateView
	RegionalNote string
	// Sources is where this row came from and the hash of that payload
	// (THREAT_MODEL.md T35). SourcesNote states a failed or empty read; both
	// empty means the panel is omitted.
	Sources     []sourceView
	SourcesNote string
}

// sourceView is one provenance row in the drawer.
type sourceView struct {
	Source    string
	FetchedAt string
	SHA256    string
	Short     string
}

// vantageStateView is one vantage's row in the drawer.
type vantageStateView struct {
	ID      string
	Kind    string
	Role    string
	State   string
	Text    string
	Glyph   string
	Since   string
	Primary bool
}

// targetCorroborationView is one feed event near the target's scope.
type targetCorroborationView struct {
	Feed      string
	Kind      string
	Severity  string
	Place     string
	StartedAt string
	EndedAt   string
}

// toCorroborationRows renders feed events for the drawer. The place is the
// country or ASN the event names, never a guess.
func toCorroborationRows(events []store.ExternalEvent) []targetCorroborationView {
	out := make([]targetCorroborationView, 0, len(events))
	for _, e := range events {
		row := targetCorroborationView{
			Feed: e.Feed, Kind: e.Kind,
			Severity:  fmt.Sprintf("%.2f", e.Severity),
			StartedAt: e.StartedAt.UTC().Format("2006-01-02 15:04"),
		}
		switch {
		case e.Country != "":
			row.Place = e.Country
		case e.ASN > 0:
			row.Place = fmt.Sprintf("AS%d", e.ASN)
		default:
			row.Place = "-"
		}
		if !e.EndedAt.IsZero() {
			row.EndedAt = e.EndedAt.UTC().Format("2006-01-02 15:04")
		}
		out = append(out, row)
	}
	return out
}

// stripCell is one hour of the 24-hour strip. The bar is characters because the
// page's CSP has no style-src; the title carries the exact numbers.
type stripCell struct {
	Char  string
	Class string
	Title string
}

type probeView struct {
	TS         string
	State      string
	Text       string
	Glyph      string
	Stage      string
	ErrorClass string
	HTTPStatus int
	Latency    string
	Confidence string
	Detail     string
}

// toProbeView renders one stored observation for the probe tables. Times are
// local to the reader's clock, not the target's: the page is about "when did
// this happen to me".
func toProbeView(row store.ProbeRow) probeView {
	latency := "-"
	if row.LatencyMS > 0 {
		latency = fmt.Sprintf("%d ms", row.LatencyMS)
	}
	return probeView{
		TS:         row.TS.UTC().Format("15:04:05"),
		State:      string(row.State),
		Text:       row.State.Text(),
		Glyph:      row.State.Glyph(),
		Stage:      row.Stage,
		ErrorClass: row.ErrorClass,
		HTTPStatus: row.HTTPStatus,
		Latency:    latency,
		Confidence: row.Confidence,
		Detail:     row.Detail,
	}
}

func (s *server) handleTarget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := r.URL.Path[len("/targets/"):]
	targets, err := s.deps.Source.Snapshot(r.Context())
	if err != nil {
		http.Error(w, "board unavailable", http.StatusServiceUnavailable)
		return
	}
	for _, t := range targets {
		if t.ID != id {
			continue
		}

		view := targetView{Tile: toTile(t), Insecure: s.insecure(r)}
		vantage := s.deps.Vantage
		if vantage == "" {
			vantage = "local"
		}

		// Every vantage's answer, so a disagreement is shown rather than called
		// global (DESIGN.md §7.1). A failed read is a missing panel, not a
		// missing target.
		if s.deps.Vantages == nil {
			view.RegionalNote = "Vantage comparison is not configured on this instance."
		} else {
			rows, err := s.deps.Vantages.TargetVantages(r.Context(), t.ID)
			switch {
			case err != nil:
				s.deps.Logger.Warn("vantage comparison failed",
					"event", "vantage_comparison_failed", "component", "http",
					"target", t.Host, "error", err)
				view.RegionalNote = "Vantage comparison is temporarily unavailable."
			case len(rows) == 0:
				view.RegionalNote = "No vantage has probed this target yet."
			default:
				primaryDown, otherUp := false, false
				for _, row := range rows {
					st := state.State(row.State)
					view.Vantages = append(view.Vantages, vantageStateView{
						ID: row.ID, Kind: row.Kind, Role: row.Role,
						State: row.State, Text: st.Text(), Glyph: st.Glyph(),
						Since: formatTimestamp(row.Since), Primary: row.Role == "primary",
					})
					if row.Role == "primary" && st == state.Down {
						primaryDown = true
					}
					if row.Role != "primary" && (st == state.Up || st == state.UpReachability) {
						otherUp = true
					}
				}
				if primaryDown && otherUp {
					view.RegionalNote = "The primary vantage cannot reach this target but another vantage can: this is a regional path problem, not a service outage."
				}
			}
		}

		// Where the row came from (THREAT_MODEL.md T35). The drawer shows the
		// source and the payload hash so a poisoned list can be traced back to
		// the input that produced it. An unwired reader omits the panel; a
		// failed or empty read states its case rather than leaving it blank.
		if s.deps.Provenance != nil {
			sources, err := s.deps.Provenance.TargetSources(r.Context(), t.ID)
			switch {
			case err != nil:
				s.deps.Logger.Warn("target provenance failed",
					"event", "target_provenance_failed", "component", "http",
					"target", t.Host, "error", err)
				view.SourcesNote = "Provenance is temporarily unavailable."
			case len(sources) == 0:
				view.SourcesNote = "No provenance recorded for this target; it may predate the provenance table."
			default:
				for _, src := range sources {
					short := src.SHA256
					if len(short) > 12 {
						short = short[:12]
					}
					view.Sources = append(view.Sources, sourceView{
						Source:    src.Source,
						FetchedAt: formatTimestamp(src.FetchedAt),
						SHA256:    src.SHA256,
						Short:     short,
					})
				}
			}
		}

		if s.deps.Series == nil {
			view.StripNote = "24-hour history is not configured on this instance."
		} else {
			now := time.Now().UTC()
			points, err := s.deps.Series.Series(r.Context(), t.ID, vantage, "1h",
				now.Add(-24*time.Hour), now)
			switch {
			case err != nil:
				s.deps.Logger.Warn("series failed",
					"event", "series_failed", "component", "http", "target", t.Host, "error", err)
				view.StripNote = "24-hour history is temporarily unavailable."
			case len(points) == 0:
				view.StripNote = "No hourly rollups yet; the fold runs every ten minutes."
			default:
				view.Strip = toStrip(points)
			}
		}

		view.Meta = []metaRow{
			{"Class", displayOr(t.Class, "unknown")},
			{"Sector", displayOr(t.Sector, "unclassified")},
			{"Country", displayOr(t.Country, "unknown")},
			{"Probe", displayOr(t.Probe, "http")},
			{"Port", strconv.Itoa(t.Port)},
			{"Tier", strconv.Itoa(t.Tier)},
			{"Interval", fmt.Sprintf("%ds", t.IntervalS)},
			{"ASN", asnLabel(t.ASN)},
			{"AS name", displayOr(t.ASName, "not learned yet")},
			{"AS country", displayOr(t.ASCountry, "not learned yet")},
			{"Internet exchanges", displayOr(t.IXNames, "not learned yet")},
			{"Prefix", displayOr(t.Prefix, "not learned yet")},
			{"CDN", displayOr(t.CDN, "not learned yet")},
			{"Certificate issuer", displayOr(t.CertIssuer, "not a TLS probe")},
			{"Certificate expires", displayOr(formatCertExpiry(t.CertNotAfter), "not a TLS probe")},
			{"Certificate pin", pinLabel(t.CertPinned)},
			{"Certificate chain", chainLabel(t.CertChainLen, t.CertIntermediateSPKI)},
			{"Leaf certificate", leafLabel(t.CertLeafFingerprint)},
			{"DNS divergence", dnsLabel(t.DNSVerdict, t.DNSConfidence, t.DNSDetail, formatTimestamp(t.DNSAt))},
			{"Measurability", displayOr(t.Measurability, "ok")},
		}

		// Why: the newest log lines that name this target. The state-change
		// reasons below are the other half of the answer, and a report without
		// both usually starts with "it went down and I do not know why".
		if s.deps.Logs == nil {
			view.WhyNote = "The log ring is not wired on this instance."
		} else {
			records := s.deps.Logs.LogRecords(slog.LevelDebug, t.Host, 50)
			if len(records) == 0 {
				view.WhyNote = "No recent log line names this target."
			}
			for i := len(records) - 1; i >= 0; i-- {
				view.Why = append(view.Why, toDiagnosticsRecord(records[i]))
			}
		}

		if s.deps.TargetCorroboration == nil {
			view.CorroborationNote = "Feed corroboration is not configured on this instance."
		} else if t.Country == "" && t.ASN == 0 {
			view.CorroborationNote = "No country or ASN is learned yet, so there is no scope to match."
		} else {
			events, err := s.deps.TargetCorroboration.ScopeCorroboration(
				r.Context(), t.Country, t.ASN, time.Now().UTC().Add(-24*time.Hour), 10)
			switch {
			case err != nil:
				s.deps.Logger.Warn("target corroboration failed",
					"event", "target_corroboration_failed", "component", "http",
					"target", t.Host, "error", err)
				view.CorroborationNote = "Feed corroboration is temporarily unavailable."
			case len(events) == 0:
				view.CorroborationNote = "No feed event in this target's country or ASN in the last 24 hours."
			default:
				view.Corroboration = toCorroborationRows(events)
			}
		}

		// Provider status: only when the operator declared a page. A target
		// with no declared provider has no provider claim, so the panel is
		// omitted rather than guessed (DESIGN.md §10.5).
		view.StatusURL = t.StatusURL
		providerOperational := false
		if t.StatusURL != "" {
			if s.deps.Provider == nil {
				view.ProviderNote = "Provider status is not configured on this instance."
			} else {
				events, err := s.deps.Provider.ProviderCorroboration(
					r.Context(), t.StatusURL, time.Now().UTC().Add(-24*time.Hour), 10)
				switch {
				case err != nil:
					s.deps.Logger.Warn("provider corroboration failed",
						"event", "provider_corroboration_failed", "component", "http",
						"target", t.Host, "error", err)
					view.ProviderNote = "Provider status is temporarily unavailable."
				case len(events) == 0:
					view.ProviderNote = "The provider's status page reports no incident in the last 24 hours."
					providerOperational = true
				default:
					view.Provider = toCorroborationRows(events)
				}
			}
		}

		// The most interesting sentence in the product (DESIGN.md §8.4): the
		// monitor cannot reach the target while the operator's declared status
		// page says there is nothing wrong. That asymmetry points at the local
		// path, a region, or a bot-wall rather than the service.
		failing := t.State == board.StateDown || t.State == board.StateDegraded ||
			t.State == board.StateBlocked
		if failing && providerOperational {
			view.Compare = fmt.Sprintf(
				"You: %s - official status: operational -> likely you, your region, or a bot-wall.",
				t.State.Text())
		}

		if s.deps.Changes == nil {
			view.ChangeNote = "State history is not configured on this instance."
		} else {
			changes, err := s.deps.Changes.StateChanges(r.Context(), t.ID, vantage, 20)
			switch {
			case err != nil:
				s.deps.Logger.Warn("state changes failed",
					"event", "state_changes_failed", "component", "http", "target", t.Host, "error", err)
				view.ChangeNote = "State history is temporarily unavailable."
			case len(changes) == 0:
				view.ChangeNote = "No state changes recorded yet."
			default:
				for _, c := range changes {
					view.Changes = append(view.Changes, changeRow{
						TS:     c.TS.UTC().Format("2006-01-02 15:04:05"),
						From:   c.From.Text(),
						To:     c.To.Text(),
						Reason: c.Reason,
					})
				}
			}
		}

		if s.deps.History != nil {
			view.History = true

			rows, err := s.deps.History.RecentProbes(r.Context(), t.ID, vantage, 20)
			if err != nil {
				s.deps.Logger.Warn("probe history failed",
					"event", "history_failed", "component", "http", "target", t.Host, "error", err)
			}
			for _, row := range rows {
				latency := "—"
				if row.LatencyMS > 0 {
					latency = fmt.Sprintf("%d ms", row.LatencyMS)
				}
				view.Probes = append(view.Probes, probeView{
					TS:         row.TS.UTC().Format("15:04:05"),
					State:      string(row.State),
					Text:       row.State.Text(),
					Glyph:      row.State.Glyph(),
					Stage:      row.Stage,
					ErrorClass: row.ErrorClass,
					HTTPStatus: row.HTTPStatus,
					Latency:    latency,
					Confidence: row.Confidence,
					Detail:     row.Detail,
				})
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		view.Insecure = s.insecure(r)
		view.NoAuth = s.auth.disabled()
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		_ = s.tmpl.ExecuteTemplate(w, "target.html", view)
		return
	}
	http.Error(w, "no such target", http.StatusNotFound)
}

// --- incidents -------------------------------------------------------------

type incidentRow struct {
	ID           string
	OpenedAt     string
	Scope        string
	ScopeValue   string
	Headline     string
	Confidence   string
	Open         bool
	ClosedAt     string
	ClosedReason string
}

type incidentListView struct {
	// Insecure marks a plaintext browser connection so the page can say so
	// (NETWORKING.md 7.1, THREAT_MODEL.md T8).
	Insecure  bool
	NoAuth    bool
	Incidents []incidentRow
}

type attributionView struct {
	Rank    int
	Kind    string
	Value   string
	Failing int
	Total   int
	Share   string
	Score   string
}

type incidentDetailView struct {
	// Insecure marks a plaintext browser connection so the page can say so
	// (NETWORKING.md 7.1, THREAT_MODEL.md T8).
	Insecure          bool
	NoAuth            bool
	ID                string
	Headline          string
	Scope             string
	ScopeValue        string
	OpenedAt          string
	Confidence        string
	Closed            bool
	ClosedAt          string
	ClosedReason      string
	Attribution       []attributionView
	Targets           []string
	Corroboration     []corroborationView
	CorroborationNote string
	Annotation        string
	AnnotatedAt       string
	CSRF              string
	CanAnnotate       bool
	// Archive is the probes pinned around the event (SCHEMA.md §5). It is what
	// keeps an old incident readable after the raw 72-hour window is pruned.
	Archive     []probeView
	ArchiveNote string
}

// corroborationView is one external event that overlapped the incident.
type corroborationView struct {
	Feed      string
	Kind      string
	Severity  string
	Place     string
	StartedAt string
	EndedAt   string
}

func (s *server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Incidents == nil {
		http.Error(w, "incident log is not configured", http.StatusServiceUnavailable)
		return
	}

	rows, err := s.deps.Incidents.Incidents(r.Context(), false, 100)
	if err != nil {
		s.deps.Logger.Error("incident list failed", "event", "incident_list_failed", "component", "http", "error", err)
		http.Error(w, "incident log unavailable", http.StatusServiceUnavailable)
		return
	}

	view := incidentListView{Incidents: make([]incidentRow, 0, len(rows)), Insecure: s.insecure(r)}
	for _, inc := range rows {
		row := incidentRow{
			ID: inc.ID, OpenedAt: inc.OpenedAt.UTC().Format("2006-01-02 15:04"),
			Scope: inc.Scope, ScopeValue: inc.ScopeValue, Headline: inc.Headline,
			Confidence: inc.Confidence, Open: inc.ClosedAt.IsZero(),
		}
		if !inc.ClosedAt.IsZero() {
			row.ClosedAt = inc.ClosedAt.UTC().Format("2006-01-02 15:04")
			row.ClosedReason = inc.ClosedReason
		}
		view.Incidents = append(view.Incidents, row)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view.Insecure = s.insecure(r)
	view.NoAuth = s.auth.disabled()
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "incidents.html", view)
}

func (s *server) handleIncident(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/annotate") {
		s.handleIncidentAnnotate(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Incidents == nil {
		http.Error(w, "incident log is not configured", http.StatusServiceUnavailable)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/incidents/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "no such incident", http.StatusNotFound)
		return
	}

	inc, attrs, targets, err := s.deps.Incidents.IncidentByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "no such incident", http.StatusNotFound)
		return
	}
	if err != nil {
		s.deps.Logger.Error("incident load failed", "event", "incident_load_failed", "component", "http", "error", err)
		http.Error(w, "incident unavailable", http.StatusServiceUnavailable)
		return
	}

	view := incidentDetailView{
		Insecure: s.insecure(r),
		NoAuth:   s.auth.disabled(),
		ID:       inc.ID, Headline: inc.Headline, Scope: inc.Scope, ScopeValue: inc.ScopeValue,
		OpenedAt:   inc.OpenedAt.UTC().Format("2006-01-02 15:04"),
		Confidence: inc.Confidence, Closed: !inc.ClosedAt.IsZero(),
		Targets: targets,
	}
	if !inc.ClosedAt.IsZero() {
		view.ClosedAt = inc.ClosedAt.UTC().Format("2006-01-02 15:04")
		view.ClosedReason = inc.ClosedReason
	}
	for _, a := range attrs {
		view.Attribution = append(view.Attribution, attributionView{
			Rank: a.Rank, Kind: a.Kind, Value: a.Value,
			Failing: a.Failing, Total: a.Total,
			Share: fmt.Sprintf("%.0f%%", a.Share*100),
			Score: fmt.Sprintf("%.3f", a.Score),
		})
	}

	if s.deps.Corroboration == nil {
		view.CorroborationNote = "Feed corroboration is not configured on this instance."
	} else {
		events, err := s.deps.Corroboration.Corroboration(r.Context(), inc, time.Now().UTC(), 20)
		switch {
		case err != nil:
			// The incident is the page; a failed corroboration query is a
			// missing panel, not a missing incident.
			s.deps.Logger.Warn("corroboration failed",
				"event", "corroboration_failed", "component", "http",
				"incident_id", inc.ID, "error", err)
			view.CorroborationNote = "Feed corroboration is temporarily unavailable."
		case len(events) == 0:
			view.CorroborationNote = "No feed event overlapped this incident."
		default:
			for _, e := range events {
				row := corroborationView{
					Feed: e.Feed, Kind: e.Kind,
					Severity:  fmt.Sprintf("%.2f", e.Severity),
					StartedAt: e.StartedAt.UTC().Format("2006-01-02 15:04"),
				}
				switch {
				case e.Country != "":
					row.Place = e.Country
				case e.ASN > 0:
					row.Place = fmt.Sprintf("AS%d", e.ASN)
				default:
					row.Place = "-"
				}
				if !e.EndedAt.IsZero() {
					row.EndedAt = e.EndedAt.UTC().Format("2006-01-02 15:04")
				}
				view.Corroboration = append(view.Corroboration, row)
			}
		}
	}

	// The probes around the event. They are pinned when the incident opens, so
	// this panel still answers "what did it look like" after the raw 72-hour
	// window is gone (SCHEMA.md §5).
	if s.deps.IncidentProbes == nil {
		view.ArchiveNote = "The probe archive is not configured on this instance."
	} else {
		rows, err := s.deps.IncidentProbes.IncidentProbes(r.Context(), inc.ID, 40)
		switch {
		case err != nil:
			s.deps.Logger.Warn("incident probes failed",
				"event", "incident_probes_failed", "component", "http",
				"incident_id", inc.ID, "error", err)
			view.ArchiveNote = "Archived probes are temporarily unavailable."
		case len(rows) == 0:
			view.ArchiveNote = "No probes were pinned around this incident. The archive starts with the next one; the raw probes are pruned after 72 hours."
		default:
			for _, row := range rows {
				view.Archive = append(view.Archive, toProbeView(row))
			}
		}
	}

	view.Annotation = inc.Annotation
	if !inc.AnnotatedAt.IsZero() {
		view.AnnotatedAt = inc.AnnotatedAt.UTC().Format("2006-01-02 15:04")
	}
	csrf, _ := s.auth.csrfFrom(r)
	view.CSRF = csrf
	view.CanAnnotate = s.deps.Annotator != nil

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view.Insecure = s.insecure(r)
	view.NoAuth = s.auth.disabled()
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "incident.html", view)
}

// handleIncidentAnnotate stores the operator's ground truth from the incident
// page. It is the same write as the API action, behind a CSRF-checked form.
func (s *server) handleIncidentAnnotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Annotator == nil {
		http.Error(w, "annotation is not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil || !s.auth.csrfMatches(r, r.PostFormValue("csrf")) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/incidents/")
	id := strings.TrimSuffix(rest, "/annotate")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "no such incident", http.StatusNotFound)
		return
	}

	text := strings.TrimSpace(r.PostFormValue("annotation"))
	if text == "" || len(text) > 500 {
		http.Error(w, "annotation must be 1-500 characters", http.StatusBadRequest)
		return
	}

	if err := s.deps.Annotator.AnnotateIncident(r.Context(), id, text, time.Now().UTC()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "no such incident", http.StatusNotFound)
			return
		}
		s.deps.Logger.Error("annotation failed",
			"event", "annotation_failed", "component", "http", "incident_id", id, "error", err)
		http.Error(w, "annotation unavailable", http.StatusServiceUnavailable)
		return
	}

	s.deps.Logger.Info("incident annotated",
		"event", "incident_annotated", "component", "http", "incident_id", id)

	// #nosec G710 -- fixed prefix plus an id validated to contain no slash.
	http.Redirect(w, r, "/incidents/"+id, http.StatusSeeOther)
}

// --- coverage --------------------------------------------------------------

type coverageRow struct {
	Kind      string
	Value     string
	Total     int
	Measured  int
	Coverage  string
	CoverageN float64
	// Low marks the middle band from DESIGN.md §7.2: coverage below 0.80, or
	// fewer than 20 measurable targets, is computed but noisy. It is a badge
	// and a hatch, not a second colour scale.
	Low bool
}

type staleRow struct {
	ID        string
	Host      string
	State     string
	LastProbe string
	Age       string
}

type coverageView struct {
	// Insecure marks a plaintext browser connection so the page can say so
	// (NETWORKING.md 7.1, THREAT_MODEL.md T8).
	Insecure      bool
	NoAuth        bool
	Total         int
	Measurable    int
	Unmeasurable  int
	Coverage      string
	Concentration concentrationView
	Classes       []coverageRow
	Sectors       []coverageRow
	BlindSpots    []coverageRow
	Stale         []staleRow
	Trend         []trendRow
	TrendNote     string
}

// concentrationView is the network-spread block from DESIGN.md §10.6. The
// percentages are strings so the template does no arithmetic.
type concentrationView struct {
	ASNs         int
	Countries    int
	TopASN       string
	TopASNShare  string
	Top10Share   string
	SingleAnchor int
	ThinSectors  int
}

// trendRow is one day of coverage drawn as a block bar. It uses characters
// rather than CSS widths because the page's CSP does not allow inline styles.
type trendRow struct {
	Day      string
	Bar      string
	Coverage string
	Measured string
}

var trendBlocks = []rune("▁▂▃▄▅▆▇█")

func toTrendRows(points []store.CoveragePoint) []trendRow {
	out := make([]trendRow, 0, len(points))
	for _, p := range points {
		share := 0.0
		if p.Total > 0 {
			share = float64(p.Measurable) / float64(p.Total)
		}
		idx := int(share * float64(len(trendBlocks)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(trendBlocks) {
			idx = len(trendBlocks) - 1
		}
		// A repeated character makes a readable bar in a proportional font.
		bar := strings.Repeat(string(trendBlocks[idx]), 8)
		out = append(out, trendRow{
			Day:      p.Bucket.UTC().Format("2006-01-02"),
			Bar:      bar,
			Coverage: fmt.Sprintf("%.0f%%", share*100),
			Measured: fmt.Sprintf("%d/%d", p.Measurable, p.Total),
		})
	}
	return out
}

func toCoverageRows(buckets []store.CoverageBucket) []coverageRow {
	out := make([]coverageRow, 0, len(buckets))
	for _, b := range buckets {
		row := coverageRow{
			Kind: b.Kind, Value: b.Value, Total: b.Total, Measured: b.Measurable,
		}
		if b.Total > 0 {
			row.CoverageN = float64(b.Measurable) / float64(b.Total)
			row.Coverage = fmt.Sprintf("%.0f%%", row.CoverageN*100)
			row.Low = row.CoverageN < 0.80 || b.Measurable < 20
		} else {
			row.Coverage = "-"
		}
		if row.Value == "" {
			row.Value = "(none)"
		}
		out = append(out, row)
	}
	return out
}

// handleCoverage renders the blind-spot panel for humans. It is the same data as
// GET /api/v1/coverage: measurement, not health.
func (s *server) handleCoverage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Coverage == nil {
		http.Error(w, "coverage is not configured", http.StatusServiceUnavailable)
		return
	}

	vantage := s.deps.Vantage
	if vantage == "" {
		vantage = "local"
	}

	now := time.Now().UTC()
	cov, err := s.deps.Coverage.Coverage(r.Context(), vantage, now)
	if err != nil {
		s.deps.Logger.Error("coverage read failed", "event", "coverage_failed", "component", "http", "error", err)
		http.Error(w, "coverage unavailable", http.StatusServiceUnavailable)
		return
	}

	view := coverageView{
		Insecure:     s.insecure(r),
		Total:        cov.Totals.Total,
		Measurable:   cov.Totals.Measurable,
		Unmeasurable: cov.Totals.Unmeasurable,
		Concentration: concentrationView{
			ASNs:         cov.Concentration.ASNCount,
			Countries:    cov.Concentration.CountryCount,
			TopASNShare:  fmt.Sprintf("%.1f%%", cov.Concentration.TopASNShare*100),
			Top10Share:   fmt.Sprintf("%.1f%%", cov.Concentration.Top10Share*100),
			SingleAnchor: cov.Concentration.SingleAnchor,
			ThinSectors:  cov.Concentration.ThinSectors,
		},
		Classes:    toCoverageRows(cov.Classes),
		Sectors:    toCoverageRows(cov.Sectors),
		BlindSpots: toCoverageRows(cov.BlindSpots),
		Stale:      make([]staleRow, 0, len(cov.Stale)),
	}
	if cov.Concentration.TopASN > 0 {
		view.Concentration.TopASN = fmt.Sprintf("AS%d", cov.Concentration.TopASN)
		if cov.Concentration.TopASNName != "" {
			view.Concentration.TopASN += " " + cov.Concentration.TopASNName
		}
	}
	if cov.Totals.Total > 0 {
		view.Coverage = fmt.Sprintf("%.0f%%", 100*float64(cov.Totals.Measurable)/float64(cov.Totals.Total))
	}
	for _, st := range cov.Stale {
		row := staleRow{ID: st.ID, Host: st.Host, State: st.State}
		if st.LastProbe.IsZero() {
			row.LastProbe = "never"
			row.Age = "-"
		} else {
			row.LastProbe = st.LastProbe.UTC().Format("2006-01-02 15:04")
			row.Age = now.Sub(st.LastProbe).Round(time.Minute).String()
		}
		view.Stale = append(view.Stale, row)
	}

	if s.deps.CoverageTrend == nil {
		view.TrendNote = "Coverage history is not configured on this instance."
	} else {
		points, err := s.deps.CoverageTrend.CoverageHistory(r.Context(), "global", "", 14)
		if err != nil {
			s.deps.Logger.Warn("coverage history failed",
				"event", "coverage_history_failed", "component", "http", "error", err)
			view.TrendNote = "Coverage history is temporarily unavailable."
		} else if len(points) == 0 {
			view.TrendNote = "No daily snapshots yet; history starts on the first full day."
		} else {
			view.Trend = toTrendRows(points)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view.Insecure = s.insecure(r)
	view.NoAuth = s.auth.disabled()
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "coverage.html", view)
}

// --- world -----------------------------------------------------------------

// worldView is the country-level board (DESIGN.md §10.3). It answers "which
// countries look unhappy?" before the target grid answers "which host?".
type worldView struct {
	// Insecure marks a plaintext browser connection so the page can say so
	// (NETWORKING.md 7.1, THREAT_MODEL.md T8).
	Insecure  bool
	NoAuth    bool
	Version   string
	CSRF      string
	Countries []countryView
	Unplaced  int
	// OverlayNote explains a missing or failed feed overlay, so an empty tile
	// is not mistaken for "no known event".
	OverlayNote string
}

// countryView is one country tile. State is carried as text and glyph as well
// as colour, like every other state surface.
type countryView struct {
	Code     string
	Count    int
	Worst    string
	WorstCls string
	Glyph    string
	Healthy  bool
	Counts   []countView
	Href     string
	// Flag is the regional-indicator pair for the code. It is decorative: the
	// code is always rendered as text, so a platform without flag glyphs loses
	// nothing (DESIGN.md §10.7).
	Flag string
	// Feed is the world-view overlay: the highest-severity external event for
	// this country, labelled with its feed and kind. It is evidence, never a
	// country-wide verdict.
	Feed         string
	FeedSeverity string
}

// flagFor maps an ISO-3166-1 alpha-2 code to its regional-indicator pair. An
// invalid code gets an empty string rather than a guessed flag.
func flagFor(code string) string {
	if len(code) != 2 {
		return ""
	}
	code = strings.ToUpper(code)
	runes := make([]rune, 0, 2)
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return ""
		}
		runes = append(runes, 0x1F1E6+(r-'A'))
	}
	return string(runes)
}

// handleWorld aggregates the board by country. A target with no learned
// country is counted separately rather than folded into a real code, because
// "unknown" is not a country.
func (s *server) handleWorld(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	targets, err := s.deps.Source.Snapshot(r.Context())
	if err != nil {
		s.deps.Logger.Error("world snapshot failed", "event", "world_failed", "component", "http", "error", err)
		http.Error(w, "board unavailable", http.StatusServiceUnavailable)
		return
	}

	byCountry := make(map[string][]board.Target)
	unplaced := 0
	for _, t := range targets {
		code := strings.ToUpper(strings.TrimSpace(t.Country))
		if code == "" {
			unplaced++
			continue
		}
		byCountry[code] = append(byCountry[code], t)
	}

	csrf, _ := s.auth.csrfFrom(r)
	view := worldView{Version: s.deps.Version, CSRF: csrf, Unplaced: unplaced, Insecure: s.insecure(r)}

	// The overlay is one query for the whole page. A missing reader or a failed
	// query is stated, never rendered as "no known event".
	overlay := map[string]store.ExternalEvent{}
	if s.deps.CountryEvents == nil {
		view.OverlayNote = "Feed overlay is not configured on this instance."
	} else {
		events, err := s.deps.CountryEvents.RecentCountryEvents(
			r.Context(), time.Now().UTC().Add(-24*time.Hour))
		switch {
		case err != nil:
			s.deps.Logger.Warn("world overlay failed",
				"event", "world_overlay_failed", "component", "http", "error", err)
			view.OverlayNote = "Feed overlay is temporarily unavailable."
		case len(events) == 0:
			view.OverlayNote = "No country-scoped feed event in the last 24 hours."
		default:
			overlay = events
		}
	}

	for code, members := range byCountry {
		counts := make(map[board.State]int)
		worst := board.StateUp
		healthy := true
		for _, t := range members {
			counts[t.State]++
			if board.SeverityRank(t.State) < board.SeverityRank(worst) {
				worst = t.State
			}
			if t.State != board.StateUp && t.State != board.StateUpReachability {
				healthy = false
			}
		}

		row := countryView{
			Code:     code,
			Count:    len(members),
			Worst:    worst.Text(),
			WorstCls: worst.CSSClass(),
			Glyph:    worst.Glyph(),
			Healthy:  healthy,
			Flag:     flagFor(code),
			Href:     "/?group=country&country=" + url.QueryEscape(code),
		}
		for _, st := range []board.State{
			board.StateDown, board.StateDegraded, board.StateBlocked,
			board.StateUnknown, board.StateConfigError, board.StateUnmeasurable,
			board.StateUpReachability, board.StateUp,
		} {
			if n := counts[st]; n > 0 {
				row.Counts = append(row.Counts, countView{
					State: string(st), Label: st.Text(), Glyph: st.Glyph(),
					Class: st.CSSClass(), Count: n,
				})
			}
		}
		if event, ok := overlay[code]; ok {
			row.Feed = event.Feed + " " + event.Kind
			row.FeedSeverity = fmt.Sprintf("%.2f", event.Severity)
		}
		view.Countries = append(view.Countries, row)
	}

	sort.SliceStable(view.Countries, func(i, j int) bool {
		ri, rj := board.SeverityRank(worstStateFromClass(view.Countries[i].WorstCls)),
			board.SeverityRank(worstStateFromClass(view.Countries[j].WorstCls))
		if ri != rj {
			return ri < rj
		}
		return view.Countries[i].Code < view.Countries[j].Code
	})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view.Insecure = s.insecure(r)
	view.NoAuth = s.auth.disabled()
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "world.html", view)
}

// worstStateFromClass reverses the CSS class a country tile carries. It keeps
// the ordering logic in one place instead of storing a second enum on the view.
func worstStateFromClass(class string) board.State {
	for _, st := range []board.State{
		board.StateDown, board.StateDegraded, board.StateBlocked,
		board.StateUnknown, board.StateConfigError, board.StateUnmeasurable,
		board.StateUpReachability, board.StateUp,
	} {
		if st.CSSClass() == class {
			return st
		}
	}
	return board.StateUnknown
}

// --- corridors -------------------------------------------------------------

// corridorsView is the corridor table (DESIGN.md §9/§10.4). It shows the newest
// sample and the open step change, not a sparkline: the detection rule is the
// product, and a sparkline without a baseline would invite the wrong question.
type corridorsView struct {
	// Insecure marks a plaintext browser connection so the page can say so
	// (NETWORKING.md 7.1, THREAT_MODEL.md T8).
	Insecure   bool
	NoAuth     bool
	Version    string
	CSRF       string
	Rows       []corridorRow
	Note       string
	SeriesNote string
}

type corridorRow struct {
	ID       string
	From     string
	To       string
	RTT      string
	Baseline string
	Spark    string
	// Band marks each spark bucket against the six-hour baseline: · inside,
	// ▲ above, ▼ below (DESIGN.md §10.4).
	Band      string
	Loss      string
	LossBar   string
	SampledAt string
	State     string
	StateCls  string
	Kind      string
	Detail    string
	Since     string
	// Trace is the one walk captured when the event opened, if any.
	Trace string
}

func (s *server) handleCorridors(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	csrf, _ := s.auth.csrfFrom(r)
	view := corridorsView{Version: s.deps.Version, CSRF: csrf, Insecure: s.insecure(r)}

	if s.deps.Corridors == nil {
		view.Note = "Corridors are not configured on this instance."
	} else {
		statuses, err := s.deps.Corridors.CorridorStatus(r.Context())
		switch {
		case err != nil:
			s.deps.Logger.Warn("corridor page failed",
				"event", "corridor_page_failed", "component", "http", "error", err)
			view.Note = "Corridors are temporarily unavailable."
		case len(statuses) == 0:
			view.Note = "No corridors are configured. Add a `corridors:` section to the target file."
		default:
			series, seriesErr := s.deps.Corridors.CorridorSeries(
				r.Context(), time.Now().UTC().Add(-24*time.Hour))
			if seriesErr != nil {
				s.deps.Logger.Warn("corridor series failed",
					"event", "corridor_series_failed", "component", "http", "error", seriesErr)
				view.SeriesNote = "24-hour shapes are temporarily unavailable."
				series = nil
			}

			for _, status := range statuses {
				samples := series[status.Corridor.ID]
				spark, band := corridorShape(samples)
				row := corridorRow{
					ID: status.Corridor.ID, From: status.Corridor.From, To: status.Corridor.To,
					State: "waiting for a sample", StateCls: "state-unknown",
					RTT: "-", Baseline: baselineOf(samples), Spark: spark, Band: band,
					Loss: "-", LossBar: lossBarOf(0),
				}
				if status.Latest != nil {
					row.SampledAt = status.Latest.TS.UTC().Format("2006-01-02 15:04")
					row.RTT = fmt.Sprintf("%.0f ms", status.Latest.RTTMS)
					row.Loss = fmt.Sprintf("%.0f%%", status.Latest.Loss)
					row.LossBar = lossBarOf(status.Latest.Loss)
					if status.Latest.OK {
						row.State = "normal"
						row.StateCls = "state-up"
					} else {
						row.State = "no reply"
						row.StateCls = "state-down"
					}
				}
				if status.Open != nil {
					row.State = "step change"
					row.StateCls = "state-down"
					row.Kind = status.Open.Kind
					row.Detail = status.Open.Detail
					row.Trace = status.Open.Trace
					row.Since = status.Open.StartedAt.UTC().Format("2006-01-02 15:04")
				}
				view.Rows = append(view.Rows, row)
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view.Insecure = s.insecure(r)
	view.NoAuth = s.auth.disabled()
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "corridors.html", view)
}

// diagnosticsView is the in-app logging inspection page (LOGGING.md §9). The
// release image has no shell, so this is the only way to see the ring, the
// backpressure counters, and the debug scopes that are still on.
type diagnosticsView struct {
	// Insecure marks a plaintext browser connection so the page can say so
	// (NETWORKING.md 7.1, THREAT_MODEL.md T8).
	Insecure    bool
	NoAuth      bool
	Version     string
	CSRF        string
	Note        string
	Stats       []metaRow
	Scopes      []diagnosticsScope
	Records     []diagnosticsRecord
	RecordsNote string
}

type diagnosticsScope struct {
	ID       string
	Selector string
	Level    string
	Expires  string
}

type diagnosticsRecord struct {
	Time    string
	Level   string
	Message string
	Attrs   string
}

// ihiView is the /ihi page: the number, its parts, and what was left out.
type ihiView struct {
	// Insecure marks a plaintext browser connection so the page can say so
	// (NETWORKING.md 7.1, THREAT_MODEL.md T8).
	Insecure bool
	// NoAuth marks an instance running without a token. The page says so on
	// every render: an unauthenticated board is readable and actionable by
	// anyone who can reach it (NETWORKING.md 7.1).
	NoAuth  bool
	Version string
	CSRF    string
	Health  healthView
	Note    string
}

// handleIHI is the page behind the board's index chip. It carries the same
// number plus the weights, timestamps, and dropped components, because
// DESIGN.md §11 says the components are always one click away.
func (s *server) handleIHI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	csrf, _ := s.auth.csrfFrom(r)
	view := ihiView{Version: s.deps.Version, CSRF: csrf, Insecure: s.insecure(r)}

	if s.deps.Health == nil {
		view.Note = "The health index is not configured on this instance."
	} else {
		view.Health = toHealthView(s.deps.Health.Build(r.Context()))
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view.Insecure = s.insecure(r)
	view.NoAuth = s.auth.disabled()
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "ihi.html", view)
}

func (s *server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	csrf, _ := s.auth.csrfFrom(r)
	view := diagnosticsView{Version: s.deps.Version, CSRF: csrf, Insecure: s.insecure(r)}

	if s.deps.Logs == nil {
		view.Note = "The log ring is not wired on this instance."
	} else {
		d := s.deps.Logs.Diagnostics()
		view.Stats = []metaRow{
			{"Log level", d.Level},
			{"Log format", d.Format},
			{"Log file", displayOr(d.File, "console only")},
			{"Ring", fmt.Sprintf("%d of %d records", d.RingRecords, d.RingCapacity)},
			{"Dropped lines", fmt.Sprintf("%d", d.Dropped)},
			{"Write errors", fmt.Sprintf("%d", d.WriteErrors)},
			{"Active debug scopes", fmt.Sprintf("%d", d.Scopes)},
			{"Log streams", fmt.Sprintf("%d", d.Streams)},
		}

		for _, scope := range s.deps.Logs.ActiveScopes() {
			view.Scopes = append(view.Scopes, diagnosticsScope{
				ID:       scope.ID,
				Selector: scopeSelector(scope),
				Level:    scope.Level,
				Expires:  scope.ExpiresAt.UTC().Format("2006-01-02 15:04"),
			})
		}

		records := s.deps.Logs.LogRecords(slog.LevelDebug, "", 100)
		if len(records) == 0 {
			view.RecordsNote = "The ring holds no records at debug or above."
		}
		// Newest first: the line that explains the failure is almost always the
		// last one written.
		for i := len(records) - 1; i >= 0; i-- {
			view.Records = append(view.Records, toDiagnosticsRecord(records[i]))
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view.Insecure = s.insecure(r)
	view.NoAuth = s.auth.disabled()
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "diagnostics.html", view)
}

// scopeSelector renders the selectors a scope names. A scope always names at
// least one; "everything" is not expressible (LOGGING.md §6).
func scopeSelector(scope logging.Scope) string {
	parts := make([]string, 0, 4)
	if scope.Target != "" {
		parts = append(parts, "target="+scope.Target)
	}
	if scope.Vantage != "" {
		parts = append(parts, "vantage="+scope.Vantage)
	}
	if scope.Component != "" {
		parts = append(parts, "component="+scope.Component)
	}
	if scope.ErrorClass != "" {
		parts = append(parts, "error_class="+scope.ErrorClass)
	}
	return strings.Join(parts, ", ")
}

// toDiagnosticsRecord flattens one ring record for display. Attributes are
// sorted so the same line always renders the same way.
func toDiagnosticsRecord(rec logging.Record) diagnosticsRecord {
	keys := make([]string, 0, len(rec.Attrs))
	for key := range rec.Attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+rec.Attrs[key])
	}
	return diagnosticsRecord{
		Time:    rec.Time.UTC().Format("15:04:05.000"),
		Level:   rec.LevelName,
		Message: rec.Message,
		Attrs:   strings.Join(parts, " "),
	}
}

// rollupSpark renders up to 24 hourly buckets as block characters by healthy
// share. An empty bucket is drawn mid-scale rather than as a gap: a missing
// hour is "no probes", not zero availability.
func rollupSpark(points []store.Rollup) string {
	if len(points) < 2 {
		return ""
	}

	shares := make([]float64, 0, len(points))
	for _, p := range points {
		if p.N <= 0 {
			shares = append(shares, 0.5)
			continue
		}
		shares = append(shares, float64(p.Up)/float64(p.N))
	}

	const columns = 24
	if len(shares) > columns {
		buckets := make([]float64, 0, columns)
		size := float64(len(shares)) / columns
		for i := 0; i < columns; i++ {
			start := int(float64(i) * size)
			end := int(float64(i+1) * size)
			if end <= start {
				end = start + 1
			}
			if end > len(shares) {
				end = len(shares)
			}
			sum := 0.0
			for _, v := range shares[start:end] {
				sum += v
			}
			buckets = append(buckets, sum/float64(end-start))
		}
		shares = buckets
	}

	blocks := []rune("▁▂▃▄▅▆▇█")
	var out strings.Builder
	for _, share := range shares {
		index := int(math.Round(share * float64(len(blocks)-1)))
		if index < 0 {
			index = 0
		}
		if index >= len(blocks) {
			index = len(blocks) - 1
		}
		out.WriteRune(blocks[index])
	}
	return out.String()
}

// corridorBaselineWindow is the six-hour window the corridor detection rule
// compares against (DESIGN.md §9).
const corridorBaselineWindow = 6 * time.Hour

// sparkColumns is how many buckets a corridor shape may draw.
const sparkColumns = 24

// successfulRTTs keeps the samples that measured something. A failure has no
// RTT, and pretending it had one would bend the shape.
func successfulRTTs(samples []store.CorridorSample) []float64 {
	rtts := make([]float64, 0, len(samples))
	for _, s := range samples {
		if s.OK {
			rtts = append(rtts, s.RTTMS)
		}
	}
	return rtts
}

// downsample averages values into at most columns buckets.
func downsample(values []float64, columns int) []float64 {
	if len(values) <= columns {
		return values
	}
	out := make([]float64, 0, columns)
	size := float64(len(values)) / float64(columns)
	for i := 0; i < columns; i++ {
		start := int(float64(i) * size)
		end := int(float64(i+1) * size)
		if end <= start {
			end = start + 1
		}
		if end > len(values) {
			end = len(values)
		}
		sum := 0.0
		for _, v := range values[start:end] {
			sum += v
		}
		out = append(out, sum/float64(end-start))
	}
	return out
}

// sparkFromBuckets renders buckets as block characters. Fewer than two have no
// shape to draw, and a fabricated one would be worse than an empty cell.
func sparkFromBuckets(buckets []float64) string {
	if len(buckets) < 2 {
		return ""
	}

	low, high := buckets[0], buckets[0]
	for _, v := range buckets {
		if v < low {
			low = v
		}
		if v > high {
			high = v
		}
	}

	blocks := []rune("▁▂▃▄▅▆▇█")
	var out strings.Builder
	for _, v := range buckets {
		if high == low {
			out.WriteRune(blocks[len(blocks)/2])
			continue
		}
		index := int(math.Round((v - low) / (high - low) * float64(len(blocks)-1)))
		if index < 0 {
			index = 0
		}
		if index >= len(blocks) {
			index = len(blocks) - 1
		}
		out.WriteRune(blocks[index])
	}
	return out.String()
}

// medianAndSigma returns the median and the sample standard deviation.
func medianAndSigma(values []float64) (median, sigma float64) {
	if len(values) < 2 {
		return 0, 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	median = sorted[mid]
	if len(sorted)%2 == 0 {
		median = (sorted[mid-1] + sorted[mid]) / 2
	}
	sum := 0.0
	for _, v := range values {
		d := v - median
		sum += d * d
	}
	return median, math.Sqrt(sum / float64(len(values)-1))
}

// corridorShape renders the 24-hour RTT shape and the baseline band markers.
//
// The band is the six-hour window the detection rule compares against, with the
// same material floor the rule uses, drawn on the same buckets as the
// sparkline: '·' inside, '▲' above, '▼' below. The newest bucket is excluded
// from its own band, mirroring the detector's two-sample persistence: a spike
// must not widen the band that is supposed to catch it.
func corridorShape(samples []store.CorridorSample) (spark, band string) {
	buckets := downsample(successfulRTTs(samples), sparkColumns)
	if len(buckets) < 2 {
		return "", ""
	}
	spark = sparkFromBuckets(buckets)

	ref := samples[len(samples)-1].TS
	cutoff := ref.Add(-corridorBaselineWindow)
	newest := len(samples) / sparkColumns
	if newest < 1 {
		newest = 1
	}
	limit := len(samples) - newest

	window := make([]float64, 0, len(samples))
	for i := 0; i < limit; i++ {
		s := samples[i]
		if s.OK && !s.TS.Before(cutoff) {
			window = append(window, s.RTTMS)
		}
	}
	if len(window) < 2 {
		return spark, ""
	}

	median, sigma := medianAndSigma(window)
	margin := 3 * sigma
	if floor := 0.10 * median; floor > margin {
		margin = floor
	}

	var out strings.Builder
	for _, v := range buckets {
		switch {
		case v > median+margin:
			out.WriteRune('▲')
		case v < median-margin:
			out.WriteRune('▼')
		default:
			out.WriteRune('·')
		}
	}
	return spark, out.String()
}

// lossBarOf renders a five-cell bar; the percentage beside it is the text, so
// colour is never the only carrier.
func lossBarOf(loss float64) string {
	const cells = 5
	filled := int(math.Round(loss / 100 * cells))
	if filled < 0 {
		filled = 0
	}
	if filled > cells {
		filled = cells
	}
	return strings.Repeat("▰", filled) + strings.Repeat("▱", cells-filled)
}

// baselineOf reports the median successful RTT over the window, which is the
// number the step rule compares against.
func baselineOf(samples []store.CorridorSample) string {
	rtts := make([]float64, 0, len(samples))
	for _, s := range samples {
		if s.OK {
			rtts = append(rtts, s.RTTMS)
		}
	}
	if len(rtts) == 0 {
		return "-"
	}
	sort.Float64s(rtts)
	mid := len(rtts) / 2
	median := rtts[mid]
	if len(rtts)%2 == 0 {
		median = (rtts[mid-1] + rtts[mid]) / 2
	}
	return fmt.Sprintf("%.0f ms", median)
}

// --- events ----------------------------------------------------------------

type stateEvent struct {
	Target string `json:"target"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Text   string `json:"state_text"`
	Glyph  string `json:"glyph"`
	Class  string `json:"class"`
	Since  string `json:"since,omitempty"`
}

// stateEventFor is the one wire shape for a tile's state, used by both the
// connect snapshot and every delta so the client has a single parser.
func stateEventFor(t board.Target) stateEvent {
	since := ""
	if !t.Since.IsZero() {
		since = t.Since.UTC().Format("15:04 MST")
	}
	return stateEvent{
		Target: t.Host, Name: t.Name, State: string(t.State),
		Text: t.State.Text(), Glyph: t.State.Glyph(),
		Class: t.State.CSSClass(), Since: since,
	}
}

// snapshotEvent is the one full state a connection receives, immediately after
// the preamble. Deltas follow it; a change is never amplified into a whole
// grid, but the grid is sent once because the page was rendered before this
// stream existed.
type snapshotEvent struct {
	Targets []stateEvent `json:"targets"`
}

// logEvent is one WARN-or-above line on the UI stream. The full ring stays in
// the diagnostics page; this is the "something to notice right now" feed.
type logEvent struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// handleEvents streams one state snapshot per connection and then deltas only.
// The page renders the full board, but that render and this connection are not
// atomic: a change in the gap would otherwise be inside the baseline and never
// shown, leaving the page stale until the next change. The snapshot is bounded
// by connections, not by changes, so one event still cannot amplify into a
// whole grid.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	release, ok := s.streams.Acquire(s.clientIP(r))
	if !ok {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many concurrent event streams", http.StatusTooManyRequests)
		return
	}
	defer release()

	// The server sets WriteTimeout for normal requests; SSE outlives it.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	// Subscribe before the preamble is written. Writing "retry:" first would
	// let a change land in the gap between the client seeing the preamble and
	// this handler subscribing, which is a lost wake-up rather than a slow one.
	var ch <-chan struct{}
	watcher := s.deps.Watcher
	if watcher != nil {
		boardCh, cancel := watcher.Subscribe()
		defer cancel()
		ch = boardCh
	}

	var incidentCh <-chan struct{}
	if s.deps.IncidentWatcher != nil {
		incCh, incCancel := s.deps.IncidentWatcher.Subscribe()
		defer incCancel()
		incidentCh = incCh
	}

	// Log lines ride the same stream but are best-effort: a slow reader drops
	// lines, never state (LOGGING.md §9).
	var logCh <-chan logging.Record
	if s.deps.Logs != nil {
		ch, stop := s.deps.Logs.Subscribe()
		defer stop()
		logCh = ch
	}

	// Capture the baseline at subscription time, before the preamble is
	// visible. Taking it after the flush lets a change that lands between the
	// client reading "retry:" and this snapshot be folded into the baseline and
	// never sent: a lost wake-up, not a slow one.
	var last map[string]board.Target
	if watcher != nil {
		last = s.stateMap(r.Context())
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if _, err := io.WriteString(w, "retry: 3000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	if watcher == nil {
		s.heartbeatOnly(w, r, flusher)
		return
	}

	// One snapshot per connection, then deltas only. The page HTML was
	// rendered before this stream existed, so a change in that gap would
	// otherwise be inside the baseline and never shown; the page would stay
	// stale until the next change. This is bounded by connections, never by
	// changes, so a diff still cannot amplify one event into a whole grid.
	ids := make([]string, 0, len(last))
	for id := range last {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	events := make([]stateEvent, 0, len(ids))
	for _, id := range ids {
		events = append(events, stateEventFor(last[id]))
	}
	if data, err := json.Marshal(snapshotEvent{Targets: events}); err == nil {
		if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data); err != nil {
			return
		}
		flusher.Flush()
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-incidentCh:
			// The incident pages reload on this event rather than patching rows
			// client-side: the table is small, and a reload cannot show a stale
			// attribution or close reason.
			data, err := json.Marshal(map[string]string{
				"updated_at": time.Now().UTC().Format(time.RFC3339),
			})
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: incident\ndata: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case rec, ok := <-logCh:
			if !ok {
				logCh = nil
				continue
			}
			// WARN and above: an INFO stream would bury the state deltas.
			if rec.Level < slog.LevelWarn {
				continue
			}
			data, err := json.Marshal(logEvent{
				Time:    rec.Time.UTC().Format("15:04:05"),
				Level:   rec.LevelName,
				Message: rec.Message,
			})
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: log\ndata: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-ch:
			current := s.stateMap(r.Context())
			ids := make([]string, 0, len(current))
			for id := range current {
				ids = append(ids, id)
			}
			sort.Strings(ids)

			for _, id := range ids {
				t := current[id]
				if prev, seen := last[id]; seen && prev.State == t.State {
					continue
				}
				data, err := json.Marshal(stateEventFor(t))
				if err != nil {
					continue
				}
				if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data); err != nil {
					return
				}
			}
			last = current
			flusher.Flush()
		}
	}
}

func (s *server) heartbeatOnly(w http.ResponseWriter, r *http.Request, flusher http.Flusher) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// clientIP is the address a per-client limit and a log line should name. It is
// the peer unless the peer is inside a configured trusted proxy, in which case
// the rightmost untrusted X-Forwarded-For entry wins (NETWORKING.md §7.3).
// Without configured proxies the header is ignored, because a spoofable header
// must not multiply a client's budget.
func (s *server) clientIP(r *http.Request) string {
	ip := netx.ClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), s.deps.TrustedProxies)
	if ip == "" {
		return "unknown"
	}
	return ip
}

// insecure reports whether the browser's connection to this instance is
// plaintext. There is no TLS in the binary, so a non-TLS request is the
// ordinary case; a TLS-terminating proxy supplies X-Forwarded-Proto, which is
// believed only from a configured trusted proxy (NETWORKING.md §7.1, T8).
func (s *server) insecure(r *http.Request) bool {
	if r.TLS != nil {
		return false
	}
	if netx.TrustedPeer(r.RemoteAddr, s.deps.TrustedProxies) {
		proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
		if strings.EqualFold(proto, "https") {
			return false
		}
	}
	return true
}

func (s *server) stateMap(ctx context.Context) map[string]board.Target {
	targets, err := s.deps.Source.Snapshot(ctx)
	if err != nil {
		return map[string]board.Target{}
	}
	out := make(map[string]board.Target, len(targets))
	for _, t := range targets {
		out[t.ID] = t
	}
	return out
}
