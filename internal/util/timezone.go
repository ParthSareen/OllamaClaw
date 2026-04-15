package util

import (
	"sync"
	"time"
	_ "time/tzdata"
)

const PacificTimezoneName = "America/Los_Angeles"

var (
	pacificLocationOnce sync.Once
	pacificLocation     *time.Location
)

func PacificLocation() *time.Location {
	pacificLocationOnce.Do(func() {
		loc, err := time.LoadLocation(PacificTimezoneName)
		if err != nil {
			// Fallback only if tzdata lookup fails.
			loc = time.FixedZone("PST", -8*60*60)
		}
		pacificLocation = loc
	})
	return pacificLocation
}

func ToPacific(t time.Time) time.Time {
	return t.In(PacificLocation())
}

func FormatPacificRFC3339(t time.Time) string {
	return ToPacific(t).Format(time.RFC3339)
}
