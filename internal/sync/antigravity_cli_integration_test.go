package sync_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/sync"
)

func TestSyncEngineAntigravityCLI_HappyPath(t *testing.T) {
	env := setupTestEnv(t)
	uuid := "33333333-4444-5555-6666-777777777777"

	// Create subdirectories
	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	// Write history.jsonl to map the project
	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/my-cli-project", "timestamp": 1716244800000, "display": "Initial Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	// Write .pb file
	pbPath := filepath.Join(convDir, uuid+".pb")
	require.NoError(t, os.WriteFile(pbPath, []byte("dummy-pb"), 0o644))

	// Write .trajectory.json
	trajectoryJSON := `{
		"trajectoryId": "` + uuid + `",
		"steps": [
			{
				"type": "CORTEX_STEP_TYPE_USER_INPUT",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:40:00Z"
				},
				"userInput": {
					"userResponse": "Check workspace status"
				}
			},
			{
				"type": "CORTEX_STEP_TYPE_PLANNER_RESPONSE",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:41:00Z"
				},
				"plannerResponse": {
					"thinking": "I should list files",
					"response": "listing files now",
					"toolCalls": [
						{
							"id": "tc-123",
							"name": "run_command",
							"argumentsJson": "{\"CommandLine\":\"ls\"}"
						}
					]
				}
			},
			{
				"type": "CORTEX_STEP_TYPE_RUN_COMMAND",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:42:00Z",
					"executionId": "tc-123"
				},
				"runCommand": {
					"commandLine": "ls",
					"combinedOutput": "\"fileA.go\""
				}
			}
		]
	}`
	trajPath := filepath.Join(convDir, uuid+".trajectory.json")
	require.NoError(t, os.WriteFile(trajPath, []byte(trajectoryJSON), 0o644))

	// First Sync: should ingest 1 session
	stats := runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assert.Equal(t, 1, stats.Synced)

	// Verify database ingestion
	assertSessionProject(t, env.db, "antigravity-cli:"+uuid, "/home/user/my-cli-project")
	// Expected messages:
	// 1. User: "Check workspace status"
	// 2. Assistant: "listing files now" (with tool calls and thoughts)
	// (Note: synthetic empty-content User message with tool results is paired and filtered out by the engine)
	assertSessionMessageCount(t, env.db, "antigravity-cli:"+uuid, 2)

	msgs := fetchMessages(t, env.db, "antigravity-cli:"+uuid)
	require.Len(t, msgs, 2)

	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "Check workspace status", msgs[0].Content)

	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "listing files now", msgs[1].Content)
}

func TestSyncEngineAntigravityCLI_ReSyncOnSidecarUpdate(t *testing.T) {
	env := setupTestEnv(t)
	uuid := "44444444-5555-6666-7777-888888888888"

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	// Write history
	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/workspace-abc", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	// Write .pb file
	pbPath := filepath.Join(convDir, uuid+".pb")
	require.NoError(t, os.WriteFile(pbPath, []byte("dummy-pb"), 0o644))

	// Sync 1: pb exists, sidecar does not. Should sync fallback history.
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	assertSessionMessageCount(t, env.db, "antigravity-cli:"+uuid, 1)
	msgs := fetchMessages(t, env.db, "antigravity-cli:"+uuid)
	require.Len(t, msgs, 1)
	assert.Equal(t, "History Prompt", msgs[0].Content)

	// Sync 2: Run again immediately without any changes -> should skip
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        0,
		Skipped:       1,
	})

	// Make sure we sleep briefly to ensure mod time changes if filesystem is low-res
	time.Sleep(10 * time.Millisecond)

	// Write sidecar .trajectory.json
	trajectoryJSON := `{
		"trajectoryId": "` + uuid + `",
		"steps": [
			{
				"type": "CORTEX_STEP_TYPE_USER_INPUT",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:45:00Z"
				},
				"userInput": {
					"userResponse": "New Prompt from Trajectory"
				}
			}
		]
	}`
	trajPath := filepath.Join(convDir, uuid+".trajectory.json")
	require.NoError(t, os.WriteFile(trajPath, []byte(trajectoryJSON), 0o644))

	// Sync 3: sidecar added. Effective mtime and size changed -> should re-sync!
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	assertSessionMessageCount(t, env.db, "antigravity-cli:"+uuid, 1)
	msgs = fetchMessages(t, env.db, "antigravity-cli:"+uuid)
	require.Len(t, msgs, 1)
	assert.Equal(t, "New Prompt from Trajectory", msgs[0].Content)
}

