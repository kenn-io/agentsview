package mcp

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

const skillsCatalogJSON = `{"skills":[{"name":"agentsview-finding-history","title":"Finding conversation history","summary":"Search prior conversations and read source messages before drawing conclusions."}]}`

func TestSkillsCatalogAndGuide(t *testing.T) {
	for _, profile := range []Profile{ProfileFull, ProfileMemory} {
		t.Run(string(profile), func(t *testing.T) {
			// No archive service: listing and reading shipped guidance needs none.
			ss, cs := newInMemoryPair(t, newServer(ServeOptions{Profile: profile}))
			t.Cleanup(func() {
				require.NoError(t, cs.Close())
				require.NoError(t, ss.Wait())
			})
			assert.Contains(t, cs.InitializeResult().Instructions, "agentsview.skills")
			assert.Contains(t, cs.InitializeResult().Instructions, "web_url")
			tools, err := cs.ListTools(t.Context(), nil)
			require.NoError(t, err)
			found := false
			for _, tool := range tools.Tools {
				if tool.Name == "agentsview.skills" {
					found = true
					require.NotNil(t, tool.Annotations)
					assert.True(t, tool.Annotations.ReadOnlyHint)
				}
			}
			assert.True(t, found, "bundled guide tool must always be discoverable")
			for i, args := range []any{nil, map[string]any{}, map[string]any{"name": "agentsview-finding-history"}} {
				result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "agentsview.skills", Arguments: args})
				require.NoError(t, err)
				require.False(t, result.IsError)
				raw, err := json.Marshal(result.StructuredContent)
				require.NoError(t, err)
				assert.JSONEq(t, skillsCatalogJSON, string(raw))
				require.Len(t, result.Content, 1)
				text, ok := result.Content[0].(*mcp.TextContent)
				require.True(t, ok)
				if i < 2 {
					assert.Contains(t, text.Text, "agentsview-finding-history: Finding conversation history.")
					assert.NotContains(t, text.Text, "## Workflow")
				} else {
					assert.Contains(t, text.Text, "# Finding Conversation History")
					assert.Contains(t, text.Text, "## Workflow")
					assert.Contains(t, text.Text, "## MCP differences")
					assert.NotContains(t, text.Text, "# generated-by:")
				}
			}
		})
	}
}

func TestSkillsRejectsInvalidLookups(t *testing.T) {
	ss, cs := newInMemoryPair(t, newServer(ServeOptions{Profile: ProfileMemory}))
	t.Cleanup(func() {
		require.NoError(t, cs.Close())
		require.NoError(t, ss.Wait())
	})
	for _, args := range []map[string]any{
		{"name": "missing"}, {"name": "../../SKILL.md"}, {"name": "/etc/passwd"},
		{"name": ""}, {"name": nil}, {"name": 1}, {"path": "SKILL.md"},
	} {
		_, err := cs.CallTool(t.Context(), callParams("agentsview.skills", args))
		require.Error(t, err, "arguments: %v", args)
		var wire *jsonrpc.Error
		require.ErrorAs(t, err, &wire)
		assert.EqualValues(t, -32602, wire.Code)
	}
}

func TestSkillsUnknownNamesCannotChangeCatalog(t *testing.T) {
	for _, profile := range []Profile{ProfileFull, ProfileMemory} {
		t.Run(string(profile), func(t *testing.T) {
			ss, cs := newInMemoryPair(t, newServer(ServeOptions{Profile: profile}))
			t.Cleanup(func() {
				require.NoError(t, cs.Close())
				require.NoError(t, ss.Wait())
			})
			hegel.Test(t, func(ht *hegel.T) {
				name := hegel.Draw(ht, hegel.Text())
				ht.Assume(name != "agentsview-finding-history")
				_, err := cs.CallTool(t.Context(), callParams("agentsview.skills", map[string]any{"name": name}))
				require.Error(ht, err)
				result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "agentsview.skills"})
				require.NoError(ht, err)
				raw, err := json.Marshal(result.StructuredContent)
				require.NoError(ht, err)
				assert.JSONEq(ht, skillsCatalogJSON, string(raw))
			}, hegel.WithDatabase(""))
		})
	}
}

func TestSkillsHTTPAuthentication(t *testing.T) {
	srv := httptest.NewServer(newHTTPHandler(ServeOptions{Token: "fixture-token", Profile: ProfileMemory}))
	t.Cleanup(srv.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	response, err := srv.Client().Do(request)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	require.NoError(t, response.Body.Close())
	client := mcp.NewClient(&mcp.Implementation{Name: "skills-test", Version: "v0"}, nil)
	_, err = client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	require.Error(t, err)
	httpClient := &http.Client{Transport: skillsBearerTransport{}}
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: httpClient}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cs.Close()) })
	result, err := cs.CallTool(t.Context(), callParams("agentsview.skills", map[string]any{"name": "agentsview-finding-history"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Len(t, result.Content, 1)
	assert.Contains(t, result.Content[0].(*mcp.TextContent).Text, "# Finding Conversation History")
}

type skillsBearerTransport struct{}

func (skillsBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer fixture-token")
	return http.DefaultTransport.RoundTrip(r)
}
