package version_test

import (
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/version"
)

func TestStringCarriesEveryField(t *testing.T) {
	t.Parallel()

	got := version.String()
	for _, want := range []string{"netweather", version.Version, version.Commit, version.Date} {
		if !strings.Contains(got, want) {
			t.Errorf("version.String() = %q, missing %q", got, want)
		}
	}
	if strings.Count(got, "\n") != 0 {
		t.Errorf("version.String() must be one line, got %q", got)
	}
}

func TestDefaultsAreNotBlank(t *testing.T) {
	t.Parallel()

	// A build without ldflags must still identify itself rather than printing an
	// empty version, which is what a release bug looks like.
	for name, value := range map[string]string{
		"Version": version.Version,
		"Commit":  version.Commit,
		"Date":    version.Date,
	} {
		if strings.TrimSpace(value) == "" {
			t.Errorf("%s is blank", name)
		}
	}
}
