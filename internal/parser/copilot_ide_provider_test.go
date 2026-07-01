package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		ChangedPathRequest{
			Path:              tracePath,
			EventKind:         "remove",
			StoredSourcePaths: sourceDisplayPaths(discovered),
		},
	)
	require.NoError(t, err)
	require.Len(t, deleted, 1)
	assert.Equal(t, tracePath, deleted[0].DisplayPath)
}

func TestVisualStudioCopilotProviderTombstonesDeletedVS2026SessionFile(
	t *testing.T,
) {
	root := t.TempDir()
	conversationID := "5bc5f6d7-9a6e-4f9c-8f3c-b7be2e7d9f20"
	sessionPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "sessions",
		conversationID,
	)
	writeSourceFile(t, sessionPath, vsCopilotTraceLineJSON(
		conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`,
		},
	)+"\n")

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	assert.Equal(t, virtualPath, discovered[0].DisplayPath)

	require.NoError(t, os.Remove(sessionPath))
	deleted, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:              sessionPath,
			EventKind:         "remove",
			StoredSourcePaths: sourceDisplayPaths(discovered),
		},
	)
	require.NoError(t, err)
	require.Len(t, deleted, 1)
	assert.Equal(t, virtualPath, deleted[0].DisplayPath)
}

func TestVisualStudioCopilotProviderCanonicalizesVS2026SessionFileIDCase(
	t *testing.T,
) {
	root := t.TempDir()
	conversationID := "5bc5f6d7-9a6e-4f9c-8f3c-b7be2e7d9f20"
	upperID := strings.ToUpper(conversationID)
	sessionPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "sessions",
		upperID,
	)
	traceData := vsCopilotTraceLineJSON(
		conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`,
		},
	) + "\n"
	legacyPath := filepath.Join(
		root,
		"20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	writeSourceFile(t, legacyPath, traceData)
	writeSourceFile(t, sessionPath, traceData)
	older := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(legacyPath, older, older))
	require.NoError(t, os.Chtimes(sessionPath, newer, newer))

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	assert.Equal(t, virtualPath, discovered[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      discovered[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t,
		"visualstudio-copilot:"+conversationID,
		outcome.Results[0].Result.Session.ID,
	)
}

func TestVisualStudioCopilotProviderTombstonesUppercaseVS2026SessionFileID(
	t *testing.T,
) {
	root := t.TempDir()
	conversationID := "5bc5f6d7-9a6e-4f9c-8f3c-b7be2e7d9f20"
	sessionPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "sessions",
		strings.ToUpper(conversationID),
	)
	writeSourceFile(t, sessionPath, vsCopilotTraceLineJSON(
		conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`,
		},
	)+"\n")

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	assert.Equal(t, virtualPath, discovered[0].DisplayPath)

	require.NoError(t, os.Remove(sessionPath))
	deleted, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:              sessionPath,
			EventKind:         "remove",
			StoredSourcePaths: sourceDisplayPaths(discovered),
		},
	)
	require.NoError(t, err)
	require.Len(t, deleted, 1)
	assert.Equal(t, virtualPath, deleted[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), deleted[0])
	require.NoError(t, err)
	assert.Equal(t, SourceFingerprint{Key: virtualPath}, fingerprint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      deleted[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace)
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)
}

func TestVisualStudioCopilotProviderSupportsVS2026RootModes(
	t *testing.T,
) {
	root := t.TempDir()
	vsRoot := filepath.Join(root, ".vs")
	conversationID := "5bc5f6d7-9a6e-4f9c-8f3c-b7be2e7d9f20"
	sessionPath := filepath.Join(
		vsRoot, "SampleApp", "copilot-chat", "thread", "sessions",
		conversationID,
	)
	traceData := vsCopilotTraceLineJSON(
		conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`,
		},
	) + "\n"
	writeSourceFile(t, sessionPath, traceData)

	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	cases := []struct {
		name string
		root string
	}{
		{name: "project root", root: root},
		{name: ".vs root", root: vsRoot},
		{name: "copilot-chat root", root: filepath.Join(vsRoot, "SampleApp", "copilot-chat")},
		{name: "thread root", root: filepath.Join(vsRoot, "SampleApp", "copilot-chat", "thread")},
		{name: "sessions root", root: filepath.Join(vsRoot, "SampleApp", "copilot-chat", "thread", "sessions")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
				Roots: []string{tc.root},
			})
			require.True(t, ok)

			plan, err := provider.WatchPlan(context.Background())
			require.NoError(t, err)
			switch tc.name {
			case "project root":
				require.Len(t, plan.Roots, 3)
				assert.Equal(t, root, plan.Roots[0].Path)
				assert.False(t, plan.Roots[0].Recursive)
				assert.Equal(t, vsRoot, plan.Roots[1].Path)
				assert.True(t, plan.Roots[1].Recursive)
				assert.Equal(t, filepath.Dir(sessionPath), plan.Roots[2].Path)
				assert.False(t, plan.Roots[2].Recursive)
			case "sessions root":
				require.Len(t, plan.Roots, 1)
				assert.Equal(t, tc.root, plan.Roots[0].Path)
				assert.False(t, plan.Roots[0].Recursive)
			default:
				require.Len(t, plan.Roots, 2)
				assert.Equal(t, tc.root, plan.Roots[0].Path)
				assert.True(t, plan.Roots[0].Recursive)
				assert.Equal(t, filepath.Dir(sessionPath), plan.Roots[1].Path)
				assert.False(t, plan.Roots[1].Recursive)
			}

			discovered, err := provider.Discover(context.Background())
			require.NoError(t, err)
			require.Len(t, discovered, 1)
			assert.Equal(t, virtualPath, discovered[0].DisplayPath)

			found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
				RawSessionID: conversationID,
			})
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, virtualPath, found.DisplayPath)

			changed, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{
					Path:      sessionPath,
					EventKind: "write",
					WatchRoot: tc.root,
				},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, virtualPath, changed[0].DisplayPath)

			fingerprint, err := provider.Fingerprint(context.Background(), changed[0])
			require.NoError(t, err)
			assert.Equal(t, virtualPath, fingerprint.Key)
			assert.Positive(t, fingerprint.Size)
			assert.Positive(t, fingerprint.MTimeNS)
			assert.NotEmpty(t, fingerprint.Hash)

			parseOutcome, err := provider.Parse(context.Background(), ParseRequest{
				Source:      changed[0],
				Fingerprint: fingerprint,
			})
			require.NoError(t, err)
			require.True(t, parseOutcome.ForceReplace)
			require.Len(t, parseOutcome.Results, 1)
			assert.Equal(t, "visualstudio-copilot:"+conversationID,
				parseOutcome.Results[0].Result.Session.ID)
			assert.Equal(t, virtualPath,
				parseOutcome.Results[0].Result.Session.File.Path)
			assert.Equal(t, fingerprint.Hash,
				parseOutcome.Results[0].Result.Session.File.Hash)
			assert.Equal(t, fingerprint.Size,
				parseOutcome.Results[0].Result.Session.File.Size)
			assert.Equal(t, fingerprint.MTimeNS,
				parseOutcome.Results[0].Result.Session.File.Mtime)
		})
	}
}

