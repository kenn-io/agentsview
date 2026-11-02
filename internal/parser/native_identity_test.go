package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexFormatRetainsNativeIdentityEvidence(t *testing.T) {
	const thread = "11111111-1111-4111-8111-111111111111"
	const page = "22222222-2222-4222-8222-222222222222"
	for _, agent := range []AgentType{AgentCodex, AgentTraeX, AgentAugureCode} {
		for _, tc := range []struct{ name, key, meta, want string }{
			{"native", thread, `{"id":"` + thread + `"}`, thread},
			{"filename_only", thread, `{}`, ""},
			{"page", thread + "_" + page, `{"id":"` + thread + `"}`, thread + "_" + page},
			{"older_page", thread + "_" + page, `{"id":"` + page + `","session_id":"` + thread + `"}`, thread + "_" + page},
		} {
			t.Run(string(agent)+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "rollout-2026-01-01T00-00-00-"+tc.key+".jsonl")
				body := `{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":` + tc.meta + `}` + "\n" +
					`{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n"
				require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
				provider, ok := NewProvider(agent, ProviderConfig{Roots: []string{root}, Machine: "fixture"})
				require.True(t, ok)
				sources, err := provider.Discover(t.Context())
				require.NoError(t, err)
				require.Len(t, sources, 1)
				out, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], ForceParse: true})
				require.NoError(t, err)
				require.Len(t, out.Results, 1)
				assert.Equal(t, tc.want, out.Results[0].Result.Session.SourceSessionID)
				if tc.want != "" {
					assert.Equal(t, string(agent)+":"+tc.want, out.Results[0].Result.Session.ID)
				}
			})
		}
	}
}
