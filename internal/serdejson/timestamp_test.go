package serdejson

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Expected strings are chrono 0.4 to_rfc3339_opts(SecondsFormat::AutoSi, true).
func TestFormatTimestamp(t *testing.T) {
	base := time.Date(2026, 9, 22, 1, 2, 3, 0, time.UTC)
	tests := []struct {
		name string
		ns   int
		want string
	}{
		{"whole second", 0, "2026-09-22T01:02:03Z"},
		{"millis", 5_000_000, "2026-09-22T01:02:03.005Z"},
		{"micros", 120_000, "2026-09-22T01:02:03.000120Z"},
		{"nanos", 123_456_789, "2026-09-22T01:02:03.123456789Z"},
		{"small nanos", 100, "2026-09-22T01:02:03.000000100Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := base.Add(time.Duration(tt.ns))
			assert.Equal(t, tt.want, FormatTimestamp(ts))
			assert.Equal(t, tt.want, FormatTimestamp(ts.In(time.FixedZone("x", 9*3600))))
		})
	}
}
