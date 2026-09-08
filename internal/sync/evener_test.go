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

func TestEvenerArchiveLifecycle(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	sessions := filepath.Join(root, "projects", "project-demo", "sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o755))
	source, err := os.ReadFile("testdata/evener/demo.transcript.jsonl")
	require.NoError(t, err)
	path := filepath.Join(sessions, "demo.transcript.jsonl")
	metaPath := filepath.Join(sessions, "demo.meta.json")
	require.NoError(t, os.WriteFile(path, source, 0o600))
	require.NoError(t, os.WriteFile(metaPath, []byte(`{"id":"demo","name":"Orchard investigation"}`), 0o600))
	engine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentType("evener"): {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	ctx := t.Context()
	first := engine.SyncAll(ctx, nil)
	require.Equal(t, 1, first.Synced)
	require.Zero(t, first.Failed)
	session, err := database.GetSessionFull(ctx, "evener:demo")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "evener", session.Agent)
	require.NotNil(t, session.SessionName)
	assert.Equal(t, "Orchard investigation", *session.SessionName)
	messages, err := database.GetMessages(ctx, session.ID, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	assert.Equal(t, "Check the state before changing it.", messages[1].ThinkingText)
	assert.Equal(t, 20, messages[1].OutputTokens)
	assert.Equal(t, 130, messages[1].ContextTokens)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "call-1", messages[1].ToolCalls[0].ToolUseID)
	assert.Contains(t, messages[1].ToolCalls[0].ResultContent, "/workspace/demo")
	found, err := database.Search(ctx, db.SearchFilter{Query: "orchard"})
	require.NoError(t, err)
	require.NotEmpty(t, found.Results)
	assert.Equal(t, session.ID, found.Results[0].SessionID)

	exported, err := database.LoadArtifactExportData(ctx, session.ID, db.ArtifactExportLoadLimits{
		Messages: 100, UsageEvents: 100, MessageToolCalls: 100, ToolResultEvents: 100,
		SessionToolCalls: 100, SessionResultEvents: 100, MessageBytes: 1 << 20, UsageBytes: 1 << 20,
	})
	require.NoError(t, err)
	require.Len(t, exported.Messages, len(messages))
	assert.Equal(t, messages[1].ThinkingText, exported.Messages[1].ThinkingText)
	assert.Equal(t, messages[1].TokenUsage, exported.Messages[1].TokenUsage)
	require.Len(t, exported.Messages[1].ToolCalls, 1)
	require.Len(t, exported.Messages[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "/workspace/demo", exported.Messages[1].ToolCalls[0].ResultEvents[0].Content)

	unchanged := engine.SyncAll(ctx, nil)
	require.Zero(t, unchanged.Failed)
	assert.Zero(t, unchanged.Synced)

	// Only a sibling metadata write changes; the transcript remains untouched.
	beforeMetadata := engine.SourceMtime(session.ID)
	require.NoError(t, os.WriteFile(metaPath, []byte(`{"id":"demo","name":"Orchard diagnosis"}`), 0o600))
	assert.NotEqual(t, beforeMetadata, engine.SourceMtime(session.ID), "session polling must notice metadata-only updates")
	renamed := engine.SyncAll(ctx, nil)
	require.Zero(t, renamed.Failed)
	session, err = database.GetSessionFull(ctx, session.ID)
	require.NoError(t, err)
	require.NotNil(t, session.SessionName)
	assert.Equal(t, "Orchard diagnosis", *session.SessionName)

	tail := `{"kind":"entry","seq":4,"turn":{"kind":"ASSISTANT","timestamp":"2026-09-05T10:00:04Z","message":{"role":"assistant","content":[{"kind":"text","text":"The orchard is synchronized."}]},"response_model":"gpt-4.1","usage":{"input_tokens":10,"output_tokens":4}}}`
	require.NoError(t, os.WriteFile(path, append(append([]byte{}, source...), []byte(tail[:len(tail)/2])...), 0o600))
	partial := engine.SyncAll(ctx, nil)
	assert.Positive(t, partial.Failed, "incomplete sources stay eligible for retry")
	messages, err = database.GetMessages(ctx, session.ID, 0, 100, true)
	require.NoError(t, err)
	assert.Len(t, messages, 3)
	require.NoError(t, os.WriteFile(path, append(append([]byte{}, source...), []byte(tail+"\n")...), 0o600))
	complete := engine.SyncAll(ctx, nil)
	require.Zero(t, complete.Failed)
	messages, err = database.GetMessages(ctx, session.ID, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 4)
	assert.Equal(t, "The orchard is synchronized.", messages[3].Content)

	// An unfinished rewrite cannot replace the complete archive with a prefix.
	lines := strings.Split(strings.TrimSpace(string(source)), "\n")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines[:2], "\n")+"\n"+tail[:len(tail)/2]), 0o600))
	rewrite := engine.SyncAll(ctx, nil)
	assert.Positive(t, rewrite.Failed)
	messages, err = database.GetMessages(ctx, session.ID, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 4)
	assert.Equal(t, "The orchard is synchronized.", messages[3].Content)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, 20, messages[1].OutputTokens)

	// Replacing a source must remove obsolete messages instead of appending.
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines[:2], "\n")+"\n"), 0o600))
	replaced := engine.SyncAll(ctx, nil)
	require.Zero(t, replaced.Failed)
	messages, err = database.GetMessages(ctx, session.ID, 0, 100, true)
	require.NoError(t, err)
	assert.Len(t, messages, 1)

	// Removing provider metadata clears its title without deleting the session.
	require.NoError(t, os.Remove(metaPath))
	removedMeta := engine.SyncAll(ctx, nil)
	require.Zero(t, removedMeta.Failed)
	session, err = database.GetSessionFull(ctx, session.ID)
	require.NoError(t, err)
	require.NotNil(t, session)
	if session.SessionName != nil {
		assert.Empty(t, *session.SessionName)
	}

	// Bad input must not replace the last good archive contents.
	require.NoError(t, os.WriteFile(path, []byte(lines[0]+"\n{bad}\n"), 0o600))
	corrupt := engine.SyncAll(ctx, nil)
	assert.Equal(t, 1, corrupt.Failed)
	messages, err = database.GetMessages(ctx, session.ID, 0, 100, true)
	require.NoError(t, err)
	assert.Len(t, messages, 1)
}

