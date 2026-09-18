package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVSCodeCopilotProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 2)
	assert.Equal(filepath.Join(root, "workspaceStorage"), plan.Roots[0].Path)
	assert.True(plan.Roots[0].Recursive)
	assert.Equal(filepath.Join(root, "globalStorage"), plan.Roots[1].Path)
	assert.True(plan.Roots[1].Recursive)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(jsonlPath, discovered[0].DisplayPath)
	assert.Equal("copilot-app", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~vscode-copilot:" + sessionID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(jsonlPath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: jsonlPath, EventKind: "write", WatchRoot: filepath.Join(root, "workspaceStorage")},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(jsonlPath, changed[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	assert.Equal(jsonlPath, fingerprint.Key)
	assert.Positive(fingerprint.Size)
	assert.Positive(fingerprint.MTimeNS)
	assert.NotEmpty(fingerprint.Hash)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      found,
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.Len(outcome.Results, 1)
	require.False(outcome.ForceReplace)
	result := outcome.Results[0]
	assert.Equal(DataVersionCurrent, result.DataVersion)
	assert.Equal("vscode-copilot:"+sessionID, result.Result.Session.ID)
	assert.Equal(AgentVSCodeCopilot, result.Result.Session.Agent)
	assert.Equal("copilot-app", result.Result.Session.Project)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Equal(fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Equal(fingerprint.Size, result.Result.Session.File.Size)
	assert.Equal(fingerprint.MTimeNS, result.Result.Session.File.Mtime)
	assert.Len(result.Result.Messages, 2)
	require.Len(result.Result.UsageEvents, 1)
	assert.Equal("vscode-copilot", result.Result.UsageEvents[0].Source)
	assert.Equal("claude-opus-4-8", result.Result.UsageEvents[0].Model)
	assert.Equal(42, result.Result.UsageEvents[0].InputTokens)
	assert.Equal(7, result.Result.UsageEvents[0].OutputTokens)
}

func TestVSCodeCopilotProviderClassifiesDeletedAndMetadataPaths(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	metadataChanged, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: workspacePath, EventKind: "write"},
	)
	require.NoError(err)
	assert.ElementsMatch([]string{jsonlPath, jsonPath},
		sourceDisplayPaths(metadataChanged),
	)
	require.Len(metadataChanged, 2)
	beforeMetadata, err := provider.Fingerprint(t.Context(), metadataChanged[0])
	require.NoError(err)
	writeSourceFile(t, workspacePath,
		`{"folder":"file:///Users/alice/code/copilot-renamed-app"}`)
	afterMetadata, err := provider.Fingerprint(t.Context(), metadataChanged[0])
	require.NoError(err)
	assert.NotEqual(beforeMetadata.Hash, afterMetadata.Hash)

	require.NoError(os.Remove(jsonlPath))
	deletedJSONL, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: jsonlPath, EventKind: "remove"},
	)
	require.NoError(err)
	require.Len(deletedJSONL, 1)
	assert.Equal(jsonlPath, deletedJSONL[0].DisplayPath)

	require.NoError(os.Remove(globalPath))
	deletedGlobal, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: globalPath, EventKind: "remove"},
	)
	require.NoError(err)
	require.Len(deletedGlobal, 1)
	assert.Equal(globalPath, deletedGlobal[0].DisplayPath)
}

func TestVisualStudioCopilotProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(root, plan.Roots[0].Path)
	assert.False(plan.Roots[0].Recursive)
	assert.Equal([]string{"*_VSGitHubCopilot_traces.jsonl"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	virtualPath := VisualStudioCopilotVirtualPath(tracePath, conversationID)
	assert.Equal(virtualPath, discovered[0].DisplayPath)
	assert.Equal("visualstudio", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: conversationID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: tracePath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(tracePath, changed[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	assert.Equal(virtualPath, fingerprint.Key)
	assert.Positive(fingerprint.Size)
	assert.Positive(fingerprint.MTimeNS)
	assert.NotEmpty(fingerprint.Hash)

	foundWithProject := found
	foundWithProject.ProjectHint = "stored-solution"
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      foundWithProject,
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.True(outcome.ForceReplace)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(DataVersionCurrent, result.DataVersion)
	assert.Equal("visualstudio-copilot:"+conversationID, result.Result.Session.ID)
	assert.Equal(AgentVSCopilot, result.Result.Session.Agent)
	assert.Equal("stored-solution", result.Result.Session.Project)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Equal(fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Len(result.Result.Messages, 1)
}

func TestVisualStudioCopilotProviderClassifiesDeletedTraceAndFansOutPhysicalTrace(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 2)
	assert.ElementsMatch([]string{
		VisualStudioCopilotVirtualPath(tracePath, firstConversationID),
		VisualStudioCopilotVirtualPath(tracePath, secondConversationID),
	}, sourceDisplayPaths(discovered))

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: tracePath, EventKind: "write"},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(tracePath, changed[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), changed[0])
	require.NoError(err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      changed[0],
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.True(outcome.ForceReplace)
	require.Len(outcome.Results, 2)
	assert.ElementsMatch([]string{
		"visualstudio-copilot:" + firstConversationID,
		"visualstudio-copilot:" + secondConversationID,
	}, parseOutcomeSessionIDs(outcome))

	require.NoError(os.Remove(tracePath))
	deleted, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:              tracePath,
			EventKind:         "remove",
			StoredSourcePaths: sourceDisplayPaths(discovered),
		},
	)
	require.NoError(err)
	require.Len(deleted, 1)
	assert.Equal(tracePath, deleted[0].DisplayPath)
}

func TestVisualStudioCopilotProviderTombstonesDeletedVS2026SessionFile(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	assert.Equal(virtualPath, discovered[0].DisplayPath)

	require.NoError(os.Remove(sessionPath))
	deleted, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:              sessionPath,
			EventKind:         "remove",
			StoredSourcePaths: sourceDisplayPaths(discovered),
		},
	)
	require.NoError(err)
	require.Len(deleted, 1)
	assert.Equal(virtualPath, deleted[0].DisplayPath)
}

func TestVisualStudioCopilotProviderCanonicalizesVS2026SessionFileIDCase(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(os.Chtimes(legacyPath, older, older))
	require.NoError(os.Chtimes(sessionPath, newer, newer))

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	assert.Equal(virtualPath, discovered[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), discovered[0])
	require.NoError(err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      discovered[0],
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal("visualstudio-copilot:"+conversationID,
		outcome.Results[0].Result.Session.ID,
	)
}

func TestVisualStudioCopilotProviderTombstonesUppercaseVS2026SessionFileID(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	assert.Equal(virtualPath, discovered[0].DisplayPath)

	require.NoError(os.Remove(sessionPath))
	deleted, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:              sessionPath,
			EventKind:         "remove",
			StoredSourcePaths: sourceDisplayPaths(discovered),
		},
	)
	require.NoError(err)
	require.Len(deleted, 1)
	assert.Equal(virtualPath, deleted[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), deleted[0])
	require.NoError(err)
	assert.Equal(SourceFingerprint{Key: virtualPath}, fingerprint)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      deleted[0],
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.True(outcome.ForceReplace)
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)
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
			assert := assert.New(t)
			require := require.New(t)

			provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
				Roots: []string{tc.root},
			})
			require.True(ok)

			plan, err := provider.WatchPlan(t.Context())
			require.NoError(err)
			switch tc.name {
			case "project root":
				require.Len(plan.Roots, 3)
				assert.Equal(root, plan.Roots[0].Path)
				assert.False(plan.Roots[0].Recursive)
				assert.Equal(vsRoot, plan.Roots[1].Path)
				assert.True(plan.Roots[1].Recursive)
				assert.Equal(filepath.Dir(sessionPath), plan.Roots[2].Path)
				assert.False(plan.Roots[2].Recursive)
			case "sessions root":
				require.Len(plan.Roots, 1)
				assert.Equal(tc.root, plan.Roots[0].Path)
				assert.False(plan.Roots[0].Recursive)
			default:
				require.Len(plan.Roots, 2)
				assert.Equal(tc.root, plan.Roots[0].Path)
				assert.True(plan.Roots[0].Recursive)
				assert.Equal(filepath.Dir(sessionPath), plan.Roots[1].Path)
				assert.False(plan.Roots[1].Recursive)
			}

			discovered, err := provider.Discover(t.Context())
			require.NoError(err)
			require.Len(discovered, 1)
			assert.Equal(virtualPath, discovered[0].DisplayPath)

			found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
				RawSessionID: conversationID,
			})
			require.NoError(err)
			require.True(ok)
			assert.Equal(virtualPath, found.DisplayPath)

			changed, err := provider.SourcesForChangedPath(
				t.Context(),
				ChangedPathRequest{
					Path:      sessionPath,
					EventKind: "write",
					WatchRoot: tc.root,
				},
			)
			require.NoError(err)
			require.Len(changed, 1)
			assert.Equal(virtualPath, changed[0].DisplayPath)

			fingerprint, err := provider.Fingerprint(t.Context(), changed[0])
			require.NoError(err)
			assert.Equal(virtualPath, fingerprint.Key)
			assert.Positive(fingerprint.Size)
			assert.Positive(fingerprint.MTimeNS)
			assert.NotEmpty(fingerprint.Hash)

			parseOutcome, err := provider.Parse(t.Context(), ParseRequest{
				Source:      changed[0],
				Fingerprint: fingerprint,
			})
			require.NoError(err)
			require.True(parseOutcome.ForceReplace)
			require.Len(parseOutcome.Results, 1)
			assert.Equal("visualstudio-copilot:"+conversationID,
				parseOutcome.Results[0].Result.Session.ID)
			assert.Equal(virtualPath,
				parseOutcome.Results[0].Result.Session.File.Path)
			assert.Equal(fingerprint.Hash,
				parseOutcome.Results[0].Result.Session.File.Hash)
			assert.Equal(fingerprint.Size,
				parseOutcome.Results[0].Result.Session.File.Size)
			assert.Equal(fingerprint.MTimeNS,
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
			require := require.New(t)

			provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
				Roots: []string{tc.root},
			})
			require.True(ok)

			changed, err := provider.SourcesForChangedPath(
				t.Context(),
				ChangedPathRequest{
					Path:      sessionPath,
					EventKind: "write",
					WatchRoot: tc.root,
				},
			)
			require.NoError(err)
			require.Len(changed, 1)
			assert.Equal(t, virtualPath, changed[0].DisplayPath)
		})
	}
}

