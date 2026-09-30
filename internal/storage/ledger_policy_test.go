package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
)

func TestLedgerPushPolicyFor(t *testing.T) {
	no := false
	tests := []struct {
		name string
		cfg  config.LedgerConfig
		want *LedgerPushPolicy
	}{
		{"ledger_off", config.LedgerConfig{Zones: []config.LedgerZoneConfig{{ID: "default"}}}, nil},
		{"default_zone_only", config.LedgerConfig{Enabled: true}, &LedgerPushPolicy{Zones: []string{"default"}}},
		{
			"replicate_false_zones_are_left_out",
			config.LedgerConfig{Enabled: true, ReplicateConfidential: true, Zones: []config.LedgerZoneConfig{
				{ID: "ops"}, {ID: "private", Replicate: &no},
			}},
			&LedgerPushPolicy{Zones: []string{"default", "ops"}, ReplicateConfidential: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, LedgerPushPolicyFor(tt.cfg))
		})
	}
}
