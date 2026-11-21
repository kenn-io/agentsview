package mcp

import (
	"encoding/json"
	"slices"
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
				for _, tc := range []struct{ name, args, corrected string }{
					{"lone surrogate", `{"query":"example","include_one_shot":"\ud800"}`, `{"query":"example"}`},
					{"plain", `{"query":"example","include_one_shot":true}`, `{"query":"example"}`},
					{"duplicate name", `{"query":"first","query":"example","include_one_shot":true}`, `{"query":"example"}`},
				} {
					t.Run(tc.name, func(t *testing.T) {
						result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: ToolSearchSessions, Arguments: json.RawMessage(tc.args)})
						require.NoError(t, err)
						require.True(t, result.IsError)
						require.Len(t, result.Content, 2)
						assert.Contains(t, result.Content[0].(*mcp.TextContent).Text, `unexpected additional properties ["include_one_shot"]`)
						assert.Equal(t, "Unknown arguments: include_one_shot. "+searchHint, result.Content[1].(*mcp.TextContent).Text)
						result, err = client.CallTool(t.Context(), &mcp.CallToolParams{Name: ToolSearchSessions, Arguments: json.RawMessage(tc.corrected)})
						require.NoError(t, err)
						assert.False(t, result.IsError, "%+v", result.Content)
					})
				}
				t.Run("unknown and wrong type", func(t *testing.T) {
					result, err := client.CallTool(t.Context(), callParams(ToolSearchSessions, map[string]any{"limit": "10", "include_one_shot": true}))
					require.NoError(t, err)
					require.True(t, result.IsError)
					require.Len(t, result.Content, 2)
					assert.Contains(t, result.Content[0].(*mcp.TextContent).Text, "limit")
					assert.Equal(t, "Unknown arguments: include_one_shot. "+searchHint, result.Content[1].(*mcp.TextContent).Text)
				})
				for _, tc := range []struct {
					name, tool string
					args       map[string]any
				}{
					{"wrong type", ToolSearchSessions, map[string]any{"limit": "ten"}},
					{"missing session", ToolGetSessionOverview, map[string]any{"session_id": "missing"}},
					{"missing session with validator words", ToolGetSessionOverview, map[string]any{"session_id": "unexpected additional properties"}},
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
					assert.Contains(t, result.Content[0].(*mcp.TextContent).Text, "not_an_argument")
					hint := result.Content[1].(*mcp.TextContent).Text
					require.True(t, strings.HasPrefix(hint, "Unknown arguments: not_an_argument. "))
					hint = strings.TrimPrefix(hint, "Unknown arguments: not_an_argument. ")
					switch tool.Name {
					case ToolSearchSessions:
						assert.Equal(t, searchHint, hint)
					case ToolGetMemoryStatus:
						assert.Equal(t, "Accepted arguments: none.", hint)
					default:
						schema := tool.InputSchema.(map[string]any)
						properties := schema["properties"].(map[string]any)
						require.True(t, strings.HasPrefix(hint, "Accepted arguments: "))
						require.True(t, strings.HasSuffix(hint, "."))
						names := strings.Split(strings.TrimSuffix(strings.TrimPrefix(hint, "Accepted arguments: "), "."), ", ")
						assert.True(t, slices.IsSorted(names))
						assert.Len(t, names, len(properties))
						for _, name := range names {
							assert.Contains(t, properties, name)
						}
					}
				})
			}
		})
	}
}
