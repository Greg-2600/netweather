package probe_test

import (
	"crypto/tls"
	"testing"

	"github.com/Greg-2600/netweather/internal/probe"
)

// TestTransportCapsPerHostConcurrency pins THREAT_MODEL.md T24: the transport
// permits one connection per host, keeps a small idle pool, and never honours
// an environment proxy. A later copy of this literal is exactly the kind of
// drift the threat-model table would then describe incorrectly.
func TestTransportCapsPerHostConcurrency(t *testing.T) {
	t.Parallel()

	tr := probe.TransportForDial(nil)

	if tr.Proxy != nil {
		t.Error("Proxy is set; HTTP_PROXY would reroute probes")
	}
	if tr.MaxConnsPerHost != 1 {
		t.Errorf("MaxConnsPerHost = %d, want 1", tr.MaxConnsPerHost)
	}
	if tr.MaxIdleConnsPerHost != 1 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 1", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns != 4 {
		t.Errorf("MaxIdleConns = %d, want the small documented pool of 4", tr.MaxIdleConns)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("TLS minimum version is not TLS 1.2")
	}
	if tr.ResponseHeaderTimeout == 0 || tr.TLSHandshakeTimeout == 0 {
		t.Error("the per-stage timeouts are unset")
	}
}

// TestTransportIgnoresProxyEnvironment pins THREAT_MODEL.md invariant 6: even
// with the proxy variables set, the transport has no proxy, so a probe cannot
// be silently rerouted through an environment-supplied hop.
func TestTransportIgnoresProxyEnvironment(t *testing.T) {
	// No t.Parallel: it sets environment variables.
	t.Setenv("HTTP_PROXY", "http://proxy.example:8080")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:8080")
	t.Setenv("ALL_PROXY", "socks5://proxy.example:1080")

	if tr := probe.TransportForDial(nil); tr.Proxy != nil {
		t.Fatal("Proxy is set from the environment; probes would be rerouted")
	}
}
