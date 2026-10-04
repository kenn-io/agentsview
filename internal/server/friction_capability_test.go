package server_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/server"
)

func TestVersionAdvertisesFriction(t *testing.T) {
	tests := []struct {
		name   string
		enable bool
	}{
		{name: "disabled", enable: false},
		{name: "enabled", enable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []server.Option
			te := setupWithServerOpts(t, nil)
			if tt.enable {
				runner := &review.Runner{Store: te.db, Loc: time.UTC, Now: time.Now, BackfillDays: 7}
				opts = append(opts, server.WithFriction(runner, nil))
				te = setupWithServerOpts(t, opts)
			}
			version := decode[map[string]any](t, te.get(t, "/api/v1/version"))
			assert.Equal(t, tt.enable, version["friction_available"])
		})
	}
}
