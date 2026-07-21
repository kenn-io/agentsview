package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveLegacyTraeNamespacedID(t *testing.T) {
	tests := []struct {
		name            string
		oldID           string
		filePath        string
		sourceSessionID string
		existing        map[string]bool
		want            string
	}{
		{
			name:            "path-derived workspace target must exist",
			oldID:           "trae:rewrite",
			filePath:        "/tmp/workspaceStorage/hash/state.vscdb#rewrite",
			sourceSessionID: "rewrite",
			existing:        map[string]bool{},
			want:            "",
		},
		{
			name:            "unknown path falls back to unique workspace target",
			oldID:           "trae:rewrite",
			filePath:        "/tmp/other/state.vscdb#rewrite",
			sourceSessionID: "rewrite",
			existing: map[string]bool{
				"trae:workspaceStorage:rewrite": true,
			},
			want: "trae:workspaceStorage:rewrite",
		},
		{
			name:            "unknown path preserves ambiguous legacy state",
			oldID:           "trae:rewrite",
			filePath:        "/tmp/other/state.vscdb#rewrite",
			sourceSessionID: "rewrite",
			existing: map[string]bool{
				"trae:workspaceStorage:rewrite": true,
				"trae:globalStorage:rewrite":    true,
			},
			want: "",
		},
		{
			name:            "host-prefixed legacy id keeps host on unique fallback",
			oldID:           "laptop~trae:rewrite",
			filePath:        "/tmp/other/state.vscdb#rewrite",
			sourceSessionID: "rewrite",
			existing: map[string]bool{
				"laptop~trae:globalStorage:rewrite": true,
			},
			want: "laptop~trae:globalStorage:rewrite",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveLegacyTraeNamespacedID(
				tc.oldID,
				tc.filePath,
				tc.sourceSessionID,
				func(id string) (bool, error) {
					return tc.existing[id], nil
				},
			)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
