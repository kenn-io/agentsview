package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	positAssistantTestMainID = "11111111-1111-4111-8111-111111111111"
	positAssistantTestSubID  = "22222222-2222-4222-8222-222222222222"
)

func positAssistantTestRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(
		filepath.Join("testdata", "posit-assistant", "workspaces"),
	)
	require.NoError(t, err)
	return root
}

func positAssistantTestConvPath(root string, elem ...string) string {
	return filepath.Join(append([]string{root}, elem...)...)
}

func TestPositAssistantProviderDiscoverAndWatch(t *testing.T) {
	root := positAssistantTestRoot(t)
	provider, ok := NewProvider(AgentPositAssistant, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 3)

	byPath := make(map[string]SourceRef, len(discovered))
	for _, source := range discovered {
		byPath[source.DisplayPath] = source
	}
	mainPath := positAssistantTestConvPath(
		root, "a1b2c3d4e5f60718293a4b5c", positAssistantTestMainID,
		"conversation.json",
	)
	subPath := positAssistantTestConvPath(
		root, "a1b2c3d4e5f60718293a4b5c", positAssistantTestMainID,
		"subagents", positAssistantTestSubID, "conversation.json",
	)
	defaultPath := positAssistantTestConvPath(
		root, "default", "33333333-3333-4333-8333-333333333333",
		"conversation.json",
	)
	require.Contains(t, byPath, mainPath)
	require.Contains(t, byPath, subPath)
	require.Contains(t, byPath, defaultPath)
	assert.Equal(t, "sales-dashboard", byPath[mainPath].ProjectHint)
	assert.Equal(t, "sales-dashboard", byPath[subPath].ProjectHint)
	assert.Equal(t, "unknown", byPath[defaultPath].ProjectHint)
}

func TestPositAssistantProviderFindSource(t *testing.T) {
	root := positAssistantTestRoot(t)
	provider, ok := NewProvider(AgentPositAssistant, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	tests := []struct {
		name     string
		req      FindSourceRequest
		wantPath string
		wantOK   bool
	}{
		{
			name: "main conversation by full session ID",
			req: FindSourceRequest{
				FullSessionID: "host~posit-assistant:" + positAssistantTestMainID,
			},
			wantPath: positAssistantTestConvPath(
				root, "a1b2c3d4e5f60718293a4b5c", positAssistantTestMainID,
				"conversation.json",
			),
			wantOK: true,
		},
		{
			name: "nested subagent by raw session ID",
			req:  FindSourceRequest{RawSessionID: positAssistantTestSubID},
			wantPath: positAssistantTestConvPath(
				root, "a1b2c3d4e5f60718293a4b5c", positAssistantTestMainID,
				"subagents", positAssistantTestSubID, "conversation.json",
			),
			wantOK: true,
		},
		{
			name:   "unknown session ID",
			req:    FindSourceRequest{RawSessionID: "99999999-9999-4999-8999-999999999999"},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, ok, err := provider.FindSource(context.Background(), tt.req)
			require.NoError(t, err)
			require.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantPath, source.DisplayPath)
			}
		})
	}
}

