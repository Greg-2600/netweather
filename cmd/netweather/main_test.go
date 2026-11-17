package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := run([]string{"version"}, &out); err != nil {
		t.Fatalf("run(version) = %v, want nil", err)
	}
	if got := out.String(); !strings.Contains(got, "netweather") {
		t.Fatalf("version output %q does not identify the binary", got)
	}
}

func TestRunHealthcheck(t *testing.T) {
	t.Parallel()

	if err := run([]string{"healthcheck"}, io.Discard); err != nil {
		t.Fatalf("run(healthcheck) = %v, want nil", err)
	}
}

func TestRunUnknownCommandIsAnError(t *testing.T) {
	t.Parallel()

	err := run([]string{"definitely-not-a-command"}, io.Discard)
	if err == nil {
		t.Fatal("run(unknown) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error %q does not name the problem", err)
	}
}

func TestRunServeRefusesPublicBindWithoutToken(t *testing.T) {
	// Not parallel: it mutates the process environment.
	t.Setenv("NETWEATHER_UI_LISTEN", "0.0.0.0:2600")
	t.Setenv("NETWEATHER_UI_TOKEN", "")
	t.Setenv("NETWEATHER_UI_TOKEN_FILE", "")
	t.Setenv("NETWEATHER_UI_ALLOW_PUBLIC", "")

	err := run([]string{"serve"}, io.Discard)
	if err == nil {
		t.Fatal("a public bind without a token must refuse to start")
	}
	if !strings.Contains(err.Error(), "without auth") {
		t.Fatalf("error %q does not explain the refusal", err)
	}
}

func TestRunServeRejectsBadListenAddress(t *testing.T) {
	t.Setenv("NETWEATHER_UI_LISTEN", "not-an-address")
	t.Setenv("NETWEATHER_UI_ALLOW_PUBLIC", "true")

	if err := run([]string{"serve"}, io.Discard); err == nil {
		t.Fatal("an unparseable listen address must fail before binding")
	}
}