func TestSyncEngineAntigravityCLI_SyncAllSinceReSyncsSidecarUpdate(t *testing.T) {
	env := setupTestEnv(t)
	uuid := "77777777-8888-9999-0000-111111111111"

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/workspace-since", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	pbPath := filepath.Join(convDir, uuid+".pb")
	require.NoError(t, os.WriteFile(pbPath, []byte("dummy-pb"), 0o644))
	oldTime := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(pbPath, oldTime, oldTime))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assertSessionMessageCount(t, env.db, "antigravity-cli:"+uuid, 1)

	cutoff := time.Now().Add(-1 * time.Hour)
	time.Sleep(10 * time.Millisecond)
	trajectoryJSON := `{
		"trajectoryId": "` + uuid + `",
		"steps": [
			{
				"type": "CORTEX_STEP_TYPE_USER_INPUT",
				"status": "STATUS_COMPLETED",
				"metadata": {
					"createdAt": "2026-05-20T22:45:00Z"
				},
				"userInput": {
					"userResponse": "Prompt from SyncAllSince Trajectory"
				}
			}
		]
	}`
	trajPath := filepath.Join(convDir, uuid+".trajectory.json")
	require.NoError(t, os.WriteFile(trajPath, []byte(trajectoryJSON), 0o644))

	stats := env.engine.SyncAllSince(context.Background(), cutoff, nil)
	require.Equal(t, 1, stats.Synced, "synced = %d, want 1", stats.Synced)

	msgs := fetchMessages(t, env.db, "antigravity-cli:"+uuid)
	require.Len(t, msgs, 1)
	assert.Equal(t, "Prompt from SyncAllSince Trajectory", msgs[0].Content)
}

func TestSyncEngineAntigravityCLI_SyncAllSinceReSyncsDBWalUpdate(t *testing.T) {
	env := setupTestEnv(t)
	uuid := "22222222-3333-4444-5555-777777777777"
	sessionID := "antigravity-cli:" + uuid

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/workspace-db-wal", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	dbPath := filepath.Join(convDir, uuid+".db")
	createAntigravityCLIDisplayStepDB(t, dbPath, "Initial database prompt text")

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	baseInfo, err := os.Stat(dbPath)
	require.NoError(t, err)
	baseMtime := baseInfo.ModTime()

	time.Sleep(10 * time.Millisecond)

	writer := openAntigravityCLITestWALDB(t, dbPath)
	defer writer.Close()
	insertAntigravityCLIStep(t, writer, 1, 17, "Assistant response from WAL text")

	walInfo, err := os.Stat(dbPath + "-wal")
	require.NoError(t, err, "expected WAL sidecar after uncheckpointed write")
	require.Greater(t, walInfo.ModTime().UnixNano(), baseMtime.UnixNano())

	require.NoError(t, os.Chtimes(dbPath, baseMtime, baseMtime))
	baseAfter, err := os.Stat(dbPath)
	require.NoError(t, err)
	require.Equal(t, baseInfo.Size(), baseAfter.Size(), "base DB size changed")
	require.Equal(t, baseMtime.UnixNano(), baseAfter.ModTime().UnixNano(),
		"base DB mtime should not reveal WAL-only update")

	stats := env.engine.SyncAllSince(context.Background(), baseMtime.Add(time.Nanosecond), nil)
	assert.Equal(t, 1, stats.TotalSessions)
	assert.Equal(t, 1, stats.Synced)
	assert.Equal(t, 0, stats.Skipped)

	assertSessionMessageCount(t, env.db, sessionID, 2)
	msgs := fetchMessages(t, env.db, sessionID)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Assistant response from WAL text", msgs[0].Content)
	assert.Equal(t, "History Prompt", msgs[1].Content)
}

