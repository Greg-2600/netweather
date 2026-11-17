package baseline_test

import (
	"testing"

	"github.com/Greg-2600/netweather/internal/baseline"
)

func TestDetectorPushFloor(t *testing.T) {
	t.Parallel()

	// One bucket below the floor is not a pattern; two consecutive are.
	d := baseline.NewDetector()
	if fired, _ := d.PushFloor(0.80, 0.90); fired {
		t.Fatal("a single bucket fired")
	}
	if fired, repair := d.PushFloor(0.85, 0.90); !fired || repair == "" {
		t.Fatal("two consecutive buckets below the floor did not fire")
	}

	// A recovery in between resets the run.
	d = baseline.NewDetector()
	if fired, _ := d.PushFloor(0.80, 0.90); fired {
		t.Fatal("first bucket fired")
	}
	if fired, _ := d.PushFloor(0.95, 0.90); fired {
		t.Fatal("a recovery fired")
	}
	if fired, _ := d.PushFloor(0.80, 0.90); fired {
		t.Fatal("a bucket after a recovery fired on its own")
	}
	if fired, _ := d.PushFloor(0.70, 0.90); !fired {
		t.Fatal("the second consecutive bucket after a recovery did not fire")
	}

	// Reset clears the run, which the caller does after recording an anomaly.
	d = baseline.NewDetector()
	_, _ = d.PushFloor(0.80, 0.90)
	d.Reset()
	if fired, _ := d.PushFloor(0.80, 0.90); fired {
		t.Fatal("Reset left the previous bucket in place")
	}

	// A floor of zero is never crossed by a health share in [0,1].
	d = baseline.NewDetector()
	_, _ = d.PushFloor(0, 0.90)
	if fired, _ := d.PushFloor(0, 0.90); !fired {
		t.Fatal("zero health below a floor did not fire")
	}
}
