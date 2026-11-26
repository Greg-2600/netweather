//go:build !unix

package startup_test

import (
	"testing"

	"github.com/Greg-2600/netweather/internal/startup"
)

func TestEUIDWithoutUIDConcept(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want int
	}{
		{name: "returns the no-uid sentinel", want: -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := startup.EUID(); got != tc.want {
				t.Fatalf("EUID() = %d, want %d", got, tc.want)
			}
		})
	}
}
