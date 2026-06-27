package analyticscope

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMatchesDayHour(t *testing.T) {
	// 2024-01-01 is a Monday (ISO day 0); hour 14 UTC.
	mon14 := time.Date(2024, 1, 1, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		f    Filter
		t    time.Time
		has  bool
		want bool
	}{
		{"no constraint matches even unparsed", Filter{}, time.Time{}, false, true},
		{"day match", Filter{DayOfWeek: new(0)}, mon14, true, true},
		{"day mismatch", Filter{DayOfWeek: new(1)}, mon14, true, false},
		{"hour match", Filter{Hour: new(14)}, mon14, true, true},
		{"hour mismatch", Filter{Hour: new(9)}, mon14, true, false},
		{"constraint but unparsed never matches", Filter{Hour: new(14)}, time.Time{}, false, false},
		{"day and hour both match", Filter{DayOfWeek: new(0), Hour: new(14)}, mon14, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.f.MatchesDayHour(tc.t, tc.has))
		})
	}
}
