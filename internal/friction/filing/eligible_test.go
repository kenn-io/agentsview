package filing

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEligibleHost(t *testing.T) {
	for _, tt := range []struct {
		name                      string
		pgServe, pushTarget, want bool
	}{
		{"pg serve hub", true, false, true},
		{"pg serve with push config", true, true, true},
		{"standalone", false, false, true},
		{"pusher", false, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, EligibleHost(tt.pgServe, tt.pushTarget)) })
	}
}
