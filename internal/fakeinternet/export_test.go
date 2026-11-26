package fakeinternet

import (
	"net"

	"golang.org/x/net/dns/dnsmessage"
)

// Test-only access to the fake internet's unexported pure helpers. The linter
// requires the tests themselves to live in package fakeinternet_test.

// ResponseFor exposes Scenario.responseFor to tests.
func ResponseFor(s Scenario, path string) Response { return s.responseFor(path) }

// TrimDot exposes trimDot to tests.
func TrimDot(name string) string { return trimDot(name) }

// AnswerDNS exposes Internet.answerDNS to tests.
func AnswerDNS(i *Internet, query []byte) []byte { return i.answerDNS(query) }

// PackMessage exposes packMessage to tests.
func PackMessage(msg dnsmessage.Message) []byte { return packMessage(msg) }

// MapAddress exposes Internet.mapAddress to tests.
func MapAddress(i *Internet, host string, port int) (string, error) { return i.mapAddress(host, port) }

// NewTestInternet builds an Internet with just the fields the pure helpers
// need, without starting listeners.
func NewTestInternet(byHost map[string]Scenario, httpsLn net.Listener) *Internet {
	return &Internet{byHost: byHost, httpsLn: httpsLn}
}
