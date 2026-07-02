package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestSessionDetailDecodeConfidence verifies the derive-on-read
// decode_confidence field the detail endpoint emits. buildSessionDetail derives
// it once from the session's agent and source_version, MarshalJSON passes the
// stored field through, and UnmarshalJSON restores it so the HTTP backend
// round-trips it. It is "low" for an Antigravity session on an unrecognized
// schema, "high" for a known range, and omitted for empty labels and other
// agents that carry a generic source_version.
func TestSessionDetailDecodeConfidence(t *testing.T) {
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
			name:          "antigravity known range emits high",
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
			detail := buildSessionDetail(&db.Session{
				ID:            "s1",
				Agent:         tt.agent,
				SourceVersion: tt.sourceVersion,
			})
			if tt.wantPresent {
				assert.Equal(t, tt.wantValue, detail.DecodeConfidence,
					"buildSessionDetail should derive DecodeConfidence")
			} else {
				assert.Empty(t, detail.DecodeConfidence,
					"DecodeConfidence should be empty")
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

			// The HTTP backend decodes the response into a SessionDetail, so the
			// field must round-trip rather than being dropped.
			var roundTripped SessionDetail
			require.NoError(t, json.Unmarshal(raw, &roundTripped))
			assert.Equal(t, detail.DecodeConfidence,
				roundTripped.DecodeConfidence,
				"decode_confidence should round-trip through UnmarshalJSON")
		})
	}
}
