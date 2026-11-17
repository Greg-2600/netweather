package agent

import "testing"

// TestStageFor keeps the drawer's stage badge and LOGGING.md's stage list
// honest: a successful dns/doh/icmp probe must not be labelled http just
// because the instrument has no error class.
func TestStageFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		instrument string
		class      string
		want       string
	}{
		{"dns", "", "dns"},
		{"dns", "dns_timeout", "dns"},
		{"doh", "", "doh"},
		{"doh", "doh_nxdomain", "doh"},
		{"doh", "http_timeout", "doh"},
		{"tls", "", "tls"},
		{"tls", "tls_handshake", "tls"},
		{"tcp", "", "tcp"},
		{"tcpping", "", "tcp"},
		{"icmp", "", "icmp"},
		{"icmp", "icmp_timeout", "icmp"},
		{"ntp", "", "ntp"},
		{"ntp", "ntp_offset", "ntp"},
		{"http", "", "http"},
		{"", "", "http"},
	}
	for _, tc := range cases {
		if got := stageFor(Target{Instrument: tc.instrument}, tc.class); got != tc.want {
			t.Errorf("stageFor(%q, %q) = %q, want %q", tc.instrument, tc.class, got, tc.want)
		}
	}
}
