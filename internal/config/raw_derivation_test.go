package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawDerivationConfigBindingAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		p     PGConfig
		auth  bool
		valid bool
	}{
		{"off", PGConfig{}, false, true},
		{"missing tenant", PGConfig{RawDerivation: true}, true, false},
		{"missing auth", PGConfig{RawTenant: "tenant", RawDerivation: true, Schema: "hosted"}, false, false},
		{"valid", PGConfig{RawTenant: "tenant", RawDerivation: true, Schema: "hosted"}, true, true},
		{"negative poll", PGConfig{RawTenant: "tenant", RawDerivation: true, Schema: "hosted", RawPollSeconds: -1}, true, false},
		{"unbounded attempt", PGConfig{RawTenant: "tenant", RawDerivation: true, Schema: "hosted", RawAttemptSeconds: 301}, true, false},
		{"unbounded retries", PGConfig{RawTenant: "tenant", RawDerivation: true, Schema: "hosted", RawMaxAttempts: 11}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.ValidateRawDerivation(tc.auth)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	legacy, targets, err := parsePGConfigSection(map[string]any{"raw_tenant": "tenant", "raw_derivation": true, "raw_max_attempts": int64(3)})
	require.NoError(t, err)
	assert.Empty(t, targets)
	assert.Equal(t, "tenant", legacy.RawTenant)
	assert.True(t, legacy.RawDerivation)
	assert.Equal(t, 3, legacy.RawMaxAttempts)
	_, targets, err = parsePGConfigSection(map[string]any{"remote": map[string]any{"raw_tenant": "tenant", "raw_derivation": true}})
	require.NoError(t, err)
	assert.True(t, targets["remote"].RawDerivation)
	_, _, err = parsePGConfigSection(map[string]any{"raw_tenant": map[string]any{"url": "invalid"}})
	assert.Error(t, err)
}
