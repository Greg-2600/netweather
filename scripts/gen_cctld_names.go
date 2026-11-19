//go:build ignore

// gen_cctld_names rebuilds internal/seed/cctld_names.json from two public,
// authoritative sources:
//
//   - IANA's root zone database (https://www.iana.org/domains/root/db/<tld>.html)
//     for internationalised TLDs: the ISO country code, English translation and
//     transliteration of the label;
//   - Unicode CLDR, via golang.org/x/text/language/display, for the English
//     names of the two-letter ISO 3166-1 codes.
//
// The TLD list is read from config/fieldtrial-targets.yaml, so the table always
// covers exactly the fleet we ship. The output is committed and embedded at
// runtime; this script is only needed when the delegation set changes.
//
// Run from the repository root:
//
//	go run scripts/gen_cctld_names.go
package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/idna"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

const ianaPage = "https://www.iana.org/domains/root/db/%s.html"

var (
	tldLine = regexp.MustCompile(`(?m)^  - id: cctld-([a-z0-9-]+)$`)
	col4    = regexp.MustCompile(`(?s)<div class="col-4">(.*?)</div>`)
	tag     = regexp.MustCompile(`<[^>]+>`)
)

type ianaInfo struct {
	Country         string
	Translation     string
	Transliteration string
}

func field(page, label string) string {
	i := strings.Index(page, label+"</span>")
	if i < 0 {
		return ""
	}
	m := col4.FindStringSubmatch(page[i : i+1200])
	if len(m) == 0 {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(tag.ReplaceAllString(m[1], "")))
}

func fetch(client *http.Client, tld string) (ianaInfo, error) {
	url := fmt.Sprintf(ianaPage, tld)
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK {
				page := string(body)
				return ianaInfo{
					Country:         field(page, "Country Code"),
					Translation:     field(page, "Translation"),
					Transliteration: field(page, "Transliteration"),
				}, nil
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if attempt == 2 {
			return ianaInfo{}, fmt.Errorf("IANA %s: %w", tld, err)
		}
		time.Sleep(time.Second)
	}
	return ianaInfo{}, nil
}

func title(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func regionName(code string) string {
	if code == "" {
		return ""
	}
	r, err := language.ParseRegion(code)
	if err != nil || r.String() == "" {
		return ""
	}
	return display.English.Regions().Name(r)
}

func main() {
	cfg, err := os.ReadFile("config/fieldtrial-targets.yaml")
	if err != nil {
		panic(err)
	}
	matches := tldLine.FindAllStringSubmatch(string(cfg), -1)
	if len(matches) == 0 {
		panic("no ccTLD anchors in fieldtrial config")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	names := make(map[string]string, len(matches))
	for _, m := range matches {
		tld := m[1]
		if !strings.HasPrefix(tld, "xn--") {
			if name := regionName(strings.ToUpper(tld)); name != "" {
				names[tld] = name
			} else {
				names[tld] = strings.ToUpper(tld)
			}
			continue
		}
		info, err := fetch(client, tld)
		if err != nil {
			panic(err)
		}
		unicodeLabel, err := idna.ToUnicode(tld)
		if err != nil {
			unicodeLabel = ""
		}
		switch {
		case regionName(info.Country) != "" && unicodeLabel != "":
			names[tld] = regionName(info.Country) + " (" + unicodeLabel + ")"
		case regionName(info.Country) != "":
			names[tld] = regionName(info.Country)
		case info.Translation != "" && unicodeLabel != "":
			names[tld] = title(info.Translation) + " (" + unicodeLabel + ")"
		case info.Transliteration != "" && unicodeLabel != "":
			names[tld] = title(info.Transliteration) + " (" + unicodeLabel + ")"
		default:
			names[tld] = strings.ToUpper(tld)
		}
	}
	// .su is still delegated but has no CLDR region name; keep it readable.
	if _, ok := names["su"]; ok && names["su"] == "SU" {
		names["su"] = "Soviet Union"
	}
	keys := make([]string, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(keys))
	for _, k := range keys {
		ordered[k] = names[k]
	}
	b, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("internal/seed/cctld_names.json", append(b, '\n'), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %d ccTLD names\n", len(ordered))
}