func TestVisualStudioCopilotProviderDiscoversMixedCaseVS2026Layout(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 3)
	assert.Equal(root, plan.Roots[0].Path)
	assert.False(plan.Roots[0].Recursive)
	assert.Equal(vsRoot, plan.Roots[1].Path)
	assert.True(plan.Roots[1].Recursive)
	assert.Equal(filepath.Dir(sessionPath), plan.Roots[2].Path)
	assert.False(plan.Roots[2].Recursive)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(virtualPath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: conversationID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, found.DisplayPath)
}

func TestVisualStudioCopilotProviderDiscoversSymlinkedVS2026Dirs(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	targetRoot := t.TempDir()
	targetVSRoot := filepath.Join(targetRoot, "vs-data")
	targetSolutionRoot := filepath.Join(targetRoot, "solution-data")
	targetCopilotChatRoot := filepath.Join(targetRoot, "chat-data")
	targetThreadRoot := filepath.Join(targetRoot, "thread-data")
	targetSessionsRoot := filepath.Join(targetRoot, "sessions-data")
	require.NoError(os.MkdirAll(targetVSRoot, 0o755))
	require.NoError(os.MkdirAll(targetSolutionRoot, 0o755))
	require.NoError(os.MkdirAll(targetCopilotChatRoot, 0o755))
	require.NoError(os.MkdirAll(targetThreadRoot, 0o755))
	require.NoError(os.MkdirAll(targetSessionsRoot, 0o755))

	vsRoot := filepath.Join(root, ".VS")
	if err := os.Symlink(targetVSRoot, vsRoot); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	require.NoError(os.Symlink(
		targetSolutionRoot,
		filepath.Join(targetVSRoot, "SampleApp"),
	))
	require.NoError(os.Symlink(
		targetCopilotChatRoot,
		filepath.Join(targetSolutionRoot, "Copilot-Chat"),
	))
	require.NoError(os.Symlink(
		targetThreadRoot,
		filepath.Join(targetCopilotChatRoot, "thread"),
	))
	require.NoError(os.Symlink(
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
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 3)
	assert.Equal(vsRoot, plan.Roots[1].Path)
	assert.True(plan.Roots[1].Recursive)
	assert.Equal(filepath.Dir(sessionPath), plan.Roots[2].Path)
	assert.False(plan.Roots[2].Recursive)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(virtualPath, discovered[0].DisplayPath)

	streaming, ok := provider.(StreamingDiscoverer)
	require.True(ok)
	var streamed []SourceRef
	require.NoError(streaming.DiscoverEach(t.Context(), func(source SourceRef) error {
		streamed = append(streamed, source)
		return nil
	}))
	require.Len(streamed, 1)
	assert.Equal(virtualPath, streamed[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: conversationID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, found.DisplayPath)
}

func TestVisualStudioCopilotProviderCanonicalizesMixedLegacyAndVS2026Sources(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(os.Chtimes(legacyPath, older, older))
	require.NoError(os.Chtimes(sessionPath, newer, newer))

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	assert.Equal(virtualPath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: conversationID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, found.DisplayPath)
}

func TestVisualStudioCopilotProviderDeletesVS2026SessionTombstone(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	virtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	assert.Equal(virtualPath, discovered[0].DisplayPath)

	require.NoError(os.Remove(sessionPath))

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:              sessionPath,
			EventKind:         "remove",
			WatchRoot:         root,
			StoredSourcePaths: []string{virtualPath},
		},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(virtualPath, changed[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), changed[0])
	require.NoError(err)
	assert.Equal(SourceFingerprint{Key: virtualPath}, fingerprint)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      changed[0],
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.True(outcome.ForceReplace,
		"deleted VS 2026 session files must force-replace the archived member")
	assert.Equal(SkipNoSession, outcome.SkipReason)
	assert.Empty(outcome.Results)
}

func TestVisualStudioCopilotProviderRecanonicalizesDeletedVS2026Session(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(os.Chtimes(legacyPath, older, older))
	require.NoError(os.Chtimes(sessionPath, newer, newer))

	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{
		Roots: []string{root},
	})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	vsVirtualPath := VisualStudioCopilotVirtualPath(sessionPath, conversationID)
	assert.Equal(vsVirtualPath, discovered[0].DisplayPath)

	require.NoError(os.Remove(sessionPath))

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:              sessionPath,
			EventKind:         "remove",
			WatchRoot:         root,
			StoredSourcePaths: []string{vsVirtualPath},
		},
	)
	require.NoError(err)
	require.Len(changed, 1)

	legacyVirtualPath := VisualStudioCopilotVirtualPath(legacyPath, conversationID)
	assert.Equal(legacyVirtualPath, changed[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), changed[0])
	require.NoError(err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      changed[0],
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	assert.True(outcome.ResultSetComplete)
	assert.True(outcome.ForceReplace)
	require.Len(outcome.Results, 1)
	assert.Equal(
		string(AgentVSCopilot)+":"+conversationID,
		outcome.Results[0].Result.Session.ID,
	)
}

func TestVisualStudioCopilotProviderFindSourceRejectsMissingVS2026VirtualPath(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID:       conversationID,
		StoredFilePath:     virtualPath,
		FingerprintKey:     virtualPath,
		RequireFreshSource: true,
		PreferStoredSource: true,
	})
	require.NoError(err)
	assert.False(ok)
	assert.Empty(found.DisplayPath)
}

func TestVisualStudioCopilotProviderRejectsOutsideVS2026SessionLayout(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	conversationID := "8e5bb2d4-ef8e-4a90-a1f2-3d3d4d6fc9e9"
	invalidPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "transcripts",
		conversationID,
	)
	require.NoError(os.MkdirAll(filepath.Dir(invalidPath), 0o755))
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
	require.True(ok)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      invalidPath,
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(err)
	assert.Empty(changed)
	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	assert.Empty(discovered)
}

func TestVisualStudioCopilotProviderRejectsNonGUIDVS2026SessionFileNames(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{
			Path:      invalidPath,
			EventKind: "write",
			WatchRoot: root,
		},
	)
	require.NoError(err)
	assert.Empty(changed)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	assert.Empty(discovered)
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
