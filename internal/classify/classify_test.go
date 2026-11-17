package classify_test

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/state"
)

func TestTransportClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		class      string
		err        error
		icmpAnchor bool
		want       state.State
		conf       classify.Confidence
	}{
		{"dns nxdomain", "dns_nxdomain", errors.New("no such host"), false, state.Down, classify.Medium},
		{"dns timeout", "dns_timeout", errors.New("timeout"), false, state.Down, classify.Medium},
		{"tcp refused", "tcp_refused", errors.New("refused"), false, state.Down, classify.High},
		{"tcp timeout", "tcp_timeout", errors.New("timeout"), false, state.Down, classify.High},
		{"tls expired", "tls_cert_expired", errors.New("expired"), false, state.Down, classify.High},
		{"tls mismatch", "tls_cert_mismatch", errors.New("mismatch"), false, state.Down, classify.High},
		{"tls reset", "tls_reset_after_hello", errors.New("reset"), false, state.Blocked, classify.Low},
		{"issuer unexpected", "tls_issuer_unexpected", errors.New("issuer"), false, state.Unknown, classify.High},
		{"icmp timeout anchor", "icmp_timeout", errors.New("timeout"), true, state.Down, classify.Medium},
		{"icmp timeout service", "icmp_timeout", errors.New("timeout"), false, state.Unknown, classify.Low},
		{"icmp partial loss", "icmp_loss_partial", errors.New("loss"), false, state.Degraded, classify.Medium},
		{"icmp unreachable", "icmp_unreachable", errors.New("unreachable"), false, state.Down, classify.High},
		{"ntp offset", "ntp_offset", errors.New("offset"), false, state.Unknown, classify.Low},
		{"ntp unreachable", "ntp_unreachable", errors.New("no reply"), false, state.Unknown, classify.Low},
		{"http timeout", "http_timeout", errors.New("timeout"), false, state.Down, classify.High},
		{"http reset", "http_reset", errors.New("reset"), false, state.Down, classify.High},
		{"http request", "http_request", errors.New("bad url"), false, state.ConfigError, classify.High},
		{"http other", "http_error", errors.New("?"), false, state.Unknown, classify.Low},
		{"unknown class", "weird", errors.New("?"), false, state.Unknown, classify.Low},
		{"error without class", "", errors.New("?"), false, state.Unknown, classify.Low},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := classify.Decide(classify.Expected{ICMPAnchor: tc.icmpAnchor}, classify.Response{
				Err:        tc.err,
				ErrorClass: tc.class,
			})
			if got.State != tc.want || got.Confidence != tc.conf {
				t.Fatalf("got (%s, %s), want (%s, %s) [%s]", got.State, got.Confidence, tc.want, tc.conf, got.Reason)
			}
			if got.Reason == "" {
				t.Fatal("empty reason")
			}
		})
	}
}

func TestDNSBlockedIsAVisibilityFailure(t *testing.T) {
	t.Parallel()

	// The vantage's own port 53 is blocked: a timeout or resolver error is a
	// fact about us, not the target, so it must not turn anchors red.
	for _, class := range []string{"dns_timeout", "dns_error"} {
		got := classify.Decide(classify.Expected{DNSBlocked: true}, classify.Response{
			Err: errors.New("timeout"), ErrorClass: class,
		})
		if got.State != state.Unmeasurable {
			t.Errorf("%s with a blocked port = %s, want unmeasurable", class, got.State)
		}
		if got.Confidence != classify.High {
			t.Errorf("%s confidence = %s, want high", class, got.Confidence)
		}
		if !strings.Contains(got.Reason, "port 53") {
			t.Errorf("%s reason = %q, want the blocked port named", class, got.Reason)
		}

		// Without the gate the same failure is still DOWN.
		got = classify.Decide(classify.Expected{}, classify.Response{
			Err: errors.New("timeout"), ErrorClass: class,
		})
		if got.State != state.Down {
			t.Errorf("%s without a blocked port = %s, want down", class, got.State)
		}
	}

	// A name that does not exist is target truth, not a blocked port: an
	// answering resolver returned NXDOMAIN, so the gate must not suppress it.
	got := classify.Decide(classify.Expected{DNSBlocked: true}, classify.Response{
		Err: errors.New("no such host"), ErrorClass: "dns_nxdomain",
	})
	if got.State != state.Down {
		t.Fatalf("nxdomain with a blocked port = %s, want down", got.State)
	}
}

func TestStatusTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status int
		want   state.State
	}{
		{200, state.UpReachability},
		{203, state.UpReachability},
		{204, state.UpReachability},
		{301, state.UpReachability},
		{302, state.UpReachability},

		{401, state.Blocked},
		{403, state.Blocked},
		{404, state.ConfigError},
		{405, state.ConfigError},
		{407, state.ConfigError},
		{410, state.ConfigError},
		{418, state.ConfigError},
		{408, state.Down},
		{429, state.Blocked},
		{451, state.Blocked},
		{999, state.Blocked},

		{500, state.Down},
		{502, state.Down},
		{503, state.Down},
		{504, state.Down},
		{520, state.Down},
		{527, state.Down},
		{530, state.Down},

		{303, state.Degraded}, // 3xx outside the default set
		{0, state.Unknown},
	}

	for _, tc := range tests {
		got := classify.Decide(classify.Expected{}, classify.Response{Status: tc.status})
		if got.State != tc.want {
			t.Errorf("status %d: got %s, want %s (%s)", tc.status, got.State, tc.want, got.Reason)
		}
	}
}

func TestChallengeBeatsStatusAndSemantic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		resp   classify.Response
		want   state.State
		conf   classify.Confidence
		reason string
	}{
		{
			name: "cloudflare header on 200",
			resp: classify.Response{
				Status: 200,
				Header: http.Header{"Cf-Mitigated": {"challenge"}},
				Body:   []byte("ok"),
			},
			want: state.Blocked, conf: classify.Medium, reason: "cf-mitigated",
		},
		{
			name: "cloudflare body on 503",
			resp: classify.Response{
				Status: 503,
				Body:   []byte("<title>Just a moment...</title>"),
			},
			want: state.Blocked, conf: classify.Medium, reason: "just a moment...",
		},
		{
			name: "akamai 403 has two signatures",
			resp: classify.Response{
				Status: 403,
				Header: http.Header{"Server": {"AkamaiGHost"}},
				Body:   []byte("Access Denied Reference #18.abc"),
			},
			want: state.Blocked, conf: classify.High, reason: "akamai",
		},
		{
			name: "challenge redirect",
			resp: classify.Response{
				Status:   302,
				FinalURL: "https://example.com/__cf_chl_rt_tk=abc",
			},
			want: state.Blocked, conf: classify.Medium, reason: "__cf_chl",
		},
		{
			name: "perimeterx header",
			resp: classify.Response{
				Status: 200,
				Header: http.Header{"X-Px-Enforcer": {"1"}},
			},
			want: state.Blocked, conf: classify.Medium, reason: "x-px",
		},
		{
			name: "datadome body",
			resp: classify.Response{
				Status: 200,
				Body:   []byte(`<script src="https://ct.captcha-delivery.com/x.js"></script>`),
			},
			want: state.Blocked, conf: classify.Medium, reason: "captcha-delivery",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := classify.Decide(classify.Expected{BodyContains: "ok"}, tc.resp)
			if got.State != tc.want || got.Confidence != tc.conf {
				t.Fatalf("got (%s, %s), want (%s, %s)", got.State, got.Confidence, tc.want, tc.conf)
			}
			if !strings.Contains(strings.ToLower(got.Reason), strings.ToLower(tc.reason)) {
				t.Fatalf("reason %q does not mention %q", got.Reason, tc.reason)
			}
		})
	}
}

func TestTransportBeatsChallenge(t *testing.T) {
	t.Parallel()

	got := classify.Decide(classify.Expected{}, classify.Response{
		Err:        errors.New("reset"),
		ErrorClass: "tcp_reset",
		Status:     200,
		Header:     http.Header{"Cf-Mitigated": {"challenge"}},
		Body:       []byte("Just a moment..."),
	})
	if got.State != state.Down {
		t.Fatalf("transport must win, got %s (%s)", got.State, got.Reason)
	}
}