func TestVisualStudioCopilotProviderClassifiesMixedCaseVS2026Layout(
	t *testing.T,
) {
	root := t.TempDir()
	conversationID := "5bc5f6d7-9a6e-4f9c-8f3c-b7be2e7d9f20"
	sessionPath := filepath.Join(
		root, ".VS", "SampleApp", "Copilot-Chat", "thread", "Sessions",
		conversationID,
	)
	writeSourceFile(t, sessionPath, vsCopilotTraceLineJSON(
		conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`,
		},
	)+"\n")

	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	cases := []struct {
		name string
		root string
	}{
		{name: "project root", root: root},
		{name: ".vs root", root: filepath.Join(root, ".VS")},
		{name: "copilot-chat root", root: filepath.Join(root, ".VS", "SampleApp", "Copilot-Chat")},
		{name: "thread root", root: filepath.Join(root, ".VS", "SampleApp", "Copilot-Chat", "thread")},
		{name: "sessions root", root: filepath.Join(root, ".VS", "SampleApp", "Copilot-Chat", "thread", "Sessions")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
				Roots: []string{tc.root},
			})
			require.True(t, ok)

			changed, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{
					Path:      sessionPath,
					EventKind: "write",
					WatchRoot: tc.root,
				},
			)
			require.NoError(t, err)
			require.Len(t, changed, 1)
			assert.Equal(t, virtualPath, changed[0].DisplayPath)
		})
	}
}

func TestVisualStudioCopilotProviderDiscoversMixedCaseVS2026Layout(
	t *testing.T,
) {
	root := t.TempDir()
	vsRoot := filepath.Join(root, ".VS")
	conversationID := "5bc5f6d7-9a6e-4f9c-8f3c-b7be2e7d9f20"
	sessionPath := filepath.Join(
		vsRoot, "SampleApp", "Copilot-Chat", "thread", "Sessions",
		conversationID,
	)
	writeSourceFile(t, sessionPath, vsCopilotTraceLineJSON(
		conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`,
		},
	)+"\n")

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 3)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.Equal(t, vsRoot, plan.Roots[1].Path)
	assert.True(t, plan.Roots[1].Recursive)
	assert.Equal(t, filepath.Dir(sessionPath), plan.Roots[2].Path)
	assert.False(t, plan.Roots[2].Recursive)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, virtualPath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: conversationID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, found.DisplayPath)
}