func TestEvenerRelationshipsAndParentArrival(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o755))
	parent, err := os.ReadFile("testdata/evener/demo.transcript.jsonl")
	require.NoError(t, err)
	child := strings.Replace(string(parent), `"session_id":"demo"`, `"session_id":"fork","parent_session_id":"demo"`, 1)
	child += "{\"kind\":\"entry\",\"seq\":4,\"turn\":{\"kind\":\"USER_INPUT\",\"message\":{\"role\":\"user\",\"content\":[{\"kind\":\"text\",\"text\":\"Try the fork approach\"}]}}}\n"
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "fork.transcript.jsonl"), []byte(child), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "fork.meta.json"), []byte(`{"id":"fork","parent_session_id":"demo","divergence_turn":4}`), 0o600))
	engine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentEvener: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	ctx := t.Context()
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	messages, err := database.GetMessages(ctx, "evener:fork", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 4, "unavailable parent keeps shared content")
	// An unfinished parent is not archived, so its copied history stays in
	// the complete child until both sources can be imported.
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "demo.transcript.jsonl"), append(append([]byte{}, parent...), []byte(`{"kind":"entry","seq":4`)...), 0o600))
	assert.Positive(t, engine.SyncAll(ctx, nil).Failed)
	messages, err = database.GetMessages(ctx, "evener:fork", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 4, "unfinished parent cannot own the copied prefix")
	assert.Equal(t, 20, messages[1].OutputTokens)
	before := engine.SourceMtime("evener:fork")
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "demo.transcript.jsonl"), parent, 0o600))
	assert.NotEqual(t, before, engine.SourceMtime("evener:fork"))
	parentMeta := filepath.Join(sessions, "demo.meta.json")
	for _, invalid := range []string{`{bad}`, `{"id":"another-session"}`} {
		require.NoError(t, os.WriteFile(parentMeta, []byte(invalid), 0o600))
		assert.Positive(t, engine.SyncAll(ctx, nil).Failed)
		messages, err = database.GetMessages(ctx, "evener:fork", 0, 100, true)
		require.NoError(t, err)
		require.Len(t, messages, 4, "invalid parent metadata prevents prefix ownership")
		assert.Equal(t, 20, messages[1].OutputTokens)
	}
	before = engine.SourceMtime("evener:fork")
	require.NoError(t, os.Remove(parentMeta))
	assert.NotEqual(t, before, engine.SourceMtime("evener:fork"), "parent metadata removal refreshes child")
	subagent := strings.Replace(string(parent), `"session_id":"demo"`, `"session_id":"worker","parent_session_id":"demo"`, 1)
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "worker.transcript.jsonl"), []byte(subagent), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "worker.meta.json"), []byte(`{"id":"worker","parent_session_id":"demo","is_subagent":true}`), 0o600))
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	for _, tc := range []struct {
		id, relationship string
		count, output    int
	}{{"fork", string(parser.RelFork), 1, 0}, {"worker", string(parser.RelSubagent), 3, 20}} {
		session, err := database.GetSessionFull(ctx, "evener:"+tc.id)
		require.NoError(t, err)
		require.NotNil(t, session)
		require.NotNil(t, session.ParentSessionID)
		assert.Equal(t, "evener:demo", *session.ParentSessionID)
		assert.Equal(t, tc.relationship, session.RelationshipType)
		messages, err := database.GetMessages(ctx, session.ID, 0, 100, true)
		require.NoError(t, err)
		assert.Len(t, messages, tc.count)
		output := 0
		for _, msg := range messages {
			output += msg.OutputTokens
		}
		assert.Equal(t, tc.output, output)
	}
	assert.Zero(t, engine.SyncAll(ctx, nil).Synced)
}

func TestEvenerRemoteImportDoesNotStampStatDigest(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "demo.transcript.jsonl")
	source, err := os.ReadFile("testdata/evener/demo.transcript.jsonl")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, source, 0o600))
	rewrite := func(p string) string { return "remote:" + p }
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentEvener: {root}},
		Machine:   "remote", PathRewriter: rewrite, Ephemeral: true,
	})
	t.Cleanup(engine.Close)
	for range 2 {
		stats := engine.SyncAll(t.Context(), nil)
		require.Zero(t, stats.Failed)
		for _, key := range []string{path, rewrite(path)} {
			_, exists, err := database.GetProviderStatHash(t.Context(), parser.AgentEvener, key)
			require.NoError(t, err)
			assert.False(t, exists, "remote freshness must verify content")
		}
	}
}
