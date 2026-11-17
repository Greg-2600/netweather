package streamlimit_test

import (
	"testing"

	"github.com/Greg-2600/netweather/internal/streamlimit"
)

func TestAcquireEnforcesPerIPAndTotal(t *testing.T) {
	t.Parallel()

	l := streamlimit.New(2, 1)

	releaseA, ok := l.Acquire("10.0.0.1")
	if !ok {
		t.Fatal("first slot was refused")
	}
	if _, ok := l.Acquire("10.0.0.1"); ok {
		t.Fatal("a second connection from the same IP was allowed")
	}

	releaseB, ok := l.Acquire("10.0.0.2")
	if !ok {
		t.Fatal("a second IP was refused while below the total")
	}
	if _, ok := l.Acquire("10.0.0.3"); ok {
		t.Fatal("the total cap was not enforced")
	}

	releaseA()
	releaseB()

	total, perIP := l.Counts()
	if total != 0 || len(perIP) != 0 {
		t.Fatalf("counts after release = %d, %+v", total, perIP)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	l := streamlimit.New(1, 1)
	release, ok := l.Acquire("10.0.0.1")
	if !ok {
		t.Fatal("slot was refused")
	}

	release()
	release()
	release()

	total, _ := l.Counts()
	if total != 0 {
		t.Fatalf("double release left total = %d", total)
	}

	// A double release must not free someone else's slot either.
	release2, ok := l.Acquire("10.0.0.1")
	if !ok {
		t.Fatal("slot was not reusable after release")
	}
	defer release2()
}

func TestDefaultsCannotBeDisabled(t *testing.T) {
	t.Parallel()

	l := streamlimit.New(0, -1)

	// The default per-IP cap is 8, so the ninth from one address is refused.
	releases := make([]func(), 0, streamlimit.DefaultPerIP)
	for i := 0; i < streamlimit.DefaultPerIP; i++ {
		release, ok := l.Acquire("10.0.0.1")
		if !ok {
			t.Fatalf("connection %d was refused under the default cap", i+1)
		}
		releases = append(releases, release)
	}
	if _, ok := l.Acquire("10.0.0.1"); ok {
		t.Fatal("the default per-IP cap was not enforced")
	}
	for _, release := range releases {
		release()
	}

	// Another IP is still served, proving the total default is not exhausted.
	release, ok := l.Acquire("10.0.0.2")
	if !ok {
		t.Fatal("a different IP was refused")
	}
	release()
}
