//go:build pgtest

package postgres

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawtest"
)

// A filename alias does not replace a missing provider identity. Identical
// real-provider results must coalesce only when that native key is present.
func TestRawClaudeProviderIdentityControlsDeviceCoalescing(t *testing.T) {
	for _, known := range []bool{false, true} {
		name := "missing_identity"
		if known {
			name = "native_identity"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := rawtest.Claude(t, root)
			if !known {
				body, err := os.ReadFile(path)
				require.NoError(t, err)
				body = []byte(strings.ReplaceAll(string(body), `,"sessionId":"`+rawtest.ClaudeID+`"`, ""))
				require.NoError(t, os.WriteFile(path, body, 0600))
			}
			p, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			sources, err := parser.DiscoverRawCaptureSources(t.Context(), p)
			require.NoError(t, err)
			require.Len(t, sources.Sources, 1)
			parsed, err := p.Parse(t.Context(), parser.ParseRequest{Source: sources.Sources[0], ForceParse: true})
			require.NoError(t, err)
			require.Len(t, parsed.Results, 1)
			expected := ""
			if known {
				expected = rawtest.ClaudeID
			}
			assert.Equal(t, expected, parsed.Results[0].Result.Session.SourceSessionID)
			f := newProjectionFixture(t)
			for _, device := range []string{"device-a", "device-b"} {
				m, _ := f.accept(t, device, "capture", "", parser.AgentClaude)
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, rawderive.ParsedManifest{Outcome: parsed}))
			}
			identity, err := f.sink.Resolve(t.Context(), rawtest.ClaudeID)
			require.NoError(t, err)
			var revisions, groups int
			require.NoError(t, f.runtime.QueryRow(`SELECT count(DISTINCT content_revision),count(DISTINCT group_id) FROM raw_content_revisions`).Scan(&revisions, &groups))
			assert.Equal(t, 1, revisions, "content hash remains deterministic across devices")
			if known {
				assert.Equal(t, RawIdentityUnique, identity.State)
				assert.Equal(t, 1, groups)
			} else {
				assert.Equal(t, RawIdentityAmbiguous, identity.State)
				assert.Equal(t, 2, groups)
				assert.Len(t, identity.Variants, 2)
			}
		})
	}
}