func TestJSShellIsReachabilityNotDegraded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"noscript", `<html><noscript>Please enable JavaScript</noscript></html>`},
		{"small script shell", `<html><head><script src="/app.js"></script></head></html>`},
		{"spa root", `<html><body><div id="root"></div></body></html>`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := classify.Decide(classify.Expected{}, classify.Response{Status: 200, Body: []byte(tc.body)})
			if got.State != state.UpReachability || got.Confidence != classify.Low {
				t.Fatalf("got (%s, %s), want (up_reachability, low)", got.State, got.Confidence)
			}
		})
	}
}

func TestSmallPageWithConfiguredMarkerIsNotAShell(t *testing.T) {
	t.Parallel()

	got := classify.Decide(
		classify.Expected{BodyContains: "welcome"},
		classify.Response{Status: 200, Body: []byte(`<script src="/a.js"></script>welcome`)},
	)
	if got.State != state.Up || got.Confidence != classify.High {
		t.Fatalf("got (%s, %s), want (up, high): %s", got.State, got.Confidence, got.Reason)
	}
}

func TestSemanticChecks(t *testing.T) {
	t.Parallel()

	header := http.Header{"X-Health": {"green"}, "Content-Type": {"application/json"}}
	jsonBody := []byte(`{"status":"ok","service":{"up":true}}`)

	tests := []struct {
		name string
		exp  classify.Expected
		resp classify.Response
		want state.State
	}{
		{
			name: "body contains",
			exp:  classify.Expected{BodyContains: "HEALTHY"},
			resp: classify.Response{Status: 200, Body: []byte("all healthy")},
			want: state.Up,
		},
		{
			name: "body contains fails",
			exp:  classify.Expected{BodyContains: "healthy"},
			resp: classify.Response{Status: 200, Body: []byte("service is down")},
			want: state.Degraded,
		},
		{
			name: "body not contains fails",
			exp:  classify.Expected{BodyNotContains: "error"},
			resp: classify.Response{Status: 200, Body: []byte("an error occurred")},
			want: state.Degraded,
		},
		{
			name: "regex passes",
			exp:  classify.Expected{BodyRegex: regexp.MustCompile(`v\d+\.\d+`)},
			resp: classify.Response{Status: 200, Body: []byte("version v1.2")},
			want: state.Up,
		},
		{
			name: "regex fails",
			exp:  classify.Expected{BodyRegex: regexp.MustCompile(`v\d+\.\d+`)},
			resp: classify.Response{Status: 200, Body: []byte("version")},
			want: state.Degraded,
		},
		{
			name: "title",
			exp:  classify.Expected{TitleContains: "Dashboard"},
			resp: classify.Response{Status: 200, Body: []byte("<title>Admin Dashboard</title>")},
			want: state.Up,
		},
		{
			name: "title missing",
			exp:  classify.Expected{TitleContains: "Dashboard"},
			resp: classify.Response{Status: 200, Body: []byte("<title>Login</title>")},
			want: state.Degraded,
		},
		{
			name: "header present",
			exp:  classify.Expected{HeaderPresent: "X-Health"},
			resp: classify.Response{Status: 200, Header: header},
			want: state.Up,
		},
		{
			name: "header absent",
			exp:  classify.Expected{HeaderPresent: "X-Missing"},
			resp: classify.Response{Status: 200, Header: header},
			want: state.Degraded,
		},
		{
			name: "header value",
			exp:  classify.Expected{HeaderName: "X-Health", HeaderValue: "green"},
			resp: classify.Response{Status: 200, Header: header},
			want: state.Up,
		},
		{
			name: "header value mismatch",
			exp:  classify.Expected{HeaderName: "X-Health", HeaderValue: "red"},
			resp: classify.Response{Status: 200, Header: header},
			want: state.Degraded,
		},
		{
			name: "min body bytes",
			exp:  classify.Expected{MinBodyBytes: 100},
			resp: classify.Response{Status: 200, Body: []byte("tiny")},
			want: state.Degraded,
		},
		{
			name: "json path",
			exp:  classify.Expected{JSONPath: "service.up", JSONValue: "true"},
			resp: classify.Response{Status: 200, Body: jsonBody},
			want: state.Up,
		},
		{
			name: "json path wrong value",
			exp:  classify.Expected{JSONPath: "status", JSONValue: "down"},
			resp: classify.Response{Status: 200, Body: jsonBody},
			want: state.Degraded,
		},
		{
			name: "json path missing",
			exp:  classify.Expected{JSONPath: "nope", JSONValue: "x"},
			resp: classify.Response{Status: 200, Body: jsonBody},
			want: state.Degraded,
		},
		{
			name: "json invalid",
			exp:  classify.Expected{JSONPath: "a", JSONValue: "1"},
			resp: classify.Response{Status: 200, Body: []byte("not json")},
			want: state.Degraded,
		},
		{
			name: "json path through scalar",
			exp:  classify.Expected{JSONPath: "status.deep", JSONValue: "1"},
			resp: classify.Response{Status: 200, Body: jsonBody},
			want: state.Degraded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := classify.Decide(tc.exp, tc.resp)
			if got.State != tc.want {
				t.Fatalf("got %s (%s), want %s", got.State, got.Reason, tc.want)
			}
		})
	}
}

