package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestEvenerRespectsBlockedResultCategories(t *testing.T) {
	for _, category := range []string{"Read", "Glob", "Bash"} {
		t.Run(category, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			database := openTestDB(t)
			root := t.TempDir()
			sessions := filepath.Join(root, "sessions")
			require.NoError(os.MkdirAll(sessions, 0o755))
			source, err := os.ReadFile("testdata/evener/demo.transcript.jsonl")
			require.NoError(err)
			text := strings.ReplaceAll(string(source), "exec_command", category)
			text = strings.ReplaceAll(text, `"content":"/workspace/demo"`, `"content":"result-marker"`)
			require.NoError(os.WriteFile(filepath.Join(sessions, "demo.transcript.jsonl"), []byte(text), 0o600))
			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentEvener: {root}}, Machine: "local",
				BlockedResultCategories: []string{"Read", "Glob"},
			})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			require.Zero(stats.Failed)
			require.Equal(1, stats.Synced)
			messages, err := database.GetMessages(t.Context(), "evener:demo", 0, 100, true)
			require.NoError(err)
			require.Len(messages, 3)
			require.Len(messages[1].ToolCalls, 1)
			call := messages[1].ToolCalls[0]
			assert.Equal(category, call.Category)
			require.Len(call.ResultEvents, 1)
			if category == "Bash" {
				assert.Equal("result-marker", call.ResultContent)
				assert.Equal("result-marker", call.ResultEvents[0].Content)
			} else {
				assert.Empty(call.ResultContent)
				assert.Empty(call.ResultEvents[0].Content)
			}
			assert.Empty(messages[2].Content, "tool result bodies must not bypass filtering as message text")
			assert.Equal(13, messages[2].ContentLength)
			assert.Contains(messages[1].Content, "I will inspect the orchard.")
		})
	}
}

func TestEvenerArchiveLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := openTestDB(t)
	root := t.TempDir()
	sessions := filepath.Join(root, "projects", "project-demo", "sessions")
	require.NoError(os.MkdirAll(sessions, 0o755))
	source, err := os.ReadFile("testdata/evener/demo.transcript.jsonl")
	require.NoError(err)
	path := filepath.Join(sessions, "demo.transcript.jsonl")
	metaPath := filepath.Join(sessions, "demo.meta.json")
	require.NoError(os.WriteFile(path, source, 0o600))
	require.NoError(os.WriteFile(metaPath, []byte(`{"id":"demo","name":"Orchard investigation"}`), 0o600))
	engine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentType("evener"): {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	ctx := t.Context()
	first := engine.SyncAll(ctx, nil)
	require.Equal(1, first.Synced)
	require.Zero(first.Failed)
	session, err := database.GetSessionFull(ctx, "evener:demo")
	require.NoError(err)
	require.NotNil(session)
	assert.Equal("evener", session.Agent)
	require.NotNil(session.SessionName)
	assert.Equal("Orchard investigation", *session.SessionName)
	messages, err := database.GetMessages(ctx, session.ID, 0, 100, true)
	require.NoError(err)
	require.Len(messages, 3)
	assert.Equal("Check the state before changing it.", messages[1].ThinkingText)
	assert.Equal(20, messages[1].OutputTokens)
	assert.Equal(130, messages[1].ContextTokens)
	require.Len(messages[1].ToolCalls, 1)
	assert.Equal("call-1", messages[1].ToolCalls[0].ToolUseID)
	assert.Contains(messages[1].ToolCalls[0].ResultContent, "/workspace/demo")
	found, err := database.Search(ctx, db.SearchFilter{Query: "orchard"})
	require.NoError(err)
	require.NotEmpty(found.Results)
	assert.Equal(session.ID, found.Results[0].SessionID)

	exported, err := database.LoadArtifactExportData(ctx, session.ID, db.ArtifactExportLoadLimits{
		Messages: 100, UsageEvents: 100, MessageToolCalls: 100, ToolResultEvents: 100,
		SessionToolCalls: 100, SessionResultEvents: 100, MessageBytes: 1 << 20, UsageBytes: 1 << 20,
	})
	require.NoError(err)
	require.Len(exported.Messages, len(messages))
	assert.Equal(messages[1].ThinkingText, exported.Messages[1].ThinkingText)
	assert.Equal(messages[1].TokenUsage, exported.Messages[1].TokenUsage)
	require.Len(exported.Messages[1].ToolCalls, 1)
	require.Len(exported.Messages[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal("/workspace/demo", exported.Messages[1].ToolCalls[0].ResultEvents[0].Content)

	unchanged := engine.SyncAll(ctx, nil)
	require.Zero(unchanged.Failed)
	assert.Zero(unchanged.Synced)

	// Only a sibling metadata write changes; the transcript remains untouched.
	beforeMetadata := engine.SourceMtime(ctx, session.ID)
	require.NoError(os.WriteFile(metaPath, []byte(`{"id":"demo","name":"Orchard diagnosis"}`), 0o600))
	assert.NotEqual(beforeMetadata, engine.SourceMtime(ctx, session.ID), "session polling must notice metadata-only updates")
	renamed := engine.SyncAll(ctx, nil)
	require.Zero(renamed.Failed)
	session, err = database.GetSessionFull(ctx, session.ID)
	require.NoError(err)
	require.NotNil(session.SessionName)
	assert.Equal("Orchard diagnosis", *session.SessionName)

	tail := `{"kind":"entry","seq":4,"turn":{"kind":"ASSISTANT","timestamp":"2026-09-05T10:00:04Z","message":{"role":"assistant","content":[{"kind":"text","text":"The orchard is synchronized."}]},"response_model":"gpt-4.1","usage":{"input_tokens":10,"output_tokens":4}}}`
	require.NoError(os.WriteFile(path, append(append([]byte{}, source...), []byte(tail[:len(tail)/2])...), 0o600))
	partial := engine.SyncAll(ctx, nil)
	assert.Positive(partial.Failed, "incomplete sources stay eligible for retry")
	messages, err = database.GetMessages(ctx, session.ID, 0, 100, true)
	require.NoError(err)
	assert.Len(messages, 3)
	require.NoError(os.WriteFile(path, append(append([]byte{}, source...), []byte(tail+"\n")...), 0o600))
	complete := engine.SyncAll(ctx, nil)
	require.Zero(complete.Failed)
	messages, err = database.GetMessages(ctx, session.ID, 0, 100, true)
	require.NoError(err)
	require.Len(messages, 4)
	assert.Equal("The orchard is synchronized.", messages[3].Content)

	// An unfinished rewrite cannot replace the complete archive with a prefix.
	lines := strings.Split(strings.TrimSpace(string(source)), "\n")
	require.NoError(os.WriteFile(path, []byte(strings.Join(lines[:2], "\n")+"\n"+tail[:len(tail)/2]), 0o600))
	rewrite := engine.SyncAll(ctx, nil)
	assert.Positive(rewrite.Failed)
	messages, err = database.GetMessages(ctx, session.ID, 0, 100, true)
	require.NoError(err)
	require.Len(messages, 4)
	assert.Equal("The orchard is synchronized.", messages[3].Content)
	require.Len(messages[1].ToolCalls, 1)
	assert.Equal(20, messages[1].OutputTokens)

	// Replacing a source must remove obsolete messages instead of appending.
	require.NoError(os.WriteFile(path, []byte(strings.Join(lines[:2], "\n")+"\n"), 0o600))
	replaced := engine.SyncAll(ctx, nil)
	require.Zero(replaced.Failed)
	messages, err = database.GetMessages(ctx, session.ID, 0, 100, true)
	require.NoError(err)
	assert.Len(messages, 1)

	// Removing provider metadata clears its title without deleting the session.
	require.NoError(os.Remove(metaPath))
	removedMeta := engine.SyncAll(ctx, nil)
	require.Zero(removedMeta.Failed)
	session, err = database.GetSessionFull(ctx, session.ID)
	require.NoError(err)
	require.NotNil(session)
	if session.SessionName != nil {
		assert.Empty(*session.SessionName)
	}

	// Bad input must not replace the last good archive contents.
	require.NoError(os.WriteFile(path, []byte(lines[0]+"\n{bad}\n"), 0o600))
	corrupt := engine.SyncAll(ctx, nil)
	assert.Equal(1, corrupt.Failed)
	messages, err = database.GetMessages(ctx, session.ID, 0, 100, true)
	require.NoError(err)
	assert.Len(messages, 1)
}

func TestEvenerRelationshipsAndParentArrival(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := openTestDB(t)
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	require.NoError(os.MkdirAll(sessions, 0o755))
	parent, err := os.ReadFile("testdata/evener/demo.transcript.jsonl")
	require.NoError(err)
	child := strings.Replace(string(parent), `"session_id":"demo"`, `"session_id":"fork","parent_session_id":"demo"`, 1)
	child += "{\"kind\":\"entry\",\"seq\":4,\"turn\":{\"kind\":\"USER_INPUT\",\"message\":{\"role\":\"user\",\"content\":[{\"kind\":\"text\",\"text\":\"Try the fork approach\"}]}}}\n"
	require.NoError(os.WriteFile(filepath.Join(sessions, "fork.transcript.jsonl"), []byte(child), 0o600))
	require.NoError(os.WriteFile(filepath.Join(sessions, "fork.meta.json"), []byte(`{"id":"fork","parent_session_id":"demo","divergence_turn":4}`), 0o600))
	engine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentEvener: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	ctx := t.Context()
	require.Zero(engine.SyncAll(ctx, nil).Failed)
	messages, err := database.GetMessages(ctx, "evener:fork", 0, 100, true)
	require.NoError(err)
	require.Len(messages, 4, "unavailable parent keeps shared content")
	// An unfinished parent is not archived, so its copied history stays in
	// the complete child until both sources can be imported.
	require.NoError(os.WriteFile(filepath.Join(sessions, "demo.transcript.jsonl"), append(append([]byte{}, parent...), []byte(`{"kind":"entry","seq":4`)...), 0o600))
	assert.Positive(engine.SyncAll(ctx, nil).Failed)
	messages, err = database.GetMessages(ctx, "evener:fork", 0, 100, true)
	require.NoError(err)
	require.Len(messages, 4, "unfinished parent cannot own the copied prefix")
	assert.Equal(20, messages[1].OutputTokens)
	before := engine.SourceMtime(ctx, "evener:fork")
	require.NoError(os.WriteFile(filepath.Join(sessions, "demo.transcript.jsonl"), parent, 0o600))
	assert.NotEqual(before, engine.SourceMtime(ctx, "evener:fork"))
	parentMeta := filepath.Join(sessions, "demo.meta.json")
	for _, invalid := range []string{`{bad}`, `{"id":"another-session"}`} {
		require.NoError(os.WriteFile(parentMeta, []byte(invalid), 0o600))
		assert.Positive(engine.SyncAll(ctx, nil).Failed)
		messages, err = database.GetMessages(ctx, "evener:fork", 0, 100, true)
		require.NoError(err)
		require.Len(messages, 4, "invalid parent metadata prevents prefix ownership")
		assert.Equal(20, messages[1].OutputTokens)
	}
	before = engine.SourceMtime(ctx, "evener:fork")
	require.NoError(os.Remove(parentMeta))
	assert.NotEqual(before, engine.SourceMtime(ctx, "evener:fork"), "parent metadata removal refreshes child")
	subagent := strings.Replace(string(parent), `"session_id":"demo"`, `"session_id":"worker","parent_session_id":"demo"`, 1)
	require.NoError(os.WriteFile(filepath.Join(sessions, "worker.transcript.jsonl"), []byte(subagent), 0o600))
	require.NoError(os.WriteFile(filepath.Join(sessions, "worker.meta.json"), []byte(`{"id":"worker","parent_session_id":"demo","is_subagent":true}`), 0o600))
	require.Zero(engine.SyncAll(ctx, nil).Failed)
	for _, tc := range []struct {
		id, relationship string
		count, output    int
	}{{"fork", string(parser.RelFork), 1, 0}, {"worker", string(parser.RelSubagent), 3, 20}} {
		session, err := database.GetSessionFull(ctx, "evener:"+tc.id)
		require.NoError(err)
		require.NotNil(session)
		require.NotNil(session.ParentSessionID)
		assert.Equal("evener:demo", *session.ParentSessionID)
		assert.Equal(tc.relationship, session.RelationshipType)
		messages, err := database.GetMessages(ctx, session.ID, 0, 100, true)
		require.NoError(err)
		assert.Len(messages, tc.count)
		output := 0
		for _, msg := range messages {
			output += msg.OutputTokens
		}
		assert.Equal(tc.output, output)
	}
	assert.Zero(engine.SyncAll(ctx, nil).Synced)
}

func TestEvenerRemoteImportDoesNotStampStatDigest(t *testing.T) {
	require := require.New(t)

	database := openTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "demo.transcript.jsonl")
	source, err := os.ReadFile("testdata/evener/demo.transcript.jsonl")
	require.NoError(err)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(os.WriteFile(path, source, 0o600))
	rewrite := func(p string) string { return "remote:" + p }
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentEvener: {root}},
		Machine:   "remote", PathRewriter: rewrite, Ephemeral: true,
	})
	t.Cleanup(engine.Close)
	for range 2 {
		stats := engine.SyncAll(t.Context(), nil)
		require.Zero(stats.Failed)
		for _, key := range []string{path, rewrite(path)} {
			_, exists, err := database.GetProviderStatHash(t.Context(), parser.AgentEvener, key)
			require.NoError(err)
			assert.False(t, exists, "remote freshness must verify content")
		}
	}
}
