package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadFile_PGLegacyFieldNamesRejectNestedTablesCaseInsensitive(t *testing.T) {
	tests := []struct {
		name    string
		field   string
		wantErr string
	}{
		{
			name:    "url",
			field:   "URL",
			wantErr: "[pg].URL must be a scalar or array field, not a nested table",
		},
		{
			name:    "schema",
			field:   "Schema",
			wantErr: "[pg].Schema must be a scalar or array field, not a nested table",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parsePGConfigSection(map[string]any{
				tt.field: map[string]any{
					"url": "postgres://nested",
				},
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