func TestExpectedStatusOverride(t *testing.T) {
	t.Parallel()

	// 200 is not in the configured set, so it is not a success even though it is
	// a 2xx; the fallback says "unexpected", not "down".
	got := classify.Decide(
		classify.Expected{Status: []int{204}},
		classify.Response{Status: 200},
	)
	if got.State != state.Degraded || got.Confidence != classify.Low {
		t.Fatalf("got (%s, %s), want (degraded, low)", got.State, got.Confidence)
	}
}

func TestDefaultStatusSet(t *testing.T) {
	t.Parallel()

	exp := classify.Expected{}
	for _, s := range []int{200, 203, 204, 301, 302} {
		if !exp.StatusAllowed(s) {
			t.Errorf("default should allow %d", s)
		}
	}
	for _, s := range []int{201, 300, 304, 400} {
		if exp.StatusAllowed(s) {
			t.Errorf("default should not allow %d", s)
		}
	}
}

func TestHasSemantic(t *testing.T) {
	t.Parallel()

	if (classify.Expected{}).HasSemantic() {
		t.Fatal("empty expectation should not be semantic")
	}
	if !(classify.Expected{MinBodyBytes: 1}).HasSemantic() {
		t.Fatal("min body bytes is a semantic check")
	}
	if !(classify.Expected{JSONPath: "a"}).HasSemantic() {
		t.Fatal("json path is a semantic check")
	}
}

func TestDNSDivergenceIsAdvisory(t *testing.T) {
	t.Parallel()

	got := classify.Decide(classify.Expected{}, classify.Response{
		Err: errors.New("one resolver failed"), ErrorClass: "dns_divergence",
	})
	if got.State != state.Unknown {
		t.Fatalf("state = %s, want unknown", got.State)
	}
	if got.Confidence != classify.High {
		t.Fatalf("confidence = %s, want high", got.Confidence)
	}
	if !strings.Contains(got.Reason, "disagrees with its own baseline") {
		t.Fatalf("reason = %q, want the divergence named", got.Reason)
	}
}

func TestPolicyRefusalIsUnmeasurable(t *testing.T) {
	t.Parallel()

	for _, class := range []string{"policy_blocked", "policy_blocked_port"} {
		got := classify.Decide(classify.Expected{}, classify.Response{
			Err: errors.New("destination blocked"), ErrorClass: class,
		})
		if got.State != state.Unmeasurable {
			t.Errorf("%s state = %s, want unmeasurable", class, got.State)
		}
		if got.Confidence != classify.High {
			t.Errorf("%s confidence = %s, want high", class, got.Confidence)
		}
		if !strings.Contains(got.Reason, "outbound policy") {
			t.Errorf("%s reason = %q, want the policy named", class, got.Reason)
		}
	}
}