func TestPositAssistantProviderClassifiesChangedPaths(t *testing.T) {
	root := positAssistantTestRoot(t)
	provider, ok := NewProvider(AgentPositAssistant, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	mainConvPath := positAssistantTestConvPath(
		root, "a1b2c3d4e5f60718293a4b5c", positAssistantTestMainID,
		"conversation.json",
	)
	mainConvDir := filepath.Dir(mainConvPath)

	tests := []struct {
		name      string
		path      string
		wantPaths []string
	}{
		{
			name:      "lm-messages append maps to its conversation",
			path:      filepath.Join(mainConvDir, "lm-messages.jsonl"),
			wantPaths: []string{mainConvPath},
		},
		{
			name:      "ui-messages write does not affect stored session data",
			path:      filepath.Join(mainConvDir, "ui-messages.jsonl"),
			wantPaths: nil,
		},
		{
			name: "workspace manifest re-emits all workspace conversations",
			path: filepath.Join(root, "a1b2c3d4e5f60718293a4b5c", "workspace.json"),
			wantPaths: []string{
				mainConvPath,
				positAssistantTestConvPath(
					root, "a1b2c3d4e5f60718293a4b5c", positAssistantTestMainID,
					"subagents", positAssistantTestSubID, "conversation.json",
				),
			},
		},
		{
			name:      "workspace index is not a session source",
			path:      filepath.Join(root, "a1b2c3d4e5f60718293a4b5c", "index.json"),
			wantPaths: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sources, err := provider.SourcesForChangedPath(
				context.Background(),
				ChangedPathRequest{Path: tt.path, EventKind: "write"},
			)
			require.NoError(t, err)
			paths := make([]string, 0, len(sources))
			for _, source := range sources {
				paths = append(paths, source.DisplayPath)
			}
			assert.ElementsMatch(t, tt.wantPaths, paths)
		})
	}
}

func TestPositAssistantProviderParseMainConversation(t *testing.T) {
	root := positAssistantTestRoot(t)
	provider, ok := NewProvider(AgentPositAssistant, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: positAssistantTestMainID,
	})
	require.NoError(t, err)
	require.True(t, ok)

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	convInfo, err := os.Stat(source.DisplayPath)
	require.NoError(t, err)
	lmInfo, err := os.Stat(
		filepath.Join(filepath.Dir(source.DisplayPath), "lm-messages.jsonl"),
	)
	require.NoError(t, err)
	wsInfo, err := os.Stat(
		filepath.Join(root, "a1b2c3d4e5f60718293a4b5c", "workspace.json"),
	)
	require.NoError(t, err)
	assert.Equal(t, source.DisplayPath, fingerprint.Key)
	assert.Equal(t,
		convInfo.Size()+lmInfo.Size()+wsInfo.Size(), fingerprint.Size)
	assert.NotEmpty(t, fingerprint.Hash)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)

	sess := result.Result.Session
	assert.Equal(t, "posit-assistant:"+positAssistantTestMainID, sess.ID)
	assert.Equal(t, AgentPositAssistant, sess.Agent)
	assert.Equal(t, "sales-dashboard", sess.Project)
	assert.Equal(t, "/home/dev/projects/sales-dashboard", sess.Cwd)
	assert.Equal(t, "feature/quarterly-report", sess.GitBranch)
	assert.Equal(t, "devbox", sess.Machine)
	assert.Equal(t, "Sales data loading", sess.SessionName)
	assert.Equal(t, "How do I load the sales data?", sess.FirstMessage)
	assert.Equal(t, 6, sess.MessageCount)
	assert.Equal(t, 2, sess.UserMessageCount)
	assert.Equal(t,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), sess.StartedAt)
	assert.Equal(t,
		time.Date(2025, 1, 1, 0, 0, 40, 0, time.UTC), sess.EndedAt)
	assert.Equal(t, 34+20+5, sess.TotalOutputTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 12+100+100, sess.PeakContextTokens)
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, fingerprint.Hash, sess.File.Hash)
	assert.Empty(t, sess.ParentSessionID)
	assert.Equal(t, 0, sess.MalformedLines)

	msgs := result.Result.Messages
	require.Len(t, msgs, 6)

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "How do I load the sales data?", msgs[0].Content)
	assert.Equal(t,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), msgs[0].Timestamp)

	first := msgs[1]
	assert.Equal(t, RoleAssistant, first.Role)
	assert.Equal(t, "Let me look at the data folder.", first.Content,
		"MESSAGESUMMARY tag must be stripped from displayed text")
	assert.Equal(t, "The user wants to load data.", first.ThinkingText)
	assert.True(t, first.HasThinking)
	assert.True(t, first.HasToolUse)
	assert.Equal(t, "claude-sonnet-4-6", first.Model)
	assert.Equal(t, 212, first.ContextTokens)
	assert.Equal(t, 34, first.OutputTokens)
	assert.JSONEq(t,
		`{
			"input_tokens": 12,
			"output_tokens": 34,
			"cache_creation_input_tokens": 100,
			"cache_read_input_tokens": 100
		}`,
		string(first.TokenUsage))
	require.Len(t, first.ToolCalls, 1)
	assert.Equal(t, "toolu_01", first.ToolCalls[0].ToolUseID)
	assert.Equal(t, "read", first.ToolCalls[0].ToolName)
	assert.Equal(t, "Read", first.ToolCalls[0].Category)
	assert.Equal(t, "data/sales.csv", first.ToolCalls[0].FilePath)
	assert.JSONEq(t, `{"file_path":"data/sales.csv"}`,
		first.ToolCalls[0].InputJSON)

	toolMsg := msgs[2]
	assert.Equal(t, RoleTool, toolMsg.Role)
	require.Len(t, toolMsg.ToolResults, 1)
	assert.Equal(t, "toolu_01", toolMsg.ToolResults[0].ToolUseID)
	assert.Equal(t, len("region,amount\nwest,100\n"),
		toolMsg.ToolResults[0].ContentLength)

	assert.Equal(t, RoleAssistant, msgs[3].Role)
	assert.Equal(t, `Use read.csv("data/sales.csv") to load it.`, msgs[3].Content)
	assert.Equal(t, RoleUser, msgs[4].Role)
	assert.Equal(t, "thanks", msgs[4].Content)
	assert.Equal(t, RoleAssistant, msgs[5].Role)
	assert.Equal(t, "You're welcome!", msgs[5].Content)

	for _, msg := range msgs {
		assert.NotEqual(t, "This branch was abandoned.", msg.Content,
			"inactive tree branches must not appear in the transcript")
	}
}

