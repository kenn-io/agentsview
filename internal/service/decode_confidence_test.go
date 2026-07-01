package service_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

// TestSessionDetailDecodeConfidenceJSON verifies the derive-on-read
// decode_confidence field the detail endpoint emits: it appears as "low" for
// an Antigravity session on an unrecognized schema, and is omitted for known
// schemas, empty labels, and other agents that carry a generic source_version.
func TestSessionDetailDecodeConfidenceJSON(t *testing.T) {
	tests := []struct {
		name          string
		agent         string
		sourceVersion string
		wantPresent   bool
		wantValue     string
	}{
		{
			name:          "antigravity unknown schema emits low",
			agent:         "antigravity",
			sourceVersion: "agy-schema:abc123def456",
			wantPresent:   true,
			wantValue:     "low",
		},
		{
			name:          "antigravity-cli unknown schema emits low",
			agent:         "antigravity-cli",
			sourceVersion: "agy-schema:abc123def456",
			wantPresent:   true,
			wantValue:     "low",
		},
		{
			name:          "antigravity known range omitted (high not surfaced as badge)",
			agent:         "antigravity",
			sourceVersion: "1.0.7-1.0.10",
			wantPresent:   true,
			wantValue:     "high",
		},
		{
			name:          "antigravity empty source_version omits field",
			agent:         "antigravity",
			sourceVersion: "",
			wantPresent:   false,
		},
		{
			name:          "piebald generic label omits field",
			agent:         "piebald",
			sourceVersion: "piebald-appdb-v1",
			wantPresent:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail := service.SessionDetail{
				Session: db.Session{
					ID:            "s1",
					Agent:         tt.agent,
					SourceVersion: tt.sourceVersion,
				},
			}
			raw, err := json.Marshal(detail)
			require.NoError(t, err)

			var decoded map[string]any
			require.NoError(t, json.Unmarshal(raw, &decoded))

			got, present := decoded["decode_confidence"]
			assert.Equal(t, tt.wantPresent, present,
				"decode_confidence presence")
			if tt.wantPresent {
				assert.Equal(t, tt.wantValue, got)
			}
		})
	}
}