func TestVisualStudioCopilotProviderDiscoversSymlinkedVS2026Dirs(
	t *testing.T,
) {
	root := t.TempDir()
	targetRoot := t.TempDir()
	targetVSRoot := filepath.Join(targetRoot, "vs-data")
	targetSolutionRoot := filepath.Join(targetRoot, "solution-data")
	targetCopilotChatRoot := filepath.Join(targetRoot, "chat-data")
	targetThreadRoot := filepath.Join(targetRoot, "thread-data")
	targetSessionsRoot := filepath.Join(targetRoot, "sessions-data")
	require.NoError(t, os.MkdirAll(targetVSRoot, 0o755))
	require.NoError(t, os.MkdirAll(targetSolutionRoot, 0o755))
	require.NoError(t, os.MkdirAll(targetCopilotChatRoot, 0o755))
	require.NoError(t, os.MkdirAll(targetThreadRoot, 0o755))
	require.NoError(t, os.MkdirAll(targetSessionsRoot, 0o755))

	vsRoot := filepath.Join(root, ".VS")
	if err := os.Symlink(targetVSRoot, vsRoot); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	require.NoError(t, os.Symlink(
		targetSolutionRoot,
		filepath.Join(targetVSRoot, "SampleApp"),
	))
	require.NoError(t, os.Symlink(
		targetCopilotChatRoot,
		filepath.Join(targetSolutionRoot, "Copilot-Chat"),
	))
	require.NoError(t, os.Symlink(
		targetThreadRoot,
		filepath.Join(targetCopilotChatRoot, "thread"),
	))
	require.NoError(t, os.Symlink(
		targetSessionsRoot,
		filepath.Join(targetThreadRoot, "Sessions"),
	))

	conversationID := "5bc5f6d7-9a6e-4f9c-8f3c-b7be2e7d9f20"
	writeSourceFile(t, filepath.Join(targetSessionsRoot, conversationID),
		vsCopilotTraceLineJSON(
			conversationID,
			"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`,
			},
		)+"\n")
	sessionPath := filepath.Join(
		vsRoot, "SampleApp", "Copilot-Chat", "thread", "Sessions",
		conversationID,
	)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 3)
	assert.Equal(t, vsRoot, plan.Roots[1].Path)
	assert.True(t, plan.Roots[1].Recursive)
	assert.Equal(t, filepath.Dir(sessionPath), plan.Roots[2].Path)
	assert.False(t, plan.Roots[2].Recursive)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, virtualPath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: conversationID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, found.DisplayPath)
}

