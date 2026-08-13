package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Regression tests for the review findings on checkpoint consistency:
//  1. a checkpoint persisted for a committed prefix must hash exactly that
//     prefix, even when the live file kept growing (append-during-checkpoint);
//  2. TraeX must load the checkpoint it persisted (no codex: prefix hardcode);
//  3. a stale checkpoint (crash after full replacement commit, before its
//     checkpoint upsert) must never seed a resume against a newer DB prefix;
//  4. a cold restart (new engine, fresh cursor cache) must resume from the
//     persisted checkpoint with full parity;
//  5. the checkpoint-bypassing audit must repair same-stat in-place rewrites.

func TestCodexCheckpointHashStateBoundedToCommittedOffset(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c99"
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl")
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "run command", "2024-01-01T10:00:01Z"),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_race", nil, "2024-01-01T10:00:02Z",
		),
	)
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)

	before, ok, err := database.GetParserCheckpoint("codex:" + uuid)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(len(initial)), before.Offset)

	// Model a rollout append after the full parse/DB commit but before
	// persistFullParseCheckpoint hashes the live source.
	tail := testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON(
		"call_race", "done", "2024-01-01T10:00:03Z",
	))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(tail)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	engine.persistFullParseCheckpoint(context.Background(), pendingWrite{
		sess: parser.ParsedSession{
			ID:    "codex:" + uuid,
			Agent: parser.AgentCodex,
			File: parser.FileInfo{
				Path: path,
				Hash: before.Hash,
			},
		},
		checkpoint: before.Cursor,
	})

	after, ok, err := database.GetParserCheckpoint("codex:" + uuid)
	require.NoError(t, err)
	require.True(t, ok)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(len(initial)), after.Offset,
		"DB still commits only the original prefix")

	_, resumedHash, err := codexResumeHash(
		path, after.Offset, info.Size(), after.HashState,
	)
	require.NoError(t, err)
	actualHash, err := ComputeFileHash(path)
	require.NoError(t, err)
	require.Equal(t, actualHash, resumedHash,
		"resuming the persisted state must reproduce the real source hash")

	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	stored, err := database.GetSessionFull(
		context.Background(), "codex:"+uuid,
	)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.NotNil(t, stored.FileHash)
	require.Equal(t, actualHash, *stored.FileHash,
		"a normal append resume must persist the real source hash")
}

func TestCodexCheckpointTraeXLoadsPersistedCheckpoint(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122d99"
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl")
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "hello", "2024-01-01T10:00:01Z"),
	)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentTraeX: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	_, ok, err := database.GetParserCheckpoint("traex:" + uuid)
	require.NoError(t, err)
	require.True(t, ok, "full TraeX parse persists a traex checkpoint")

	provider, ok := parser.NewProvider(parser.AgentTraeX, parser.ProviderConfig{
		Roots: []string{root}, Machine: "local",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	result, err := engine.codexCheckpointFingerprint(
		context.Background(), sources[0], parser.DiscoveredFile{
			Agent:          parser.AgentTraeX,
			Path:           path,
			ProviderSource: &sources[0],
		},
	)
	require.NoError(t, err)
	require.Equal(t, codexCheckpointUnchanged, result.decision,
		"a cold TraeX worker should reuse the checkpoint it persisted")
}

func TestCodexCheckpointStaleCannotResumeFromNewerDBOffset(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122e99"
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl")
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "hello", "2024-01-01T10:00:01Z"),
	)
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	oldCheckpoint, ok, err := database.GetParserCheckpoint("codex:" + uuid)
	require.NoError(t, err)
	require.True(t, ok)

	appendLine := func(line string) {
		t.Helper()
		f, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		require.NoError(t, openErr)
		_, writeErr := f.WriteString(testjsonl.JoinJSONL(line))
		require.NoError(t, writeErr)
		require.NoError(t, f.Close())
	}
	appendLine(testjsonl.CodexTurnContextJSON(
		"gpt-5.5", "2024-01-01T10:00:02Z",
	))
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	engine.Close()
	engine = NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	// Recreate the state left by a crash after a full replacement commits its
	// newer file_size/next_ordinal but before its out-of-transaction checkpoint
	// upsert: the DB prefix is newer than the surviving checkpoint seed.
	require.NoError(t, database.UpsertParserCheckpoint(*oldCheckpoint))
	appendLine(testjsonl.CodexMsgJSON(
		"assistant", "new reply", "2024-01-01T10:00:03Z",
	))
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)

	messages, err := database.GetAllMessages(
		context.Background(), "codex:"+uuid,
	)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Equal(t, "gpt-5.5", messages[1].Model,
		"resume seed must describe the same prefix as the DB byte offset")
}

func TestCodexCheckpointColdRestartResumeParity(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122f99"
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl")
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "hello", "2024-01-01T10:00:01Z"),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_restart", nil, "2024-01-01T10:00:02Z",
		),
	)
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	engine.Close()

	// Cold restart: a fresh engine has an empty cursor cache and must resume
	// entirely from the persisted checkpoint.
	engine = NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	appended := testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON(
		"call_restart", "done", "2024-01-01T10:00:03Z",
	))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	sess, err := database.GetSessionFull(context.Background(), "codex:"+uuid)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.True(t, sess.LastWriteIncremental,
		"a cold restart must resume incrementally from the checkpoint")
	msgs, err := database.GetAllMessages(context.Background(), "codex:"+uuid)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	require.Equal(t, "done", msgs[1].ToolCalls[0].ResultContent)
}

func TestCodexCheckpointAuditRepairsSameStatRewrite(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122a99"
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl")
	original := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "alpha request", "2024-01-01T10:00:01Z"),
	)
	rewritten := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "bravo request", "2024-01-01T10:00:01Z"),
	)
	require.Len(t, rewritten, len(original))
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(context.Background(), nil).Synced)
	before, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(rewritten), 0o644))
	require.NoError(t, os.Chtimes(path, before.ModTime(), before.ModTime()))

	engine.SetCheckpointAudit(true)
	stats, tombstoned, err := engine.ReconcileWatchRootsWithStats(
		context.Background(), []string{root}, false,
	)
	require.NoError(t, err)
	require.Zero(t, tombstoned)
	require.Equal(t, 1, stats.Synced,
		"the audit must detect and repair the same-stat rewrite")
	engine.SetCheckpointAudit(false)

	msgs, err := database.GetAllMessages(context.Background(), "codex:"+uuid)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "bravo request", msgs[0].Content)
}
