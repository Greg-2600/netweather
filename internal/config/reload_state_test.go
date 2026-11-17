package config_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/config"
)

func TestReloadState(t *testing.T) {
	t.Parallel()

	var state config.ReloadState

	// The zero value reports no failure, which is what a process that has never
	// reloaded should show.
	if _, _, failed := state.LastReloadFailure(); failed {
		t.Fatal("a zero state reported a failure")
	}

	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	state.Failed(errors.New("config: targets.yaml: line 4: probe \"quik\" is not supported"), at)
	gotAt, detail, failed := state.LastReloadFailure()
	if !failed {
		t.Fatal("a recorded failure was not reported")
	}
	if !gotAt.Equal(at) || detail != "config: targets.yaml: line 4: probe \"quik\" is not supported" {
		t.Fatalf("failure = (%v, %q)", gotAt, detail)
	}

	// A later good edit clears the warning.
	state.Succeeded()
	if _, _, failed := state.LastReloadFailure(); failed {
		t.Fatal("a success left the previous failure in place")
	}
}

func TestReloadStateIgnoresNilAndNilError(t *testing.T) {
	t.Parallel()

	var nilState *config.ReloadState
	// These must not panic: the loop may run with a nil state in a test.
	nilState.Failed(errors.New("x"), time.Now())
	nilState.Succeeded()
	if _, _, failed := nilState.LastReloadFailure(); failed {
		t.Fatal("a nil state reported a failure")
	}

	var state config.ReloadState
	state.Failed(nil, time.Now())
	if _, _, failed := state.LastReloadFailure(); failed {
		t.Fatal("a nil error was recorded as a failure")
	}
}