func TestSyncEngineAntigravityCLI_MalformedSidecarFallback(t *testing.T) {
	env := setupTestEnv(t)
	uuid := "55555555-6666-7777-8888-999999999999"

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/workspace-xyz", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	pbPath := filepath.Join(convDir, uuid+".pb")
	require.NoError(t, os.WriteFile(pbPath, []byte("dummy-pb"), 0o644))

	// Malformed sidecar
	trajPath := filepath.Join(convDir, uuid+".trajectory.json")
	require.NoError(t, os.WriteFile(trajPath, []byte("invalid-json{"), 0o644))

	// Ingest: Should fall back to reading history.jsonl safely
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	assertSessionMessageCount(t, env.db, "antigravity-cli:"+uuid, 1)
	msgs := fetchMessages(t, env.db, "antigravity-cli:"+uuid)
	require.Len(t, msgs, 1)
	assert.Equal(t, "History Prompt", msgs[0].Content)
}

func TestSyncEngineAntigravityCLI_DBDecodeFallbackRetries(t *testing.T) {
	env := setupTestEnv(t)
	uuid := "77777777-8888-9999-aaaa-bbbbbbbbbbbb"
	sessionID := "antigravity-cli:" + uuid

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/workspace-db", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	dbPath := filepath.Join(convDir, uuid+".db")
	require.NoError(t, os.WriteFile(dbPath, []byte("not a sqlite database"), 0o644))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	assertSessionMessageCount(t, env.db, sessionID, 1)
	msgs := fetchMessages(t, env.db, sessionID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "History Prompt", msgs[0].Content)
	assert.Less(t, env.db.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"degraded DB fallback should stay stale so unchanged syncs retry")

	require.NoError(t, env.db.SetSessionDataVersion(sessionID, db.CurrentDataVersion()))
	require.NoError(t, env.db.ResetAllMtimes(), "force fallback rewrite")
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assert.Less(t, env.db.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"DB decode fallback should demote previously current rows")

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assert.Less(t, env.db.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"unchanged DB decode fallback should keep retrying")
}

func TestSyncEngineAntigravityCLI_NeedsRetryReplacesCurrentMessages(t *testing.T) {
	env := setupTestEnv(t)
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	sessionID := "antigravity-cli:" + uuid

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/workspace-db-retry", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	dbPath := filepath.Join(convDir, uuid+".db")
	createAntigravityCLIDisplayStepDB(t, dbPath, "Initial database prompt text")
	conn := openAntigravityCLITestDB(t, dbPath)
	insertAntigravityCLIStep(t, conn, 1, 17, "Initial assistant response text")
	require.NoError(t, conn.Close())

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assertSessionMessageCount(t, env.db, sessionID, 2)
	assert.Equal(t, db.CurrentDataVersion(), env.db.GetSessionDataVersion(sessionID))

	require.NoError(t, os.WriteFile(dbPath, []byte("not a sqlite database"), 0o644))

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	assertSessionMessageCount(t, env.db, sessionID, 1)
	msgs := fetchMessages(t, env.db, sessionID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "History Prompt", msgs[0].Content)
	assert.Less(t, env.db.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"retry fallback should be written before the row is demoted")
}

func TestSyncEngineAntigravityCLI_DBUndisplayableStepsFallbackRetries(t *testing.T) {
	env := setupTestEnv(t)
	uuid := "88888888-9999-aaaa-bbbb-cccccccccccc"
	sessionID := "antigravity-cli:" + uuid

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/workspace-db-filtered", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	dbPath := filepath.Join(convDir, uuid+".db")
	createAntigravityCLIUndisplayableStepDB(t, dbPath)

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})

	assertSessionMessageCount(t, env.db, sessionID, 1)
	msgs := fetchMessages(t, env.db, sessionID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "History Prompt", msgs[0].Content)
	assert.Less(t, env.db.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"DB fallback after dropping all raw steps should stay stale so unchanged syncs retry")

	require.NoError(t, env.db.SetSessionDataVersion(sessionID, db.CurrentDataVersion()))
	require.NoError(t, env.db.ResetAllMtimes(), "force fallback rewrite")
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assert.Less(t, env.db.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"DB fallback after dropping all raw steps should demote previously current rows")

	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 1,
		Synced:        1,
		Skipped:       0,
	})
	assert.Less(t, env.db.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"unchanged DB fallback after dropping all raw steps should keep retrying")
}