func TestPositAssistantProviderParseSubagentConversation(t *testing.T) {
	root := positAssistantTestRoot(t)
	provider, ok := NewProvider(AgentPositAssistant, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: positAssistantTestSubID,
	})
	require.NoError(t, err)
	require.True(t, ok)

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	sess := outcome.Results[0].Result.Session
	assert.Equal(t, "posit-assistant:"+positAssistantTestSubID, sess.ID)
	assert.Equal(t,
		"posit-assistant:"+positAssistantTestMainID, sess.ParentSessionID)
	assert.Equal(t, RelSubagent, sess.RelationshipType)
	assert.Equal(t, "sales-dashboard", sess.Project)
	assert.Equal(t,
		"Explore the data folder and report its layout", sess.FirstMessage)

	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 2)
	assert.Equal(t, "claude-haiku-4-5", msgs[1].Model)
}

func TestPositAssistantProviderParseDefaultWorkspace(t *testing.T) {
	root := positAssistantTestRoot(t)
	provider, ok := NewProvider(AgentPositAssistant, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	source, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "33333333-3333-4333-8333-333333333333",
	})
	require.NoError(t, err)
	require.True(t, ok)

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)

	sess := outcome.Results[0].Result.Session
	assert.Equal(t, "unknown", sess.Project)
	assert.Empty(t, sess.Cwd)
	assert.Empty(t, sess.SessionName)
	assert.Empty(t, sess.GitBranch,
		"conversations without recorded gitBranch metadata must parse cleanly")
	assert.Equal(t, "hello", sess.FirstMessage)
}

