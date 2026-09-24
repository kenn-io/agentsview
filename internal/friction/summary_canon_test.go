package friction

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalSummaryJSON(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "golden", "summary.json"))
	require.NoError(t, err)
	tests := []struct {
		name string
		in   []byte
	}{
		{"identity_on_golden", golden},
		{"compact_reencoding", []byte(`{"schema_version":3,"frustrations":0,"interruptions":0,"corrections":0,"errors":0,"workarounds":0,"deferrals":0,"patterns":0,"p0_alerts":{"bash":["session-a","session-b"]},"sessions_scanned":3,"tracker_failures":0,"spend":null,"created_issues":[],"personas":{},"digest_path":null}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := CanonicalSummaryJSON(tt.in)
			require.NoError(t, err)
			assert.Equal(t, byte('\n'), out[len(out)-1])
			again, err := CanonicalSummaryJSON(out)
			require.NoError(t, err)
			assert.Equal(t, out, again, "canonical form is a fixed point")
		})
	}
	out, err := CanonicalSummaryJSON(golden)
	require.NoError(t, err)
	assert.Equal(t, string(golden), string(out))
	_, err = CanonicalSummaryJSON([]byte("{"))
	require.Error(t, err)
}
