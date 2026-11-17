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

func TestRunServeIsHonestUntilPhase0Lands(t *testing.T) {
	t.Parallel()

	err := run([]string{"serve"}, io.Discard)
	if err == nil {
		t.Fatal("run(serve) = nil, but the server is not implemented; a silent success would be a lie")
	}
}
