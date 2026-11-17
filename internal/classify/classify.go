// Package classify turns one observation into a candidate state, a confidence,
// and a reason. It is the anti-noise core: precedence is transport -> challenge
// -> expected status -> auth/bot wall -> config error -> rate limit/legal ->
// origin failure -> fallback (CLASSIFIER.md §8).
//
// It is pure: no clock, no network, no store. That is what makes the rule table
// testable against recorded responses instead of live sites.
package classify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/Greg-2600/netweather/internal/state"
)

// Confidence says how much weight a second observation should give this result.
type Confidence string

const (
	High   Confidence = "high"
	Medium Confidence = "medium"
	Low    Confidence = "low"
)

// Result is a classifier decision.
type Result struct {
	State      state.State
	Confidence Confidence
	Reason     string
}

// Response is everything an instrument observed. Body is already capped by the
// caller (32 KB, CLASSIFIER.md §4); the classifier never reads more.
type Response struct {
	// Err and ErrorClass are set by the instrument's transport layer. Any
	// non-empty ErrorClass short-circuits the HTTP rules.
	Err        error
	ErrorClass string

	Status   int
	Header   http.Header
	Body     []byte
	FinalURL string // final hop after redirects, for challenge-URL detection
}

// Expected is the per-target contract. Zero value means "any default status,
// reachability only".
type Expected struct {
	Status []int // defaults to 200,203,204,301,302

	BodyContains    string
	BodyNotContains string
	BodyRegex       *regexp.Regexp
	TitleContains   string
	HeaderPresent   string
	HeaderName      string
	HeaderValue     string
	MinBodyBytes    int
	JSONPath        string
	JSONValue       string

	// ICMPAnchor marks a target that exists to answer ICMP. Only those may be
	// classified DOWN from an ICMP timeout; on a service target it is meaningless.
	ICMPAnchor bool
}

var defaultStatuses = []int{200, 203, 204, 301, 302}

// HasSemantic reports whether any content assertion is configured.
func (e Expected) HasSemantic() bool {
	return e.BodyContains != "" || e.BodyNotContains != "" || e.BodyRegex != nil ||
		e.TitleContains != "" || e.HeaderPresent != "" || e.HeaderName != "" ||
		e.MinBodyBytes > 0 || e.JSONPath != ""
}

// StatusAllowed reports whether status is in the expected set (or the default).
func (e Expected) StatusAllowed(status int) bool {
	allowed := e.Status
	if len(allowed) == 0 {
		allowed = defaultStatuses
	}
	for _, s := range allowed {
		if s == status {
			return true
		}
	}
	return false
}

// Decide applies the rule table. It never mutates its inputs and never fails.
func Decide(t Expected, r Response) Result {
	// 1. Transport.
	if r.Err != nil || r.ErrorClass != "" {
		return transport(t, r)
	}

	// 2. Challenge, before status and before semantic: a WAF page can be a 200,
	//    a 403, or a 503, and all three mean the same thing.
	if sigs := challengeSignatures(r); len(sigs) > 0 {
		confidence := Medium
		if len(sigs) > 1 {
			confidence = High
		}
		return Result{state.Blocked, confidence, "challenge: " + strings.Join(sigs, ", ")}
	}

	// 3. Expected status.
	if t.StatusAllowed(r.Status) {
		if isJSShell(r.Body, t) {
			return Result{state.UpReachability, Low, "no assessable content"}
		}
		if !t.HasSemantic() {
			return Result{state.UpReachability, Low, "reachability only"}
		}
		if ok, why := runSemantic(t, r); !ok {
			return Result{state.Degraded, Medium, why}
		}
		return Result{state.Up, High, "semantic ok"}
	}

	// 4-8. Status-only outcomes.
	return statusResult(r.Status)
}

