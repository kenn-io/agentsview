package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestToolArgumentHints(t *testing.T) {
	const searchHint = "Accepted arguments: cursor, date_from, date_to, include_active, limit, project, query, session_id, sort."
	for _, profile := range []Profile{ProfileFull, ProfileMemory} {
		t.Run(string(profile), func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			srv := newServer(ServeOptions{Service: service.NewDirectBackend(d, nil, withTestDocbank()), Profile: profile})
			server, client := newInMemoryPair(t, srv)
			t.Cleanup(func() {
				require.NoError(t, client.Close())
				require.NoError(t, server.Wait())
			})
			if profile == ProfileFull {
				for _, tc := range []struct{ name, args string }{
					{"lone surrogate", `{"query":"example","include_one_shot":"\ud800"}`},
					{"duplicate name", `{"query":"first","query":"example","include_one_shot":true}`},
				} {
					t.Run(tc.name, func(t *testing.T) {
						result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: ToolSearchSessions, Arguments: json.RawMessage(tc.args)})
						require.NoError(t, err)
						require.True(t, result.IsError)
						require.Len(t, result.Content, 2)
						assert.NotEmpty(t, result.Content[0].(*mcp.TextContent).Text)
						assert.Equal(t, "Unknown arguments: include_one_shot. "+searchHint, result.Content[1].(*mcp.TextContent).Text)
					})
				}
				t.Run("unknown and wrong type", func(t *testing.T) {
					result, err := client.CallTool(t.Context(), callParams(ToolSearchSessions, map[string]any{"limit": "10", "include_one_shot": true}))
					require.NoError(t, err)
					require.True(t, result.IsError)
					require.Len(t, result.Content, 2)
					assert.NotEmpty(t, result.Content[0].(*mcp.TextContent).Text)
					assert.Equal(t, "Unknown arguments: include_one_shot. "+searchHint, result.Content[1].(*mcp.TextContent).Text)
				})
				for _, tc := range []struct {
					name, tool string
					args       map[string]any
				}{
					{"wrong type", ToolSearchSessions, map[string]any{"limit": "ten"}},
					{"missing session", ToolGetSessionOverview, map[string]any{"session_id": "missing"}},
				} {
					t.Run(tc.name, func(t *testing.T) {
						result, err := client.CallTool(t.Context(), callParams(tc.tool, tc.args))
						require.NoError(t, err)
						require.True(t, result.IsError)
						require.Len(t, result.Content, 1)
						assert.NotContains(t, result.Content[0].(*mcp.TextContent).Text, "Accepted arguments:")
					})
				}
			}
			list, err := client.ListTools(t.Context(), nil)
			require.NoError(t, err)
			for _, tool := range list.Tools {
				t.Run(tool.Name, func(t *testing.T) {
					result, err := client.CallTool(t.Context(), callParams(tool.Name, map[string]any{"not_an_argument": true}))
					require.NoError(t, err)
					require.True(t, result.IsError)
					require.Len(t, result.Content, 2)
					hint := result.Content[1].(*mcp.TextContent).Text
					switch tool.Name {
					case ToolSearchSessions:
						assert.Equal(t, "Unknown arguments: not_an_argument. "+searchHint, hint)
					case ToolGetMemoryStatus:
						assert.Equal(t, "Unknown arguments: not_an_argument. Accepted arguments: none.", hint)
					default:
						assert.True(t, strings.HasPrefix(hint, "Unknown arguments: not_an_argument. Accepted arguments: "), hint)
					}
				})
			}
		})
	}
}
