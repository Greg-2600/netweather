package shipped_test

import (
	"testing"

	shipped "github.com/Greg-2600/netweather/config"
)

func TestFieldtrialFileIsTheFullFleet(t *testing.T) {
	t.Parallel()

	f, err := shipped.FieldtrialFile()
	if err != nil {
		t.Fatalf("FieldtrialFile: %v", err)
	}
	if len(f.Targets) != 1000 {
		t.Fatalf("targets = %d, want the 1000-target fleet", len(f.Targets))
	}
	if len(f.Corridors) != 10 {
		t.Fatalf("corridors = %d, want the ten field-trial corridors", len(f.Corridors))
	}
	for _, target := range f.Targets {
		if target.ID == "" || target.Class == "" || target.Probe == "" {
			t.Fatalf("incomplete shipped target: %+v", target)
		}
	}
}