func transport(t Expected, r Response) Result {
	class := r.ErrorClass
	if class == "" {
		class = "transport"
	}

	switch {
	case class == "tls_issuer_unexpected":
		// TAMPER is a verdict, not a target state; a single unexpected issuer is
		// recorded as an anomaly and needs a second signal (CLASSIFIER.md §3.2).
		return Result{state.Unknown, High, class + ": issuer outside learned set"}
	case class == "tls_reset_after_hello":
		// Ambiguous by definition, so low confidence and never on its own.
		return Result{state.Blocked, Low, class + ": reproducible TLS reset"}
	case class == "icmp_loss_partial":
		return Result{state.Degraded, Medium, class}
	case class == "icmp_timeout":
		if t.ICMPAnchor {
			return Result{state.Down, Medium, class}
		}
		return Result{state.Unknown, Low, class + ": not a service signal"}
	case strings.HasPrefix(class, "ntp_"):
		return Result{state.Unknown, Low, class + ": self-health only"}
	case strings.HasPrefix(class, "dns_"):
		return Result{state.Down, Medium, class}
	case strings.HasPrefix(class, "tcp_"), strings.HasPrefix(class, "tls_"), class == "icmp_unreachable":
		return Result{state.Down, High, class}
	default:
		return Result{state.Unknown, Low, "unclassified transport error: " + class}
	}
}

func statusResult(status int) Result {
	switch {
	case status == http.StatusUnauthorized:
		return Result{state.Blocked, High, "auth wall (401)"}
	case status == http.StatusForbidden:
		return Result{state.Blocked, High, "forbidden (403)"}
	case status == 404, status == 405, status == 407, status == 410:
		return Result{state.ConfigError, High, fmt.Sprintf("%d on probe path", status)}
	case status == http.StatusRequestTimeout:
		return Result{state.Down, High, "origin gave up (408)"}
	case status == http.StatusTooManyRequests:
		return Result{state.Blocked, High, "rate limited (429)"}
	case status == 451:
		return Result{state.Blocked, High, "legal block (451)"}
	case status == 999:
		return Result{state.Blocked, High, "vendor block (999)"}
	case status >= 500 && status <= 599:
		return Result{state.Down, High, fmt.Sprintf("origin %d", status)}
	case status >= 400 && status < 500:
		return Result{state.ConfigError, High, fmt.Sprintf("%d on probe path", status)}
	case status >= 100 && status < 400:
		return Result{state.Degraded, Low, fmt.Sprintf("unexpected %d", status)}
	default:
		return Result{state.Unknown, Low, fmt.Sprintf("unclassified status %d", status)}
	}
}

var bodyMarkers = []string{
	"just a moment...",
	"checking your browser before accessing",
	"enable javascript and cookies to continue",
	"attention required! | cloudflare",
	"request unsuccessful. incapsula incident id",
	"_incapsula_resource",
	"captcha-delivery.com",
	"px-captcha",
	"please verify you are a human",
	"pardon our interruption",
	"unusual traffic from your computer network",
	"the requested url was rejected",
	"radware bot manager captcha",
	"are you a robot?",
	"checking if the site connection is secure",
}

var redirectMarkers = []string{
	"/__cf_chl",
	"/cdn-cgi/challenge-platform",
	"/sorry/",
	"google.com/sorry",
	"/incapsula/",
	"/ddm/",
	"captcha-delivery.com",
	"/px/",
}

