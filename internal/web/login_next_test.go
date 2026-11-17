package web_test

import (
	"net/http"
	neturl "net/url"
	"strings"
	"testing"
	"time"
)

func TestLoginCarriesTheRequestedPage(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	_, jar := newClient(t)
	client := noRedirectClient(jar)

	// An anonymous page request names where it wanted to go.
	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if loc := resp.Header.Get("Location"); loc != "/login?next=%2Ftargets%2F2" {
		t.Fatalf("redirect = %q, want the login page with the target", loc)
	}

	// The login page carries that destination in the form.
	resp, err = client.Get(ts.URL + "/login?next=" + neturl.QueryEscape("/targets/2")) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if !strings.Contains(page, `name="next" value="/targets/2"`) {
		t.Fatalf("login form lost the destination:\n%s", page)
	}
	csrf := csrfFrom(t, page)

	resp, err = client.PostForm(ts.URL+"/login", neturl.Values{ //nolint:bodyclose // body() closes it
		"token": {testToken},
		"csrf":  {csrf},
		"next":  {"/targets/2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if loc := resp.Header.Get("Location"); loc != "/targets/2" {
		t.Fatalf("successful login went to %q, want the target", loc)
	}

	// The session the redirect set is usable on that page.
	follow := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	resp, err = follow.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	if page := body(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("target page after login = %d:\n%s", resp.StatusCode, page)
	}
}

func TestLoginRefusesAnOffsiteNext(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	_, jar := newClient(t)
	client := noRedirectClient(jar)

	// Read one form to get a CSRF token; the cookie stays valid across the
	// attempts below, which keeps the per-client login bucket under its cap.
	resp, err := client.Get(ts.URL + "/login") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	csrf := csrfFrom(t, body(t, resp))

	cases := []struct {
		name string
		next string
	}{
		{"protocol-relative", "//evil.example/targets/2"},
		{"absolute", "https://evil.example/targets/2"},
		{"backslash", "/\\evil.example"},
		{"login loop", "/login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.PostForm(ts.URL+"/login", neturl.Values{ //nolint:bodyclose // body() closes it
				"token": {testToken},
				"csrf":  {csrf},
				"next":  {tc.next},
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = body(t, resp)
			if loc := resp.Header.Get("Location"); loc != "/" {
				t.Fatalf("next %q redirected to %q, want the board", tc.next, loc)
			}
		})
	}

	// The form itself never echoes a hostile destination back. A fresh client
	// is used because the one above now has a session, and /login redirects an
	// authenticated visitor to the board instead of rendering the form.
	_, freshJar := newClient(t)
	fresh := noRedirectClient(freshJar)
	resp, err = fresh.Get(ts.URL + "/login?next=" + neturl.QueryEscape("//evil.example")) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if strings.Contains(page, "//evil.example") {
		t.Fatalf("the login form echoed the hostile destination:\n%s", page)
	}
	if !strings.Contains(page, `name="next" value="/"`) {
		t.Fatalf("the login form did not fall back to the board:\n%s", page)
	}
}
