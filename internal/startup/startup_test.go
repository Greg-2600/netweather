package startup_test

import (
	"errors"
	"testing"

	"github.com/Greg-2600/netweather/internal/startup"
)

func TestRefuseIfRoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		euid    int
		wantErr error
	}{
		{name: "root is refused", euid: 0, wantErr: startup.ErrRunningAsRoot},
		{name: "uid 1 is allowed", euid: 1, wantErr: nil},
		{name: "release uid is allowed", euid: 65532, wantErr: nil},
		{name: "no uid concept is allowed", euid: -1, wantErr: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := startup.RefuseIfRoot(tc.euid)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("RefuseIfRoot(%d) = %v, want %v", tc.euid, err, tc.wantErr)
			}
		})
	}
}
