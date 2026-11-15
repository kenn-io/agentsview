package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
)

func TestPGActivityInputBytesIncludesPrices(t *testing.T) {
	raw := pgActivityInputs{}
	withPrices := pgActivityInputs{priced: &pgActivityPriced{rows: make([]pgActivityPricedRow, 2), block: &export.PricingBlock{}}}
	assert.Greater(t, pgActivityInputBytes(withPrices), pgActivityInputBytes(raw))
}

func TestPGActivityPricedCovers(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	effective := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	next := effective.Add(time.Hour)
	priced := pgActivityPriced{rangeStart: start, rangeEnd: end, effectiveEnd: effective, next: next, pricingDigest: "prices"}
	for _, tc := range []struct {
		name   string
		query  activity.Query
		digest string
		want   bool
	}{
		{"same", activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: effective}, "prices", true},
		{"advance to row", activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: next}, "prices", true},
		{"advance past row", activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: next.Add(time.Microsecond)}, "prices", false},
		{"rewind", activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: effective.Add(-time.Microsecond)}, "prices", false},
		{"start changes", activity.Query{RangeStart: start.Add(time.Microsecond), RangeEnd: end, EffectiveEnd: effective}, "prices", false},
		{"end changes", activity.Query{RangeStart: start, RangeEnd: end.Add(time.Microsecond), EffectiveEnd: effective}, "prices", false},
		{"prices change", activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: effective}, "new prices", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, priced.covers(tc.query, tc.digest))
		})
	}
	priced.next = time.Time{}
	assert.True(t, priced.covers(activity.Query{RangeStart: start, RangeEnd: end, EffectiveEnd: end}, "prices"))
}
