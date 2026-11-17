package board

import "sort"

// SeverityRank orders states by how much attention they need. Lower is worse:
// the grid exists to answer "what is down?", so failures come first.
func SeverityRank(s State) int {
	switch s {
	case StateDown:
		return 0
	case StateDegraded:
		return 1
	case StateBlocked:
		return 2
	case StateUnknown:
		return 3
	case StateConfigError:
		return 4
	case StateUnmeasurable:
		return 5
	case StateUpReachability:
		return 6
	case StateUp:
		return 7
	default:
		return 8
	}
}

// Order returns a copy sorted by severity, then name, then host. The input is
// never mutated: the API keeps source order, while the UI clusters by
// availability.
func Order(targets []Target) []Target {
	out := append([]Target(nil), targets...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := SeverityRank(out[i].State), SeverityRank(out[j].State)
		if ri != rj {
			return ri < rj
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Host < out[j].Host
	})
	return out
}
