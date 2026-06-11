package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestSyncPathsCommandCode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	commandCodeDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCommandCode: {commandCodeDir},
		},
		Machine: "local",
	})

	sessionID := "adc026b4-c620-43e4-8cc4-295593889d18"
	projectDir := filepath.Join(commandCodeDir, "users-alice-code-sample-project")
	path := filepath.Join(projectDir, sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(projectDir, 0o755), "MkdirAll(%q)", projectDir)
	require.NoError(t, os.WriteFile(path, []byte(
		`{"id":"m1","timestamp":"2026-06-01T10:00:00Z","sessionId":"adc026b4-c620-43e4-8cc4-295593889d18","role":"user","content":[{"type":"text","text":"Inspect server logs"}],"gitBranch":"feature/command-code","metadata":{"version":2,"cwd":"/Users/alice/code/sample-project"}}
{"id":"m2","timestamp":"2026-06-01T10:00:01Z","sessionId":"adc026b4-c620-43e4-8cc4-295593889d18","role":"assistant","content":[{"type":"text","text":"The error is in startup."}],"gitBranch":"feature/command-code","metadata":{"version":2}}
`), 0o644), "WriteFile(%q)", path)
	require.NoError(t, os.WriteFile(
		filepath.Join(projectDir, sessionID+".meta.json"),
		[]byte(`{"title":"Startup investigation"}`),
		0o644,
	), "WriteFile(meta)")

	engine.SyncPaths([]string{path})

	assertSessionState(t, testDB, "commandcode:"+sessionID, func(sess *db.Session) {
		assert.Equal(t, "commandcode", sess.Agent)
		assert.Equal(t, "sample_project", sess.Project)
		require.NotNil(t, sess.FirstMessage)
		assert.Equal(t, "Inspect server logs", *sess.FirstMessage)
		require.NotNil(t, sess.DisplayName)
		assert.Equal(t, "Startup investigation", *sess.DisplayName)
		assert.Equal(t, 2, sess.MessageCount)
		assert.Equal(t, 1, sess.UserMessageCount)
	})
}

func TestSyncPathsCommandCodeMetaArrivalTriggersResync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	commandCodeDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCommandCode: {commandCodeDir},
		},
		Machine: "local",
	})

	sessionID := "adc026b4-c620-43e4-8cc4-295593889d18"
	projectDir := filepath.Join(commandCodeDir, "users-alice-code-sample-project")
	jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
	metaPath := filepath.Join(projectDir, sessionID+".meta.json")
	require.NoError(t, os.MkdirAll(projectDir, 0o755), "MkdirAll(%q)", projectDir)
	require.NoError(t, os.WriteFile(jsonlPath, []byte(
		`{"id":"m1","timestamp":"2026-06-01T10:00:00Z","sessionId":"adc026b4-c620-43e4-8cc4-295593889d18","role":"user","content":[{"type":"text","text":"Inspect server logs"}],"gitBranch":"feature/command-code","metadata":{"version":2}}
{"id":"m2","timestamp":"2026-06-01T10:00:01Z","sessionId":"adc026b4-c620-43e4-8cc4-295593889d18","role":"assistant","content":[{"type":"text","text":"The error is in startup."}],"gitBranch":"feature/command-code","metadata":{"version":2}}
`), 0o644), "WriteFile(%q)", jsonlPath)

	engine.SyncPaths([]string{jsonlPath})
	assertSessionState(t, testDB, "commandcode:"+sessionID, func(sess *db.Session) {
		assert.Equal(t, "users_alice_code_sample_project", sess.Project)
		assert.Empty(t, sess.Cwd)
	})

	// Ensure the later meta file is included in the effective snapshot and
	// reparses the unchanged transcript to fill fallback cwd/project metadata.
	require.NoError(t, os.WriteFile(metaPath, []byte(`{"title":"Startup investigation","cwd":"/Users/alice/code/sample-project"}`), 0o644), "WriteFile(meta)")
	require.NoError(t, os.Chtimes(metaPath, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second)))

	engine.SyncPaths([]string{metaPath})
	assertSessionState(t, testDB, "commandcode:"+sessionID, func(sess *db.Session) {
		require.NotNil(t, sess.DisplayName)
		assert.Equal(t, "Startup investigation", *sess.DisplayName)
		assert.Equal(t, "sample_project", sess.Project)
		assert.Equal(t, "/Users/alice/code/sample-project", sess.Cwd)
	})
}

