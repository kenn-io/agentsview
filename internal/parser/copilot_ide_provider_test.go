package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCopilotIDEProvidersOwnLegacyEntrypoints guards the fold: the
// provider-specific Discover/Find/Parse free functions (and the Visual Studio
// virtual-path splitter) must stay deleted, and neither the provider files nor
// their legacy source files may reach back into them as a shim. Discovery and
// lookup live on the provider source sets; parse lives on the provider methods;
// the Visual Studio virtual-path resolution is reproduced via the
// provider-neutral ParseVirtualSourcePath helper.
func TestCopilotIDEProvidersOwnLegacyEntrypoints(t *testing.T) {
	files := map[string]string{}
	for _, name := range []string{
		"discovery.go",
		"vscode_copilot.go",
		"vscode_copilot_provider.go",
		"visualstudio_copilot.go",
		"visualstudio_copilot_provider.go",
	} {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		files[name] = string(data)
	}

	deletedSymbols := []string{
		"func DiscoverVSCodeCopilotSessions",
		"func FindVSCodeCopilotSourceFile",
		"func ParseVSCodeCopilotSession",
		"func DiscoverVisualStudioCopilotSessions",
		"func FindVisualStudioCopilotSourceFile",
		"func ParseVisualStudioCopilotConversation",
		"func ParseVisualStudioCopilotSession",
		"func ParseVisualStudioCopilotVirtualPath",
	}
	for name, src := range files {
		for _, symbol := range deletedSymbols {
			assert.NotContainsf(t, src, symbol, "%s still defines %s", name, symbol)
		}
	}

	deletedCalls := []string{
		"DiscoverVSCodeCopilotSessions(",
		"FindVSCodeCopilotSourceFile(",
		"ParseVSCodeCopilotSession(",
		"DiscoverVisualStudioCopilotSessions(",
		"FindVisualStudioCopilotSourceFile(",
		"ParseVisualStudioCopilotConversation(",
		"ParseVisualStudioCopilotSession(",
		"ParseVisualStudioCopilotVirtualPath(",
	}
	for _, providerFile := range []string{
		"vscode_copilot_provider.go",
		"visualstudio_copilot_provider.go",
	} {
		for _, call := range deletedCalls {
			assert.NotContainsf(
				t, files[providerFile], call,
				"%s references removed legacy entrypoint %s", providerFile, call,
			)
		}
	}
}

func TestCopilotIDEProviderFactoriesReplaceLegacyAdapter(t *testing.T) {
	for _, agent := range []AgentType{AgentVSCodeCopilot, AgentVSCopilot} {
		t.Run(string(agent), func(t *testing.T) {
			factory, ok := ProviderFactoryByType(agent)
			require.True(t, ok)
			require.NotNil(t, factory)

			provider, ok := NewProvider(agent, ProviderConfig{
				Roots:   []string{t.TempDir()},
				Machine: "devbox",
			})
			require.True(t, ok)
			require.NotNil(t, provider)
		})
	}
}

func TestVSCodeCopilotProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	sessionID := "vscode-provider"
	hashDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
	chatDir := filepath.Join(hashDir, "chatSessions")
	jsonPath := filepath.Join(chatDir, sessionID+".json")
	jsonlPath := filepath.Join(chatDir, sessionID+".jsonl")
	writeSourceFile(t, filepath.Join(hashDir, "workspace.json"),
		`{"folder":"file:///Users/alice/code/copilot-app"}`)
	writeSourceFile(t, jsonPath, `{"version":3,"sessionId":"`+sessionID+`","requests":[]}`)
	writeSourceFile(t, jsonlPath, strings.Join([]string{
		`{"kind":0,"v":{"version":3,"sessionId":"` + sessionID + `","creationDate":1770650022790,"requests":[]}}`,
		`{"kind":2,"k":["requests"],"v":[{"requestId":"req1","timestamp":1770650031889,"message":{"text":"Hello VS Code","parts":[]},"response":[{"value":"Hi from VS Code"}],"modelId":"copilot/claude-opus-4.8","result":{"metadata":{"promptTokens":42,"outputTokens":7,"resolvedModel":"claude-opus-4-8"}}}]}`,
	}, "\n")+"\n")

	provider, ok := NewProvider(AgentVSCodeCopilot, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)
	assert.Equal(t, filepath.Join(root, "workspaceStorage"), plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, filepath.Join(root, "globalStorage"), plan.Roots[1].Path)
	assert.True(t, plan.Roots[1].Recursive)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, jsonlPath, discovered[0].DisplayPath)
	assert.Equal(t, "copilot-app", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~vscode-copilot:" + sessionID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, jsonlPath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: jsonlPath, EventKind: "write", WatchRoot: filepath.Join(root, "workspaceStorage")},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, jsonlPath, changed[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, jsonlPath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      found,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	require.False(t, outcome.ForceReplace)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "vscode-copilot:"+sessionID, result.Result.Session.ID)
	assert.Equal(t, AgentVSCodeCopilot, result.Result.Session.Agent)
	assert.Equal(t, "copilot-app", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Equal(t, fingerprint.Size, result.Result.Session.File.Size)
	assert.Equal(t, fingerprint.MTimeNS, result.Result.Session.File.Mtime)
	assert.Len(t, result.Result.Messages, 2)
	require.Len(t, result.Result.UsageEvents, 1)
	assert.Equal(t, "vscode-copilot", result.Result.UsageEvents[0].Source)
	assert.Equal(t, "claude-opus-4-8", result.Result.UsageEvents[0].Model)
	assert.Equal(t, 42, result.Result.UsageEvents[0].InputTokens)
	assert.Equal(t, 7, result.Result.UsageEvents[0].OutputTokens)
}

func TestVSCodeCopilotProviderClassifiesDeletedAndMetadataPaths(t *testing.T) {
	root := t.TempDir()
	hashDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
	chatDir := filepath.Join(hashDir, "chatSessions")
	workspacePath := filepath.Join(hashDir, "workspace.json")
	jsonlPath := filepath.Join(chatDir, "deleted-jsonl.jsonl")
	jsonPath := filepath.Join(chatDir, "fallback-json.json")
	globalPath := filepath.Join(
		root,
		"globalStorage",
		"emptyWindowChatSessions",
		"deleted-global.json",
	)
	writeSourceFile(t, workspacePath,
		`{"folder":"file:///Users/alice/code/copilot-app"}`)
	writeSourceFile(t, jsonlPath, vscodeCopilotProviderJSONL("deleted-jsonl", "Hello deleted"))
	writeSourceFile(t, jsonPath, vscodeCopilotProviderJSON("fallback-json", "Hello fallback"))
	writeSourceFile(t, globalPath, vscodeCopilotProviderJSON("deleted-global", "Hello global"))

	provider, ok := NewProvider(AgentVSCodeCopilot, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	metadataChanged, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: workspacePath, EventKind: "write"},
	)
	require.NoError(t, err)
	assert.ElementsMatch(t,
		[]string{jsonlPath, jsonPath},
		sourceDisplayPaths(metadataChanged),
	)
	require.Len(t, metadataChanged, 2)
	beforeMetadata, err := provider.Fingerprint(context.Background(), metadataChanged[0])
	require.NoError(t, err)
	writeSourceFile(t, workspacePath,
		`{"folder":"file:///Users/alice/code/copilot-renamed-app"}`)
	afterMetadata, err := provider.Fingerprint(context.Background(), metadataChanged[0])
	require.NoError(t, err)
	assert.NotEqual(t, beforeMetadata.Hash, afterMetadata.Hash)

	require.NoError(t, os.Remove(jsonlPath))
	deletedJSONL, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: jsonlPath, EventKind: "remove"},
	)
	require.NoError(t, err)
	require.Len(t, deletedJSONL, 1)
	assert.Equal(t, jsonlPath, deletedJSONL[0].DisplayPath)

	require.NoError(t, os.Remove(globalPath))
	deletedGlobal, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: globalPath, EventKind: "remove"},
	)
	require.NoError(t, err)
	require.Len(t, deletedGlobal, 1)
	assert.Equal(t, globalPath, deletedGlobal[0].DisplayPath)
}

func TestVisualStudioCopilotProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	tracePath := filepath.Join(
		root,
		"20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	writeSourceFile(t, tracePath, strings.Join([]string{
		vsCopilotTraceLineJSON(conversationID,
			"execute_tool run_command_in_terminal",
			"1781293588624985000", "1781293588769581200",
			map[string]string{
				"gen_ai.tool.name":           "run_command_in_terminal",
				"gen_ai.tool.call.id":        "call_123",
				"gen_ai.tool.call.arguments": `{"command":"go test ./..."}`,
				"gen_ai.tool.call.result":    `{"Value":"ok"}`,
			}),
		vsCopilotTraceLineJSON(conversationID,
			"invoke_agent GitHub Copilot",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.agent.name":       "GitHub Copilot",
				"gen_ai.request.model":    "gpt-5.5",
				"copilot_chat.mode":       "Agent",
				"copilot_chat.turn_count": "1",
			}),
	}, "\n")+"\n")

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*_VSGitHubCopilot_traces.jsonl"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	virtualPath := VisualStudioCopilotVirtualPath(tracePath, conversationID)
	assert.Equal(t, virtualPath, discovered[0].DisplayPath)
	assert.Equal(t, "visualstudio", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: conversationID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: tracePath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, tracePath, changed[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, virtualPath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.Positive(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)

	foundWithProject := found
	foundWithProject.ProjectHint = "stored-solution"
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      foundWithProject,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "visualstudio-copilot:"+conversationID, result.Result.Session.ID)
	assert.Equal(t, AgentVSCopilot, result.Result.Session.Agent)
	assert.Equal(t, "stored-solution", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Len(t, result.Result.Messages, 1)
}

func TestVisualStudioCopilotProviderClassifiesDeletedTraceAndFansOutPhysicalTrace(
	t *testing.T,
) {
	root := t.TempDir()
	firstConversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	secondConversationID := "5b9f63f6-7626-4416-a874-fc7bd2c3f006"
	tracePath := filepath.Join(
		root,
		"20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	writeSourceFile(t, tracePath, strings.Join([]string{
		vsCopilotTraceLineJSON(firstConversationID,
			"execute_tool run_command_in_terminal",
			"1781293588624985000", "1781293588769581200",
			map[string]string{
				"gen_ai.tool.name":           "run_command_in_terminal",
				"gen_ai.tool.call.id":        "call_123",
				"gen_ai.tool.call.arguments": `{"command":"go test ./..."}`,
				"gen_ai.tool.call.result":    `{"Value":"ok"}`,
			}),
		vsCopilotTraceLineJSON(secondConversationID,
			"execute_tool run_command_in_terminal",
			"1781293688624985000", "1781293688769581200",
			map[string]string{
				"gen_ai.tool.name":           "run_command_in_terminal",
				"gen_ai.tool.call.id":        "call_456",
				"gen_ai.tool.call.arguments": `{"command":"go vet ./..."}`,
				"gen_ai.tool.call.result":    `{"Value":"ok"}`,
			}),
	}, "\n")+"\n")

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	assert.ElementsMatch(t, []string{
		VisualStudioCopilotVirtualPath(tracePath, firstConversationID),
		VisualStudioCopilotVirtualPath(tracePath, secondConversationID),
	}, sourceDisplayPaths(discovered))

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: tracePath, EventKind: "write"},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, tracePath, changed[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), changed[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      changed[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 2)
	assert.ElementsMatch(t, []string{
		"visualstudio-copilot:" + firstConversationID,
		"visualstudio-copilot:" + secondConversationID,
	}, parseOutcomeSessionIDs(outcome))

	require.NoError(t, os.Remove(tracePath))
	deleted, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: tracePath, EventKind: "remove"},
	)
	require.NoError(t, err)
	require.Len(t, deleted, 1)
	assert.Equal(t, tracePath, deleted[0].DisplayPath)
}

func vscodeCopilotProviderJSON(sessionID, prompt string) string {
	return `{"version":3,"sessionId":"` + sessionID + `","creationDate":1770650022790,"requests":[{"requestId":"req1","timestamp":1770650031889,"message":{"text":"` + prompt + `","parts":[]},"response":[{"value":"Hi from VS Code"}],"modelId":"copilot/gpt-4o"}]}`
}

func vscodeCopilotProviderJSONL(sessionID, prompt string) string {
	return strings.Join([]string{
		`{"kind":0,"v":{"version":3,"sessionId":"` + sessionID + `","creationDate":1770650022790,"requests":[]}}`,
		`{"kind":2,"k":["requests"],"v":[{"requestId":"req1","timestamp":1770650031889,"message":{"text":"` + prompt + `","parts":[]},"response":[{"value":"Hi from VS Code"}],"modelId":"copilot/gpt-4o"}]}`,
	}, "\n") + "\n"
}

func parseOutcomeSessionIDs(outcome ParseOutcome) []string {
	ids := make([]string, 0, len(outcome.Results))
	for _, result := range outcome.Results {
		ids = append(ids, result.Result.Session.ID)
	}
	return ids
}
