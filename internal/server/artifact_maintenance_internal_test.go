package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseArtifactMaintenanceDuration(t *testing.T) {
	seconds := func(value int64) *int64 { return &value }
	cases := []struct {
		name    string
		exact   string
		legacy  *int64
		want    time.Duration
		wantErr bool
	}{
		{name: "exact nanoseconds", exact: "1.5us", want: 1500 * time.Nanosecond},
		{name: "legacy seconds", legacy: seconds(7), want: 7 * time.Second},
		{name: "absent", want: 0},
		{name: "conflicting exact and legacy", exact: "1s", legacy: seconds(1), wantErr: true},
		{name: "negative exact", exact: "-1ns", wantErr: true},
		{name: "negative legacy", legacy: seconds(-1), wantErr: true},
		{name: "overflowing legacy", legacy: seconds(maxArtifactMaintenanceGraceSeconds + 1), wantErr: true},
		{name: "invalid exact", exact: "tomorrow", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseArtifactMaintenanceDuration(tc.exact, tc.legacy)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
