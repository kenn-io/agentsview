// internal/config/config_friction_kata_test.go
package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrictionKataConfigDefaultsAndLoad(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want FrictionKataConfig
	}{
		{
			name: "defaults", data: map[string]any{},
			want: FrictionKataConfig{AutoFile: false, ReopenOnRecurrence: true, Kinds: []string{"correction", "error", "workaround", "deferral", "pattern"}},
		},
		{
			name: "partial_keeps_defaults", data: map[string]any{"friction": map[string]any{"kata": map[string]any{"auto_file": true}}},
			want: FrictionKataConfig{AutoFile: true, ReopenOnRecurrence: true, Kinds: []string{"correction", "error", "workaround", "deferral", "pattern"}},
		},
		{
			name: "all_keys", data: map[string]any{"friction": map[string]any{"kata": map[string]any{
				"auto_file": true, "reopen_on_recurrence": false, "kinds": []string{"error", "frustration", "interruption"},
			}}},
			want: FrictionKataConfig{AutoFile: true, ReopenOnRecurrence: false, Kinds: []string{"error", "frustration", "interruption"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadMinimalWithConfig(t, tt.data)
			assert.Equal(t, tt.want, cfg.Friction.Kata)
		})
	}
}

func TestFrictionKataConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		kinds   []string
		wantErr string
	}{
		{name: "all_seven", kinds: []string{"correction", "error", "workaround", "deferral", "pattern", "frustration", "interruption"}},
		{name: "empty_files_nothing"},
		{name: "unknown", kinds: []string{"error", "p0"}, wantErr: `[friction.kata] kinds entry "p0" is not a friction kind`},
		{name: "duplicate", kinds: []string{"error", "error"}, wantErr: `[friction.kata] kinds entry "error" repeats`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := FrictionKataConfig{Kinds: tt.kinds}.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
	err := loadMinimalErrWithConfig(t, map[string]any{"friction": map[string]any{"kata": map[string]any{"kinds": []string{"p0"}}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[friction.kata]")
}