func TestLowMTUGateMakesATimeoutUnmeasurable(t *testing.T) {
	t.Parallel()

	// The three classes a black-holed large handshake can produce.
	for _, class := range []string{"http_timeout", "tcp_timeout", "tls_handshake"} {
		got := classify.Decide(classify.Expected{MTUBlocked: true}, classify.Response{
			Err: errors.New("timeout"), ErrorClass: class,
		})
		if got.State != state.Unknown {
			t.Errorf("%s under a low MTU = %s, want unknown", class, got.State)
		}
		if got.Confidence != classify.High {
			t.Errorf("%s confidence = %s, want high", class, got.Confidence)
		}
		if !strings.Contains(got.Reason, "MTU") {
			t.Errorf("%s reason = %q, want the MTU named", class, got.Reason)
		}
	}

	// Without the gate the same timeout is a real DOWN, which is what makes
	// the gate meaningful.
	if got := classify.Decide(classify.Expected{}, classify.Response{
		Err: errors.New("timeout"), ErrorClass: "http_timeout",
	}); got.State != state.Down {
		t.Fatalf("ungated http_timeout = %s, want down", got.State)
	}

	// A refused connection under a low MTU is still DOWN: the SYN arrived, so
	// the path is not black-holing this handshake.
	if got := classify.Decide(classify.Expected{MTUBlocked: true}, classify.Response{
		Err: errors.New("refused"), ErrorClass: "tcp_refused",
	}); got.State != state.Down {
		t.Fatalf("tcp_refused under a low MTU = %s, want down", got.State)
	}
}

func TestStatusRange(t *testing.T) {
	t.Parallel()

	expect := classify.Expected{StatusMin: 200, StatusMax: 399}
	for _, code := range []int{200, 204, 301, 399} {
		if !expect.StatusAllowed(code) {
			t.Errorf("%d was rejected by a 200-399 range", code)
		}
	}
	for _, code := range []int{404, 500, 199} {
		if expect.StatusAllowed(code) {
			t.Errorf("%d was accepted by a 200-399 range", code)
		}
	}

	// A range and an explicit list can coexist: either admits the code.
	both := classify.Expected{Status: []int{503}, StatusMin: 200, StatusMax: 299}
	if !both.StatusAllowed(503) || !both.StatusAllowed(201) || both.StatusAllowed(302) {
		t.Fatalf("range plus list = %+v", both)
	}

	// With no range and no list the defaults still apply.
	if !(classify.Expected{}).StatusAllowed(200) || (classify.Expected{}).StatusAllowed(404) {
		t.Fatal("the default status set changed")
	}
}

func TestPartialContentRequiresTheOptIn(t *testing.T) {
	t.Parallel()

	// A ranged request earns a 206; the default status set does not include it
	// unless the target opted in.
	if !(classify.Expected{PartialOK: true}).StatusAllowed(206) {
		t.Error("PartialOK did not admit 206")
	}
	if (classify.Expected{}).StatusAllowed(206) {
		t.Error("206 was admitted without PartialOK")
	}

	// An explicit list decides for itself, and AllowsPartial mirrors it.
	if !(classify.Expected{Status: []int{200, 206}}).AllowsPartial() {
		t.Error("a list containing 206 did not allow partial content")
	}
	if (classify.Expected{Status: []int{200}}).AllowsPartial() {
		t.Error("a list without 206 allowed partial content")
	}
	// A range that covers 206 allows it; one that stops at 299 does not.
	if !(classify.Expected{StatusMin: 200, StatusMax: 399}).AllowsPartial() {
		t.Error("a 200-399 range did not allow 206")
	}
	if (classify.Expected{StatusMin: 200, StatusMax: 205}).AllowsPartial() {
		t.Error("a 200-205 range allowed 206")
	}
	// No status contract uses the defaults, which PartialOK can widen.
	if !(classify.Expected{}).AllowsPartial() {
		t.Error("the default contract did not allow the opt-in")
	}
}
