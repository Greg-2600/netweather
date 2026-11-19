//go:build !linux && !windows

package probe

import (
	"context"
	"fmt"
)

// PMTU is a stub on non-Linux platforms: DF-bit probing needs raw socket
// options that are not portable. The interface MTU self-check remains the
// honest fallback.
type PMTU struct {
	Timeout int
	Size    int
}

func NewPMTU() *PMTU { return &PMTU{Timeout: 3, Size: 1400} }

func (p *PMTU) Do(ctx context.Context, address string) Result {
	return Result{ErrorClass: "pmtu_unsupported", Err: fmt.Errorf("probe: pmtu not supported on this platform")}
}
