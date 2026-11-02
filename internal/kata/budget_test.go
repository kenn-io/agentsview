package kata

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestProbeBudget(t *testing.T) {
	tests := []struct {
		name       string
		perRequest time.Duration
		allowance  time.Duration
		want       time.Duration
	}{
		{name: "default with discovery allowance", allowance: 5 * time.Second, want: 35 * time.Second},
		{name: "default with CLI allowance", allowance: 10 * time.Second, want: 40 * time.Second},
		{name: "configured with discovery allowance", perRequest: 20 * time.Second, allowance: 5 * time.Second, want: 65 * time.Second},
		{name: "configured with CLI allowance", perRequest: 20 * time.Second, allowance: 10 * time.Second, want: 70 * time.Second},
		{name: "negative uses default", perRequest: -time.Second, allowance: 5 * time.Second, want: 35 * time.Second},
		{name: "negative allowance is ignored", perRequest: 20 * time.Second, allowance: -time.Second, want: 60 * time.Second},
		{name: "overflow saturates", perRequest: time.Duration(1<<63 - 1), allowance: 10 * time.Second, want: time.Duration(1<<63 - 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ProbeBudget(tt.perRequest, tt.allowance))
		})
	}
}
