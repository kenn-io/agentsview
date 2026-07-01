package sync_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

type coworkSyncFixture struct {
	sessionUUID string
	cliID       string
	title       string
}

func writeCoworkSyncFixture(
	t *testing.T, root string, f coworkSyncFixture,
) (metaPath, transcriptPath string) {
	t.Helper()

	sessionDirName := "local_" + f.sessionUUID
	workspaceDir := filepath.Join(root, "org", "workspace")
	metaPath = filepath.Join(workspaceDir, sessionDirName+".json")
	meta := map[string]any{
		"sessionId":      sessionDirName,
		"cliSessionId":   f.cliID,
		"title":          f.title,
		"createdAt":      int64(1_700_000_000_000),
		"lastActivityAt": int64(1_700_000_001_000),
	}
	data, err := json.Marshal(meta)
	require.NoError(t, err, "marshal cowork metadata")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755), "mkdir workspace")
	require.NoError(t, os.WriteFile(metaPath, data, 0o644), "write metadata")

	projectDir := filepath.Join(
		workspaceDir, sessionDirName, ".claude", "projects", "-sessions-demo",
	)
	require.NoError(t, os.MkdirAll(projectDir, 0o755), "mkdir project")
	transcriptPath = filepath.Join(projectDir, f.cliID+".jsonl")
	lines := []string{
		`{"type":"user","uuid":"u1","parentUuid":null,` +
			`"sessionId":"` + f.cliID + `","cwd":"/sessions/test",` +
			`"timestamp":"2026-03-01T10:00:00.000Z",` +
			`"message":{"role":"user","content":"hello there"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1",` +
			`"sessionId":"` + f.cliID + `","timestamp":"2026-03-01T10:00:05.000Z",` +
			`"message":{"role":"assistant","id":"msg_1",` +
			`"model":"claude-sonnet-4-6","stop_reason":"end_turn",` +
			`"content":[{"type":"text","text":"hi back"}],` +
			`"usage":{"input_tokens":10,"output_tokens":5}}}`,
	}
	require.NoError(t,
		os.WriteFile(transcriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644),
		"write transcript",
	)
	return metaPath, transcriptPath
}

func TestSyncAllSinceCoworkMetaUpdateTriggersResync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	coworkDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCowork: {coworkDir},
		},
		Machine: "local",
	})

	sessionID := "3021ea26-c727-48d2-934a-3e169c6ab04e"
	metaPath, transcriptPath := writeCoworkSyncFixture(t, coworkDir, coworkSyncFixture{
		sessionUUID: "0b4eea33-12a0-42ac-856b-98d61a4717c3",
		cliID:       sessionID,
		title:       "Before rename",
	})

	engine.SyncPaths([]string{transcriptPath})
	assertSessionState(t, testDB, "cowork:"+sessionID, func(sess *db.Session) {
		require.NotNil(t, sess.DisplayName)
		assert.Equal(t, "Before rename", *sess.DisplayName)
	})

	transcriptTime := time.Unix(1_781_475_210, 0)
	metaTime := transcriptTime.Add(time.Second)
	require.NoError(t, os.Chtimes(transcriptPath, transcriptTime, transcriptTime))
	require.NoError(t, os.WriteFile(metaPath, []byte(
		`{"sessionId":"local_0b4eea33-12a0-42ac-856b-98d61a4717c3",`+
			`"cliSessionId":"`+sessionID+`","title":"After rename"}`,
	), 0o644), "rewrite metadata")
	require.NoError(t, os.Chtimes(metaPath, metaTime, metaTime))

	cutoff := transcriptTime.Add(500 * time.Millisecond)
	stats := engine.SyncAllSince(context.Background(), cutoff, nil)
	require.Equal(t, 1, stats.Synced, "synced = %d, want 1", stats.Synced)

	assertSessionState(t, testDB, "cowork:"+sessionID, func(sess *db.Session) {
		require.NotNil(t, sess.DisplayName)
		assert.Equal(t, "After rename", *sess.DisplayName)
	})
}

func TestSourceMtimeCoworkIncludesMetaMtime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	coworkDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCowork: {coworkDir},
		},
		Machine: "local",
	})

	sessionID := "3021ea26-c727-48d2-934a-3e169c6ab04e"
	metaPath, transcriptPath := writeCoworkSyncFixture(t, coworkDir, coworkSyncFixture{
		sessionUUID: "0b4eea33-12a0-42ac-856b-98d61a4717c3",
		cliID:       sessionID,
		title:       "Source mtime",
	})

	transcriptTime := time.Unix(1_781_475_210, 0)
	metaTime := transcriptTime.Add(time.Second)
	require.NoError(t, os.Chtimes(transcriptPath, transcriptTime, transcriptTime))
	require.NoError(t, os.Chtimes(metaPath, metaTime, metaTime))

	engine.SyncPaths([]string{transcriptPath})
	assert.Equal(t, metaTime.UnixNano(), engine.SourceMtime("cowork:"+sessionID))
}

func TestSyncPathsCoworkReplacesUpdatedMessageOrdinal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	coworkDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCowork: {coworkDir},
		},
		Machine: "local",
	})

	sessionID := "3021ea26-c727-48d2-934a-3e169c6ab04e"
	_, transcriptPath := writeCoworkSyncFixture(t, coworkDir, coworkSyncFixture{
		sessionUUID: "0b4eea33-12a0-42ac-856b-98d61a4717c3",
		cliID:       sessionID,
		title:       "Streaming update",
	})

	engine.SyncPaths([]string{transcriptPath})
	assertCoworkAssistantContent(t, testDB, sessionID, "hi back")

	data, err := os.ReadFile(transcriptPath)
	require.NoError(t, err, "read transcript")
	updated := strings.Replace(string(data), "hi back", "updated answer", 1)
	require.NoError(t, os.WriteFile(transcriptPath, []byte(updated), 0o644),
		"rewrite transcript")
	require.NoError(t, os.Chtimes(transcriptPath, time.Now(), time.Now()),
		"touch transcript")

	engine.SyncPaths([]string{transcriptPath})
	assertCoworkAssistantContent(t, testDB, sessionID, "updated answer")
}

func assertCoworkAssistantContent(
	t *testing.T, database *db.DB, rawSessionID, want string,
) {
	t.Helper()

	msgs, err := database.GetMessages(
		context.Background(), "cowork:"+rawSessionID, 0, 100, true,
	)
	require.NoError(t, err, "GetMessages")
	require.Len(t, msgs, 2, "messages")
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, want, msgs[1].Content)
}
