package logging_test

import "time"

// testTime is a fixed instant so log output is comparable.
func testTime() time.Time {
	return time.Date(2026, 11, 16, 3, 14, 15, 0, time.UTC)
}