// challengeSignatures returns every independent signature found, deduplicated and
// sorted. Two or more raise confidence to high.
func challengeSignatures(r Response) []string {
	found := make(map[string]struct{})

	add := func(sig string) { found[sig] = struct{}{} }

	if h := r.Header; h != nil {
		if strings.EqualFold(h.Get("cf-mitigated"), "challenge") {
			add("header:cf-mitigated")
		}
		switch strings.ToLower(h.Get("x-amzn-waf-action")) {
		case "challenge", "captcha":
			add("header:x-amzn-waf-action")
		}
		for _, k := range []string{"x-iinfo", "x-datadome", "x-sucuri-block"} {
			if h.Get(k) != "" {
				add("header:" + k)
			}
		}
		for k := range h {
			lk := strings.ToLower(k)
			if strings.HasPrefix(lk, "x-px-") {
				add("header:x-px")
			}
			if strings.HasPrefix(lk, "x-kpsdk-") {
				add("header:x-kpsdk")
			}
		}
		if strings.EqualFold(h.Get("server"), "AkamaiGHost") && r.Status == http.StatusForbidden {
			add("header:akamai-403")
		}
		if strings.EqualFold(h.Get("x-vercel-mitigated"), "challenge") {
			add("header:x-vercel-mitigated")
		}
	}

	body := strings.ToLower(string(r.Body))
	for _, marker := range bodyMarkers {
		if strings.Contains(body, marker) {
			add("body:" + marker)
		}
	}
	if strings.Contains(body, "access denied") && strings.Contains(body, "reference #") {
		add("body:akamai-reference")
	}
	if strings.Contains(body, "cf-error-details") && strings.Contains(body, "challenge") {
		add("body:cf-error-details")
	}

	url := strings.ToLower(r.FinalURL)
	for _, marker := range redirectMarkers {
		if url != "" && strings.Contains(url, marker) {
			add("redirect:" + marker)
		}
	}

	out := make([]string, 0, len(found))
	for sig := range found {
		out = append(out, sig)
	}
	sort.Strings(out)
	return out
}

func isJSShell(body []byte, t Expected) bool {
	s := strings.ToLower(string(body))
	if strings.Contains(s, "<noscript>") && strings.Contains(s, "enable javascript") {
		return true
	}
	if len(body) < 2048 && strings.Contains(s, "<script src=") {
		// A configured body marker means the page is assessable even if small.
		if t.BodyContains == "" || !strings.Contains(s, strings.ToLower(t.BodyContains)) {
			return true
		}
	}
	return strings.Contains(s, `<div id="root"></div>`) || strings.Contains(s, `<div id='root'></div>`)
}

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func runSemantic(t Expected, r Response) (bool, string) {
	body := string(r.Body)
	lower := strings.ToLower(body)

	if t.BodyContains != "" && !strings.Contains(lower, strings.ToLower(t.BodyContains)) {
		return false, "missing body marker"
	}
	if t.BodyNotContains != "" && strings.Contains(lower, strings.ToLower(t.BodyNotContains)) {
		return false, "unexpected body marker"
	}
	if t.BodyRegex != nil && !t.BodyRegex.MatchString(body) {
		return false, "body regex did not match"
	}
	if t.TitleContains != "" {
		m := titleRE.FindStringSubmatch(body)
		if len(m) < 2 || !strings.Contains(strings.ToLower(m[1]), strings.ToLower(t.TitleContains)) {
			return false, "title mismatch"
		}
	}
	if t.HeaderPresent != "" && r.Header.Get(t.HeaderPresent) == "" {
		return false, "missing header " + t.HeaderPresent
	}
	if t.HeaderName != "" && r.Header.Get(t.HeaderName) != t.HeaderValue {
		return false, "header value mismatch"
	}
	if t.MinBodyBytes > 0 && len(r.Body) < t.MinBodyBytes {
		return false, "body shorter than minimum"
	}
	if t.JSONPath != "" {
		if ok, why := checkJSON(r.Body, t.JSONPath, t.JSONValue); !ok {
			return false, why
		}
	}
	return true, "semantic ok"
}

func checkJSON(body []byte, path, want string) (bool, string) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return false, "body is not JSON"
	}

	current := root
	for _, part := range strings.Split(path, ".") {
		obj, ok := current.(map[string]any)
		if !ok {
			return false, "json path is not an object at " + part
		}
		next, ok := obj[part]
		if !ok {
			return false, "json path missing " + part
		}
		current = next
	}

	if got := fmt.Sprint(current); got != want {
		return false, fmt.Sprintf("json %s = %s, want %s", path, got, want)
	}
	return true, ""
}