func TestPositAssistantProviderParseEdgeCases(t *testing.T) {
	convJSON := `{
		"schemaVersion": "3",
		"root": {"id": "44444444-4444-4444-8444-444444444444",
			"timestamp": 1735689600000, "metadata": {"kind": "main"}},
		"messages": [
			{"id": "n1", "parentId": "root", "isActive": true,
				"lmMessageIds": [0], "timestamp": 1735689600000},
			{"id": "n2", "parentId": "n1", "isActive": true,
				"lmMessageIds": [1, 99], "timestamp": 1735689601000}
		],
		"files": []
	}`

	tests := []struct {
		name          string
		conversation  string
		lmMessages    string
		wantSkip      bool
		wantMessages  int
		wantMalformed int
	}{
		{
			name:         "missing lm-messages transcript yields no session",
			conversation: convJSON,
			lmMessages:   "",
			wantSkip:     true,
		},
		{
			name:         "empty conversation file yields no session",
			conversation: "",
			lmMessages:   `{"id":0,"message":{"role":"user","content":"hi"}}` + "\n",
			wantSkip:     true,
		},
		{
			name:         "missing lm IDs and malformed lines are counted",
			conversation: convJSON,
			lmMessages: `{"id":0,"message":{"role":"user","content":"hi"}}` + "\n" +
				"not json\n" +
				`{"id":1,"message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}` + "\n",
			wantMessages:  2,
			wantMalformed: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			convDir := filepath.Join(
				root, "ws1", "44444444-4444-4444-8444-444444444444",
			)
			writeSourceFile(t,
				filepath.Join(convDir, "conversation.json"), tt.conversation)
			if tt.lmMessages != "" {
				writeSourceFile(t,
					filepath.Join(convDir, "lm-messages.jsonl"), tt.lmMessages)
			}

			provider, ok := NewProvider(AgentPositAssistant, ProviderConfig{
				Roots: []string{root},
			})
			require.True(t, ok)
			discovered, err := provider.Discover(context.Background())
			require.NoError(t, err)
			require.Len(t, discovered, 1)

			outcome, err := provider.Parse(context.Background(), ParseRequest{
				Source: discovered[0],
			})
			require.NoError(t, err)
			if tt.wantSkip {
				assert.Empty(t, outcome.Results)
				assert.Equal(t, SkipNoSession, outcome.SkipReason)
				return
			}
			require.Len(t, outcome.Results, 1)
			result := outcome.Results[0].Result
			assert.Len(t, result.Messages, tt.wantMessages)
			assert.Equal(t, tt.wantMalformed, result.Session.MalformedLines)
		})
	}
}

