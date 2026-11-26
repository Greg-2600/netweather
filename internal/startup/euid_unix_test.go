//go:build unix

package startup_test

import (
	"os"
	"testing"

	"github.com/Greg-2600/netweather/internal/startup"
)

func TestEUIDOnUnix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		check func(int) bool
	}{
		{
			name:  "matches os.Geteuid",
			check: func(got int) bool { return got == os.Geteuid() },
		},
		{
			name:  "is a valid uid",
			check: func(got int) bool { return got >= 0 },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := startup.EUID(); !tc.check(got) {
				t.Fatalf("EUID() = %d, want %s", got, tc.name)
			}
		})
	}
}
