package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