func TestSyncAllSinceCommandCodeMetaArrivalTriggersResync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	commandCodeDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCommandCode: {commandCodeDir},
		},
		Machine: "local",
	})

	sessionID := "adc026b4-c620-43e4-8cc4-295593889d18"
	projectDir := filepath.Join(commandCodeDir, "users-alice-code-sample-project")
	jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
	metaPath := filepath.Join(projectDir, sessionID+".meta.json")
	require.NoError(t, os.MkdirAll(projectDir, 0o755), "MkdirAll(%q)", projectDir)
	require.NoError(t, os.WriteFile(jsonlPath, []byte(
		`{"id":"m1","timestamp":"2026-06-01T10:00:00Z","sessionId":"adc026b4-c620-43e4-8cc4-295593889d18","role":"user","content":[{"type":"text","text":"Inspect server logs"}],"gitBranch":"feature/command-code","metadata":{"version":2}}
{"id":"m2","timestamp":"2026-06-01T10:00:01Z","sessionId":"adc026b4-c620-43e4-8cc4-295593889d18","role":"assistant","content":[{"type":"text","text":"The error is in startup."}],"gitBranch":"feature/command-code","metadata":{"version":2}}
`), 0o644), "WriteFile(%q)", jsonlPath)

	engine.SyncPaths([]string{jsonlPath})
	assertSessionState(t, testDB, "commandcode:"+sessionID, func(sess *db.Session) {
		assert.Equal(t, "users_alice_code_sample_project", sess.Project)
		assert.Empty(t, sess.Cwd)
	})

	cutoff := time.Now()
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(metaPath, []byte(`{"title":"Startup investigation","cwd":"/Users/alice/code/sample-project"}`), 0o644), "WriteFile(meta)")
	require.NoError(t, os.Chtimes(metaPath, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second)))

	stats := engine.SyncAllSince(context.Background(), cutoff, nil)
	require.Equal(t, 1, stats.Synced, "synced = %d, want 1", stats.Synced)

	assertSessionState(t, testDB, "commandcode:"+sessionID, func(sess *db.Session) {
		require.NotNil(t, sess.DisplayName)
		assert.Equal(t, "Startup investigation", *sess.DisplayName)
		assert.Equal(t, "sample_project", sess.Project)
		assert.Equal(t, "/Users/alice/code/sample-project", sess.Cwd)
	})
}

func TestSourceMtimeCommandCodeIncludesMetaMtime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	commandCodeDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCommandCode: {commandCodeDir},
		},
		Machine: "local",
	})

	sessionID := "adc026b4-c620-43e4-8cc4-295593889d18"
	projectDir := filepath.Join(commandCodeDir, "users-alice-code-sample-project")
	jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
	metaPath := filepath.Join(projectDir, sessionID+".meta.json")
	require.NoError(t, os.MkdirAll(projectDir, 0o755), "MkdirAll(%q)", projectDir)
	require.NoError(t, os.WriteFile(jsonlPath, []byte(
		`{"id":"m1","timestamp":"2026-06-01T10:00:00Z","sessionId":"adc026b4-c620-43e4-8cc4-295593889d18","role":"user","content":[{"type":"text","text":"Inspect server logs"}],"metadata":{"version":2}}
`), 0o644), "WriteFile(%q)", jsonlPath)
	require.NoError(t, os.WriteFile(metaPath, []byte(`{"title":"Startup investigation"}`), 0o644), "WriteFile(meta)")

	jsonlTime := time.Unix(1_717_238_800, 0)
	metaTime := jsonlTime.Add(5 * time.Second)
	require.NoError(t, os.Chtimes(jsonlPath, jsonlTime, jsonlTime))
	require.NoError(t, os.Chtimes(metaPath, metaTime, metaTime))

	engine.SyncPaths([]string{jsonlPath})
	assert.Equal(t, metaTime.UnixNano(),
		engine.SourceMtime("commandcode:"+sessionID))
}
