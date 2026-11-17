package config_test

import (
	"testing"

	"github.com/Greg-2600/netweather/internal/config"
)

// FuzzParse checks the documented contract: arbitrary bytes produce either a
// validated file or an error, never a panic and never a half-valid file.
func FuzzParse(f *testing.F) {
	f.Add([]byte("version: 1\ntargets: []\n"))
	f.Add([]byte("version: 1\ntargets:\n  - name: a\n    host: a.example\n    port: 443\n    probe: tls\n    class: service\n"))
	f.Add([]byte("version: 1\ntargets: []\n---\nversion: 2\n"))
	f.Add([]byte("version: 1\ncorridors:\n  - id: x\n    from: A\n    to: B\n    endpoints: [\"a.example:443\", \"b.example:443\"]\n"))
	f.Add([]byte(""))
	f.Add([]byte{0xff, 0xfe, 0x00})

	f.Fuzz(func(t *testing.T, raw []byte) {
		file, err := config.Parse(raw)
		if err != nil {
			return
		}
		if file == nil {
			t.Fatal("Parse returned a nil file with no error")
		}
		if file.Version != config.CurrentVersion {
			t.Fatalf("accepted version %d", file.Version)
		}
		for _, target := range file.Targets {
			if target.ID == "" || target.Host == "" {
				t.Fatalf("accepted a target without an id/host: %+v", target)
			}
		}
		for _, corridor := range file.Corridors {
			if corridor.ID == "" || len(corridor.Endpoints) != 2 {
				t.Fatalf("accepted an invalid corridor: %+v", corridor)
			}
		}
	})
}
