// Package shipped exposes the full target list embedded in the binary.
//
// The default install is the field-trial fleet, not the small starter list in
// internal/seed. Embedding the file keeps compose, the Kubernetes manifests,
// and the Helm chart on the same 1000-target default without a ConfigMap or a
// bind mount, and keeps the provenance hash identical to the checked-in file.
package shipped

import (
	_ "embed"
	"sync"

	"github.com/Greg-2600/netweather/internal/config"
)

// FieldtrialTargets is the checked-in full target list, compiled into the
// binary. It includes the fleet and the ten field-trial corridors.
//
//go:embed fieldtrial-targets.yaml
var FieldtrialTargets []byte

var (
	once sync.Once
	file *config.File
	err  error
)

// FieldtrialFile parses the embedded list once. The returned value is shared
// and must be treated as read-only; callers only read targets and corridors.
func FieldtrialFile() (*config.File, error) {
	once.Do(func() {
		file, err = config.Parse(FieldtrialTargets)
	})
	return file, err
}