func TestVisualStudioCopilotProviderCanonicalizesMixedLegacyAndVS2026Sources(
	t *testing.T,
) {
	root := t.TempDir()
	conversationID := "5bc5f6d7-9a6e-4f9c-8f3c-b7be2e7d9f20"
	legacyPath := filepath.Join(
		root,
		"20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	sessionPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "sessions",
		conversationID,
	)
	traceData := vsCopilotTraceLineJSON(
		conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`,
		},
	) + "\n"
	writeSourceFile(t, legacyPath, traceData)
	writeSourceFile(t, sessionPath, traceData)

	older := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(legacyPath, older, older))
	require.NoError(t, os.Chtimes(sessionPath, newer, newer))

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	assert.Equal(t, virtualPath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: conversationID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, found.DisplayPath)
}

func TestVisualStudioCopilotProviderDeletesVS2026SessionTombstone(
	t *testing.T,
) {
	root := t.TempDir()
	conversationID := "5bc5f6d7-9a6e-4f9c-8f3c-b7be2e7d9f20"
	sessionPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "sessions",
		conversationID,
	)
	writeSourceFile(t, sessionPath, vsCopilotTraceLineJSON(
		conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`,
		},
	)+"\n")

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	assert.Equal(t, virtualPath, discovered[0].DisplayPath)

	require.NoError(t, os.Remove(sessionPath))

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:              sessionPath,
			EventKind:         "remove",
			WatchRoot:         root,
			StoredSourcePaths: []string{virtualPath},
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, virtualPath, changed[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), changed[0])
	require.NoError(t, err)
	assert.Equal(t, SourceFingerprint{Key: virtualPath}, fingerprint)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      changed[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace,
		"deleted VS 2026 session files must force-replace the archived member")
	assert.Equal(t, SkipNoSession, outcome.SkipReason)
	assert.Empty(t, outcome.Results)
}

func TestVisualStudioCopilotProviderRecanonicalizesDeletedVS2026Session(
	t *testing.T,
) {
	root := t.TempDir()
	conversationID := "5bc5f6d7-9a6e-4f9c-8f3c-b7be2e7d9f20"
	legacyPath := filepath.Join(
		root,
		"20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	sessionPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "sessions",
		conversationID,
	)
	traceData := vsCopilotTraceLineJSON(
		conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`,
		},
	) + "\n"
	writeSourceFile(t, legacyPath, traceData)
	writeSourceFile(t, sessionPath, traceData)

	older := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(legacyPath, older, older))
	require.NoError(t, os.Chtimes(sessionPath, newer, newer))

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	vsVirtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	assert.Equal(t, vsVirtualPath, discovered[0].DisplayPath)

	require.NoError(t, os.Remove(sessionPath))

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:              sessionPath,
			EventKind:         "remove",
			WatchRoot:         root,
			StoredSourcePaths: []string{vsVirtualPath},
		},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)

	legacyVirtualPath := VisualStudioCopilotVirtualPath(legacyPath, conversationID)
	assert.Equal(t, legacyVirtualPath, changed[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), changed[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      changed[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	assert.Equal(
		t,
		string(AgentVSCopilot)+":"+conversationID,
		outcome.Results[0].Result.Session.ID,
	)
}

func TestVisualStudioCopilotProviderFindSourceRejectsMissingVS2026VirtualPath(
	t *testing.T,
) {
	root := t.TempDir()
	conversationID := "5bc5f6d7-9a6e-4f9c-8f3c-b7be2e7d9f20"
	sessionPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "sessions",
		conversationID,
	)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID:       conversationID,
		StoredFilePath:     virtualPath,
		FingerprintKey:     virtualPath,
		RequireFreshSource: true,
		PreferStoredSource: true,
	})
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, found.DisplayPath)
}

func TestVisualStudioCopilotProviderRejectsOutsideVS2026SessionLayout(
	t *testing.T,
) {
	root := t.TempDir()
	conversationID := "8e5bb2d4-ef8e-4a90-a1f2-3d3d4d6fc9e9"
	invalidPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "transcripts",
		conversationID,
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(invalidPath), 0o755))
	writeSourceFile(t, invalidPath, vsCopilotTraceLineJSON(
		conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`,
		},
	)+"\n")

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      invalidPath,
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, discovered)
}

func TestVisualStudioCopilotProviderRejectsNonGUIDVS2026SessionFileNames(
	t *testing.T,
) {
	root := t.TempDir()
	invalidName := "not-a-guid"
	invalidPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "sessions",
		invalidName,
	)
	writeSourceFile(t, invalidPath, vsCopilotTraceLineJSON(
		"5bc5f6d7-9a6e-4f9c-8f3c-b7be2e7d9f20",
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`,
		},
	)+"\n")

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path:      invalidPath,
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(t, err)
	assert.Empty(t, changed)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, discovered)
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
