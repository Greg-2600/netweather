// Package calendar loads the operator's atypical-day calendar (DESIGN.md §7.2).
//
// The file is deliberately local and static: it is never fetched at runtime,
// because a calendar that depends on a third party turns a detector into a
// network dependency. An absent file means "no calendar", not an error.
package calendar

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DateLayout is the only accepted date shape. A time zone is deliberately not
// accepted: a calendar day is a UTC day, the same key the baseline uses.
const DateLayout = "2006-01-02"

// Entry is one atypical day. Regions narrow the exclusion to country scopes;
// an empty list applies the day to every scope.
type Entry struct {
	Date    string   `yaml:"date"`
	Regions []string `yaml:"regions"`
}

// File is the calendar document.
type File struct {
	Dates []Entry `yaml:"dates"`
}

// Load reads and validates a calendar file. An absent file is reported as
// (nil, nil): no calendar is a normal deployment, not a startup failure.
func Load(path string) ([]Entry, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied NETWEATHER_CALENDAR.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("calendar: %w", err)
	}
	return Parse(raw)
}

// Parse validates calendar YAML. Unknown fields are refused, because a typo
// that silently does nothing is worse than a startup error.
func Parse(raw []byte) ([]Entry, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("calendar: %w", err)
	}

	out := make([]Entry, 0, len(f.Dates))
	seen := make(map[string]struct{}, len(f.Dates))
	for _, e := range f.Dates {
		day := strings.TrimSpace(e.Date)
		if _, err := time.Parse(DateLayout, day); err != nil {
			return nil, fmt.Errorf("calendar: %q is not a YYYY-MM-DD date", e.Date)
		}
		if _, ok := seen[day]; ok {
			return nil, fmt.Errorf("calendar: %s is listed twice", day)
		}
		seen[day] = struct{}{}

		regions := make([]string, 0, len(e.Regions))
		regionSeen := make(map[string]struct{}, len(e.Regions))
		for _, region := range e.Regions {
			code := strings.ToUpper(strings.TrimSpace(region))
			if len(code) != 2 {
				return nil, fmt.Errorf("calendar: %s: region %q is not a two-letter code", day, region)
			}
			if _, ok := regionSeen[code]; ok {
				continue
			}
			regionSeen[code] = struct{}{}
			regions = append(regions, code)
		}
		sort.Strings(regions)
		out = append(out, Entry{Date: day, Regions: regions})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out, nil
}

// Scopes returns the (scope, scopeValue) pairs a day's exclusion applies to.
// An entry without regions applies to every scope, represented by the empty
// pair. A region applies to that country's baseline only.
func (e Entry) Scopes() [][2]string {
	if len(e.Regions) == 0 {
		return [][2]string{{"", ""}}
	}
	out := make([][2]string, 0, len(e.Regions))
	for _, region := range e.Regions {
		out = append(out, [2]string{"country", region})
	}
	return out
}
