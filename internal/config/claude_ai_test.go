package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeAIOrganizations(t *testing.T) {
	const org = "11111111-1111-4111-8111-111111111111"
	for _, tt := range []struct {
		name    string
		toml    string
		want    []string
		wantErr string
	}{
		{name: "absent table"},
		{name: "table without organizations", toml: "[agents.claude-ai]\n"},
		{name: "empty list", toml: "[agents.claude-ai]\norganizations = []\n", want: []string{}},
		{name: "misspelled key", toml: "[agents.claude-ai]\norganisations = [\"" + org + "\"]\n", wantErr: `unknown setting "organisations"`},
		{
			name: "normalized",
			toml: "[agents.claude-ai]\norganizations = [\" AAAAAAAA-1111-4111-8111-111111111111 \", \"" + org + "\"]\n",
			want: []string{"aaaaaaaa-1111-4111-8111-111111111111", org},
		},
		{name: "single string", toml: "[agents.claude-ai]\norganizations = \"" + org + "\"\n", wantErr: "expected an array of organization UUIDs"},
		{name: "blank entry", toml: "[agents.claude-ai]\norganizations = [\" \"]\n", wantErr: "expected non-empty organization UUID strings"},
		{name: "non-string entry", toml: "[agents.claude-ai]\norganizations = [1]\n", wantErr: "expected non-empty organization UUID strings"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestEnv(t)
			if tt.toml != "" {
				writeRawConfig(t, dir, tt.toml)
			}
			cfg, err := LoadMinimal()
			require.NoError(t, err)
			got := cfg.ClaudeAI
			if tt.wantErr != "" {
				require.NotNil(t, got.Err)
				assert.Contains(t, got.Err.Error(), tt.wantErr)
				assert.Contains(t, got.Err.Error(), "Claude.ai Sync stays off")
				return
			}
			require.Nil(t, got.Err)
			assert.Equal(t, tt.want, got.Organizations)
		})
	}
}