func TestPositAssistantProviderParseSparseTokenUsage(t *testing.T) {
	root := t.TempDir()
	convDir := filepath.Join(root, "ws1", "88888888-8888-4888-8888-888888888888")
	writeSourceFile(t, filepath.Join(convDir, "conversation.json"), `{
		"schemaVersion": "3",
		"root": {"id": "88888888-8888-4888-8888-888888888888",
			"timestamp": 1735689600000, "metadata": {"kind": "main"}},
		"messages": [
			{"id": "n1", "parentId": "88888888-8888-4888-8888-888888888888",
				"isActive": true, "lmMessageIds": [0, 1, 2],
				"timestamp": 1735689600000}
		],
		"files": []
	}`)
	writeSourceFile(t, filepath.Join(convDir, "lm-messages.jsonl"),
		`{"id":0,"message":{"role":"user","content":"hi"}}`+"\n"+
			`{"id":1,"message":{"role":"assistant","content":[{"type":"text","text":"partial"}],"providerOptions":{"providerMetadata":{"positai":{"timestamp":1735689601000,"modelId":"claude-haiku-4-5","usage":{"outputTokens":17}}}}}}`+"\n"+
			`{"id":2,"message":{"role":"assistant","content":[{"type":"text","text":"empty"}],"providerOptions":{"providerMetadata":{"positai":{"timestamp":1735689602000,"modelId":"claude-haiku-4-5","usage":{}}}}}}`+"\n")

	provider, ok := NewProvider(AgentPositAssistant, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: discovered[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 3)

	partial := msgs[1]
	assert.Equal(t, 17, partial.OutputTokens)
	assert.True(t, partial.HasOutputTokens)
	assert.False(t, partial.HasContextTokens)
	assert.JSONEq(t, `{"output_tokens":17}`, string(partial.TokenUsage))

	empty := msgs[2]
	assert.Empty(t, empty.TokenUsage,
		"usage objects without recognized token fields must not mark coverage")
	assert.False(t, empty.HasOutputTokens)
	assert.False(t, empty.HasContextTokens)
}

func TestPositAssistantProviderClassifiesDeletedPaths(t *testing.T) {
	root := t.TempDir()
	convDir := filepath.Join(root, "ws1", "55555555-5555-4555-8555-555555555555")
	convPath := filepath.Join(convDir, "conversation.json")
	lmPath := filepath.Join(convDir, "lm-messages.jsonl")
	writeSourceFile(t, convPath, `{"schemaVersion":"3","root":{},"messages":[]}`)
	writeSourceFile(t, lmPath,
		`{"id":0,"message":{"role":"user","content":"hi"}}`+"\n")

	provider, ok := NewProvider(AgentPositAssistant, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	// A removed transcript must map back to the surviving conversation
	// source so the session reparses without the deleted messages.
	require.NoError(t, os.Remove(lmPath))
	sources, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: lmPath, EventKind: "remove"},
	)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, convPath, sources[0].DisplayPath)

	// A fully deleted conversation still classifies structurally; the engine
	// owns the decision to keep the stored session archived.
	require.NoError(t, os.RemoveAll(convDir))
	sources, err = provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: convPath, EventKind: "remove"},
	)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, convPath, sources[0].DisplayPath)
}

func TestPositAssistantProviderFingerprintTracksTranscriptAppends(t *testing.T) {
	root := t.TempDir()
	convDir := filepath.Join(root, "ws1", "66666666-6666-4666-8666-666666666666")
	convPath := filepath.Join(convDir, "conversation.json")
	lmPath := filepath.Join(convDir, "lm-messages.jsonl")
	writeSourceFile(t, convPath, `{"schemaVersion":"3","root":{},"messages":[]}`)
	writeSourceFile(t, lmPath,
		`{"id":0,"message":{"role":"user","content":"hi"}}`+"\n")

	provider, ok := NewProvider(AgentPositAssistant, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)

	before, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)

	f, err := os.OpenFile(lmPath, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(
		`{"id":1,"message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}` + "\n",
	)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	after, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, after.Hash,
		"appending to lm-messages.jsonl must change the composite fingerprint")
	assert.Greater(t, after.Size, before.Size)
}

func TestPositAssistantProviderFingerprintTracksWorkspaceManifest(t *testing.T) {
	root := t.TempDir()
	wsPath := filepath.Join(root, "ws1", "workspace.json")
	convDir := filepath.Join(root, "ws1", "77777777-7777-4777-8777-777777777777")
	writeSourceFile(t, filepath.Join(convDir, "conversation.json"),
		`{"schemaVersion":"3","root":{},"messages":[]}`)
	writeSourceFile(t, filepath.Join(convDir, "lm-messages.jsonl"),
		`{"id":0,"message":{"role":"user","content":"hi"}}`+"\n")

	provider, ok := NewProvider(AgentPositAssistant, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)

	// Creating the manifest after the fact must invalidate freshness so the
	// session picks up its project and cwd instead of staying "unknown".
	before, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)
	writeSourceFile(t, wsPath, `{"path":"/home/dev/projects/created-later"}`)
	created, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)
	assert.NotEqual(t, before.Hash, created.Hash,
		"creating workspace.json must change the composite fingerprint")

	writeSourceFile(t, wsPath, `{"path":"/home/dev/projects/renamed-app"}`)
	edited, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)
	assert.NotEqual(t, created.Hash, edited.Hash,
		"editing workspace.json must change the composite fingerprint")
}
