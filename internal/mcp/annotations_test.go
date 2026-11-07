package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestToolAnnotations(t *testing.T) {
	for _, profile := range []Profile{ProfileFull, ProfileMemory} {
		t.Run(string(profile), func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			srv := newServer(ServeOptions{
				Service: service.NewDirectBackend(d, nil, withTestDocbank()),
				Profile: profile,
			})
			server, client := newInMemoryPair(t, srv)
			t.Cleanup(func() {
				require.NoError(t, client.Close())
				require.NoError(t, server.Wait())
			})
			result, err := client.ListTools(t.Context(), nil)
			require.NoError(t, err)
			for _, tool := range result.Tools {
				t.Run(tool.Name, func(t *testing.T) {
					assert := assert.New(t)
					annotations := tool.Annotations
					require.NotNil(t, annotations)
					require.NotNil(t, annotations.DestructiveHint)
					require.NotNil(t, annotations.OpenWorldHint)
					assert.True(annotations.ReadOnlyHint)
					assert.False(*annotations.DestructiveHint)
					assert.True(annotations.IdempotentHint)
					// Semantic search can contact an embedding provider; file
					// references query an independently configured Docbank.
					assert.Equal(tool.Name == ToolSearchContent || tool.Name == ToolGetSessionFileReferences,
						*annotations.OpenWorldHint)
				})
			}
		})
	}
}