func TestSyncSingleSessionAntigravityCLI_DBDecodeFallbackRetries(t *testing.T) {
	env := setupTestEnv(t)
	uuid := "99999999-aaaa-bbbb-cccc-dddddddddddd"
	sessionID := "antigravity-cli:" + uuid

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	historyLine := `{"conversationId": "` + uuid + `", "workspace": "/home/user/workspace-db-single", "timestamp": 1716244800000, "display": "History Prompt"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.antigravityCLIDir, "history.jsonl"), []byte(historyLine), 0o644))

	dbPath := filepath.Join(convDir, uuid+".db")
	require.NoError(t, os.WriteFile(dbPath, []byte("not a sqlite database"), 0o644))

	// An explicit single-session sync (the file-watcher path) of a .db
	// whose decode fails must store the fallback at a stale data_version
	// so a later sync retries the high-resolution source.
	require.NoError(t, env.engine.SyncSingleSession(sessionID))

	assertSessionMessageCount(t, env.db, sessionID, 1)
	msgs := fetchMessages(t, env.db, sessionID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "History Prompt", msgs[0].Content)
	assert.Less(t, env.db.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"single-session DB fallback should stay stale so later syncs retry")

	// A previously current row must be demoted when an explicit re-sync
	// hits the same decode failure, otherwise the high-resolution DB is
	// never retried once it has been stamped current.
	require.NoError(t, env.db.SetSessionDataVersion(sessionID, db.CurrentDataVersion()))
	require.NoError(t, env.db.ResetAllMtimes(), "force fallback rewrite")
	require.NoError(t, env.engine.SyncSingleSession(sessionID))
	assert.Less(t, env.db.GetSessionDataVersion(sessionID), db.CurrentDataVersion(),
		"single-session DB decode fallback should demote previously current rows")
}

func TestSyncEngineAntigravityCLI_MissingPbOrphanSidecar(t *testing.T) {
	env := setupTestEnv(t)
	uuid := "66666666-7777-8888-9999-000000000000"

	convDir := filepath.Join(env.antigravityCLIDir, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))

	// Write ONLY .trajectory.json, no .pb
	trajPath := filepath.Join(convDir, uuid+".trajectory.json")
	require.NoError(t, os.WriteFile(trajPath, []byte("{}"), 0o644))

	// Sync: should discover no sessions
	runSyncAndAssert(t, env.engine, sync.SyncStats{
		TotalSessions: 0,
		Synced:        0,
		Skipped:       0,
	})
}

func createAntigravityCLIUndisplayableStepDB(t *testing.T, path string) {
	t.Helper()
	conn := openAntigravityCLITestDB(t, path)
	defer conn.Close()

	createAntigravityCLITestStepsTable(t, conn)
	insertAntigravityCLIStep(t, conn, 0, 14, "MODEL_PLACEHOLDER_0")
}

func createAntigravityCLIDisplayStepDB(t *testing.T, path, prompt string) {
	t.Helper()
	conn := openAntigravityCLITestDB(t, path)
	defer conn.Close()

	createAntigravityCLITestStepsTable(t, conn)
	insertAntigravityCLIStep(t, conn, 0, 14, prompt)
}

func openAntigravityCLITestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open antigravity cli test db")

	return conn
}

func openAntigravityCLITestWALDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn := openAntigravityCLITestDB(t, path)

	_, err := conn.Exec(`PRAGMA journal_mode=WAL`)
	require.NoError(t, err, "enable WAL mode")
	_, err = conn.Exec(`PRAGMA wal_autocheckpoint=0`)
	require.NoError(t, err, "disable WAL autocheckpoint")

	return conn
}

func createAntigravityCLITestStepsTable(t *testing.T, conn *sql.DB) {
	t.Helper()
	_, err := conn.Exec(`CREATE TABLE steps (
		idx integer,
		step_type integer NOT NULL DEFAULT 0,
		step_payload blob,
		PRIMARY KEY (idx))`)
	require.NoError(t, err, "create steps table")
}

func insertAntigravityCLIStep(
	t *testing.T, conn *sql.DB, idx, stepType int, content string,
) {
	t.Helper()
	payload := antigravityCLIStringPayload(content)
	_, err := conn.Exec(
		`INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`,
		idx, stepType, payload,
	)
	require.NoError(t, err, "insert antigravity cli step")
}

func antigravityCLIStringPayload(s string) []byte {
	content := []byte(s)
	return append([]byte{0x8a, 0x01, byte(len(content))}, content...)
}
