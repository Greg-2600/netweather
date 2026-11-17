package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// loadDir implements the documented config directory (DESIGN.md §13.2):
//
//	netweather.yaml      optional: version, extra targets, corridors
//	sites/<sector>/*.txt one domain per line; the directory path is the sector
//	overrides/*.yaml      per-host changes for targets that need real checks
//	corridors.yaml       optional path pairs
//
// The directory form exists so a large list is a directory tree rather than one
// file, and so the sector is the folder rather than a tag an operator has to
// repeat on every line. A single file still works and is still the documented
// default in the README.
func loadDir(root string) (*File, error) {
	out := &File{Version: CurrentVersion}

	// netweather.yaml is optional and may carry targets or corridors that do
	// not belong to a sector file. Its runtime settings stay environment
	// variables: a second source of truth for the listen address is how two
	// configs drift.
	if f, err := readFileDoc(filepath.Join(root, "netweather.yaml")); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	} else {
		if f.Version == 0 {
			f.Version = CurrentVersion
		}
		if f.Version != CurrentVersion {
			return nil, fmt.Errorf("config: %s: version %d is not supported (want %d)",
				filepath.Join(root, "netweather.yaml"), f.Version, CurrentVersion)
		}
		out.Targets = append(out.Targets, f.Targets...)
		out.Corridors = append(out.Corridors, f.Corridors...)
	}

	siteTargets, err := loadSites(filepath.Join(root, "sites"))
	if err != nil {
		return nil, err
	}
	out.Targets = append(out.Targets, siteTargets...)

	if err := applyOverrides(filepath.Join(root, "overrides"), out.Targets); err != nil {
		return nil, err
	}

	if f, err := readFileDoc(filepath.Join(root, "corridors.yaml")); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	} else {
		out.Corridors = append(out.Corridors, f.Corridors...)
	}

	if len(out.Targets) == 0 {
		return nil, fmt.Errorf("config: %s has no targets (add sites/*.txt or targets in netweather.yaml)", root)
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// loadSites reads every sites/**/*.txt file. The path under sites/ is the
// sector, so financial/banking.txt becomes sector financial/banking without a
// tag to maintain.
func loadSites(root string) ([]Target, error) {
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: %w", err)
	}

	var out []Target
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".txt") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > MaxFileBytes {
			return fmt.Errorf("config: %s is %d bytes; limit is %d", path, info.Size(), MaxFileBytes)
		}
		raw, err := os.ReadFile(path) // #nosec G304,G122 -- inside the operator's config directory.
		if err != nil {
			return err
		}

		// The path under sites/, without the extension, is the sector: the
		// documented financial/banking.txt is sector financial/banking, not a
		// container of banking domains. One file is one sub-sector.
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sector := strings.TrimSuffix(rel, filepath.Ext(rel))
		sector = strings.ReplaceAll(sector, string(filepath.Separator), "/")
		if sector == "." {
			sector = ""
		}

		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			out = append(out, Target{
				Name:      line,
				Host:      line,
				Port:      443,
				Probe:     "http",
				Class:     "service",
				Sector:    sector,
				Tier:      2,
				IntervalS: 300,
				// A sector list is a "does it answer" list, so any success or
				// redirect counts. A target that needs a specific code gets an
				// override with expect.status.
				Expect: Expect{StatusMin: 200, StatusMax: 399},
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("config: sites: %w", err)
	}
	return out, nil
}

// override is the subset of a target an operator changes in overrides/*.yaml.
// Tags are not part of the shipped schema, so the decoder rejects them rather
// than accepting a file whose contents are ignored.
type override struct {
	Target    string  `yaml:"target"`
	Name      string  `yaml:"name"`
	Sector    string  `yaml:"sector"`
	Probe     string  `yaml:"probe"`
	Interval  string  `yaml:"interval"`
	StatusURL string  `yaml:"status_url"`
	Expect    *Expect `yaml:"expect"`
}

func applyOverrides(dir string, targets []Target) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: overrides: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".yaml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path) // #nosec G304,G122 -- inside the operator's config directory.
		if err != nil {
			return fmt.Errorf("config: %s: %w", path, err)
		}
		if len(raw) > MaxFileBytes {
			return fmt.Errorf("config: %s is %d bytes; limit is %d", path, len(raw), MaxFileBytes)
		}

		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		var o override
		if err := dec.Decode(&o); err != nil {
			return fmt.Errorf("config: %s: %w", path, err)
		}

		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(o.Target), "."))
		if host == "" {
			return fmt.Errorf("config: %s: target is required", path)
		}
		index := -1
		for i := range targets {
			if strings.EqualFold(targets[i].Host, host) {
				index = i
				break
			}
		}
		if index < 0 {
			return fmt.Errorf("config: %s: target %q is not in any sites file or netweather.yaml", path, host)
		}

		t := &targets[index]
		if o.Name != "" {
			t.Name = o.Name
		}
		if o.Sector != "" {
			t.Sector = o.Sector
		}
		if o.Probe != "" {
			t.Probe = o.Probe
		}
		if o.StatusURL != "" {
			t.StatusURL = o.StatusURL
		}
		if o.Interval != "" {
			d, err := time.ParseDuration(o.Interval)
			if err != nil || d <= 0 {
				return fmt.Errorf("config: %s: interval %q is not a positive duration", path, o.Interval)
			}
			t.IntervalS = int(d / time.Second)
		}
		if o.Expect != nil {
			t.Expect = *o.Expect
		}
	}
	return nil
}

// readFileDoc decodes one YAML document without validating it, because a
// directory load validates the merged result once every part is in place.
func readFileDoc(path string) (*File, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied config path.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fs.ErrNotExist
		}
		return nil, fmt.Errorf("config: %w", err)
	}
	if len(raw) > MaxFileBytes {
		return nil, fmt.Errorf("config: %s is %d bytes; limit is %d", path, len(raw), MaxFileBytes)
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("config: %s: multiple YAML documents are not supported", path)
	}
	return &f, nil
}
