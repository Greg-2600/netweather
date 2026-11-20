package probe //nolint:testpackage // isTimeout is an unexported helper with no external seam

import "testing"

func TestIsTimeoutRejectsNil(t *testing.T) {
	t.Parallel()

	if isTimeout(nil) {
		t.Fatal("a nil error must not be a timeout")
	}
}
