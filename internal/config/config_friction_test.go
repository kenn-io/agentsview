package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrictionConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  FrictionConfig
		wantErr string
	}{
		{name: "defaults", config: FrictionConfig{Enabled: true, BackfillDays: 7}},
		{name: "zone", config: FrictionConfig{Enabled: true, Timezone: "Asia/Tokyo", BackfillDays: 30}},
		{name: "empty zone means local", config: FrictionConfig{Enabled: true, BackfillDays: 1}},
		{name: "upper bound", config: FrictionConfig{BackfillDays: 3650}},
		{name: "seat pattern", config: FrictionConfig{BackfillDays: 7, SeatPatterns: []string{"*/profiles/{seat}/projects/*"}}},
		{name: "bad zone", config: FrictionConfig{Timezone: "Mars/Olympus", BackfillDays: 7}, wantErr: `[friction] timezone "Mars/Olympus" is not an IANA zone`},
		{name: "local is not an IANA zone", config: FrictionConfig{Timezone: "Local", BackfillDays: 7}, wantErr: `[friction] timezone "Local" is not an IANA zone`},
		{name: "zero backfill", config: FrictionConfig{BackfillDays: 0}, wantErr: "[friction] backfill_days must be between 1 and 3650"},
		{name: "huge backfill", config: FrictionConfig{BackfillDays: 3651}, wantErr: "[friction] backfill_days must be between 1 and 3650"},
		{
			name: "seat pattern without capture", config: FrictionConfig{BackfillDays: 7, SeatPatterns: []string{"*/profiles/*"}},
			wantErr: `[friction] seat_patterns entry "*/profiles/*" must contain exactly one {seat}`,
		},
		{
			name: "seat pattern with two captures", config: FrictionConfig{BackfillDays: 7, SeatPatterns: []string{"{seat}/{seat}"}},
			wantErr: `[friction] seat_patterns entry "{seat}/{seat}" must contain exactly one {seat}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tt.wantErr, err.Error())
		})
	}
}

func TestFrictionConfigTOMLLoadAndFinalize(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want FrictionConfig
	}{
		{
			name: "absent section means enabled", data: map[string]any{},
			want: FrictionConfig{Enabled: true, BackfillDays: DefaultFrictionBackfillDays},
		},
		{
			name: "all keys trimmed",
			data: map[string]any{"friction": map[string]any{
				"enabled": true, "timezone": " Europe/Berlin ", "backfill_days": 14,
				"seat_patterns": []any{" */profiles/{seat}/projects/* "},
			}},
			want: FrictionConfig{
				Enabled: true, Timezone: "Europe/Berlin", BackfillDays: 14,
				SeatPatterns: []string{"*/profiles/{seat}/projects/*"},
			},
		},
		{
			name: "partial table keeps defaults",
			data: map[string]any{"friction": map[string]any{"timezone": "UTC"}},
			want: FrictionConfig{Enabled: true, Timezone: "UTC", BackfillDays: DefaultFrictionBackfillDays},
		},
		{
			name: "explicit false disables",
			data: map[string]any{"friction": map[string]any{"enabled": false}},
			want: FrictionConfig{Enabled: false, BackfillDays: DefaultFrictionBackfillDays},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.want.Kata = FrictionKataConfig{
				ReopenOnRecurrence: true,
				Kinds:              []string{"correction", "error", "workaround", "deferral", "pattern"},
			}
			cfg := loadMinimalWithConfig(t, tt.data)
			assert.Equal(t, tt.want, cfg.Friction)
		})
	}

	err := loadMinimalErrWithConfig(t, map[string]any{
		"friction": map[string]any{"timezone": "Nowhere/Land"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `[friction] timezone "Nowhere/Land" is not an IANA zone`)
}
