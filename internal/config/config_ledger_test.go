package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLedgerConfigValidate(t *testing.T) {
	no := false
	abs := t.TempDir()
	tests := []struct {
		name    string
		config  LedgerConfig
		wantErr string
	}{
		{name: "absent"},
		{name: "enabled_defaults", config: LedgerConfig{Enabled: true}},
		{name: "zones", config: LedgerConfig{Enabled: true, Zones: []LedgerZoneConfig{
			{ID: "default"}, {ID: "ops", Replicate: &no, ImportPath: abs},
		}}},
		{name: "tilde_import_path", config: LedgerConfig{Zones: []LedgerZoneConfig{{ID: "ops", ImportPath: "~/ledger"}}}},
		{name: "bad_source", config: LedgerConfig{Source: "../host"}, wantErr: "[ledger] source"},
		{name: "bad_default_zone", config: LedgerConfig{DefaultZone: "a b"}, wantErr: "[ledger] default_zone"},
		{
			name:    "zone_id_unsafe_as_path",
			config:  LedgerConfig{Zones: []LedgerZoneConfig{{ID: "../x"}}},
			wantErr: "[ledger.zones] id must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$",
		},
		{name: "empty_zone_id", config: LedgerConfig{Zones: []LedgerZoneConfig{{ID: ""}}}, wantErr: "[ledger.zones] id must match"},
		{
			name:    "duplicate_zone_id",
			config:  LedgerConfig{Zones: []LedgerZoneConfig{{ID: "ops"}, {ID: "ops"}}},
			wantErr: `[ledger.zones] duplicate id "ops"`,
		},
		{
			name:    "relative_import_path",
			config:  LedgerConfig{Zones: []LedgerZoneConfig{{ID: "ops", ImportPath: "ledger/ops"}}},
			wantErr: "[ledger.zones] import_path \"ledger/ops\" must be an absolute path",
		},
		{
			name:    "relative_spool_path",
			config:  LedgerConfig{Zones: []LedgerZoneConfig{{ID: "ops", SpoolPath: "spool"}}},
			wantErr: "[ledger.zones] spool_path",
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
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestLedgerConfigDerivedValues(t *testing.T) {
	no := false
	c := LedgerConfig{Zones: []LedgerZoneConfig{{ID: "ops", Replicate: &no}, {ID: "default"}}}
	assert.Equal(t, "av-0123456789abcdef0123456789abcdef", c.EffectiveSource("0123456789abcdef0123456789abcdef"))
	assert.Equal(t, "custom", LedgerConfig{Source: "custom"}.EffectiveSource("x"))
	assert.Equal(t, "default", c.EffectiveDefaultZone())
	assert.Equal(t, []string{"default", "ops"}, c.ZoneIDs(), "the default zone comes first")
	assert.Equal(t, []string{"main"}, LedgerConfig{DefaultZone: "main"}.ZoneIDs())

	ops, ok := c.Zone("ops")
	require.True(t, ok)
	assert.False(t, ops.Replicates())
	def, ok := c.Zone("default")
	require.True(t, ok)
	assert.True(t, def.Replicates(), "replicate defaults to true (jilog zone_spool_flag_defaults_true)")
	_, ok = c.Zone("nope")
	assert.False(t, ok)

	root := t.TempDir()
	dir, err := LedgerZoneConfig{ImportPath: root}.ImportSegmentsDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "segments"), dir)
	none, err := LedgerZoneConfig{}.ImportSegmentsDir()
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestLedgerConfigTOMLLoadAndFinalize(t *testing.T) {
	importPath := t.TempDir()
	cfg := loadMinimalWithConfig(t, map[string]any{
		"ledger": map[string]any{
			"enabled":                true,
			"source":                 " host-a ",
			"replicate_confidential": true,
			"zones": []map[string]any{
				{"id": " default "},
				{"id": "ops", "replicate": false, "import_path": importPath},
			},
		},
	})
	assert.True(t, cfg.Ledger.Enabled)
	assert.Equal(t, "host-a", cfg.Ledger.Source)
	assert.True(t, cfg.Ledger.ReplicateConfidential)
	require.Len(t, cfg.Ledger.Zones, 2)
	assert.Equal(t, "default", cfg.Ledger.Zones[0].ID)
	assert.False(t, cfg.Ledger.Zones[1].Replicates())
	assert.Equal(t, importPath, cfg.Ledger.Zones[1].ImportPath)

	off := loadMinimalWithConfig(t, map[string]any{})
	assert.False(t, off.Ledger.Enabled, "the ledger is opt-in")

	err := loadMinimalErrWithConfig(t, map[string]any{
		"ledger": map[string]any{"zones": []map[string]any{{"id": "a"}, {"id": "a"}}},
	})
	require.ErrorContains(t, err, `[ledger.zones] duplicate id "a"`)
}
