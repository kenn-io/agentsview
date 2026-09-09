package sync

import (
	"context"
	"crypto/sha256"
	"encoding"
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

	// Appending after the atomic content/checkpoint commit leaves the stored
	// checkpoint anchored to the original committed prefix. The next sync
	// resumes from that state and advances both content and checkpoint in one
	// transaction.
	tail := testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON(
		"call_race", "done", "2024-01-01T10:00:03Z",
	))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(tail)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	afterAppend, ok, err := database.GetParserCheckpoint("codex:" + uuid)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(len(initial)), afterAppend.Offset)

	beforeBlobs, ok, err := database.GetParserCheckpointBlobs("codex:" + uuid)
	require.NoError(t, err)
	require.True(t, ok)
	info, err := os.Stat(path)
	require.NoError(t, err)
	_, resumedHash, err := codexResumeHash(
		path, afterAppend.Offset, info.Size(), beforeBlobs.HashState,
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

func TestBuildCodexFullParseCheckpointUsesParseSnapshotIdentity(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)

	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("session\n"), 0o600))

	h := sha256.New()
	h.Write([]byte("parsed prefix"))
	hashState, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	require.NoError(t, err)

	pw := pendingWrite{
		sess: parser.ParsedSession{
			ID:    "codex:snapshot",
			Agent: parser.AgentCodex,
			File: parser.FileInfo{
				Path:       path,
				Size:       8,
				Mtime:      111,
				Inode:      222,
				Device:     333,
				ChangeTime: 444,
			},
			MessageCount: 2,
		},
		checkpoint:             []byte("cursor"),
		checkpointHashState:    hashState,
		checkpointAnchorDigest: "anchor",
	}
	cp, blobs, err := engine.buildCodexFullParseCheckpoint(path, pw)
	require.NoError(t, err)
	require.NotNil(t, cp)
	require.NotNil(t, blobs)
	require.Equal(t, uint64(222), cp.FileInode,
		"identity must come from the parse snapshot, not a later stat")
	require.Equal(t, uint64(333), cp.FileDevice)
	require.Equal(t, int64(111), cp.FileMTime)
	require.Equal(t, int64(444), cp.FileChangeTime)
	require.Equal(t, int64(8), cp.Offset)
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
	oldBlobs, ok, err := database.GetParserCheckpointBlobs("codex:" + uuid)
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
	require.NoError(t, database.UpsertParserCheckpoint(*oldCheckpoint, oldBlobs))
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
		context.Background(), []string{root}, false, nil,
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

func TestCodexIncrementalDuplicateCallIDTargetsExactOccurrence(t *testing.T) {
	const (
		uuid   = "019eb791-cf7d-75c1-8439-9ed74c122daa"
		callID = "reused-call"
	)
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexTurnContextJSON(
			"gpt-5.4", "2024-01-01T10:00:01Z",
		),
		testjsonl.CodexMsgJSON(
			"user", "run twice", "2024-01-01T10:00:02Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", callID, nil, "2024-01-01T10:00:03Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			callID, "first result", "2024-01-01T10:00:04Z",
		),
		testjsonl.CodexTokenCountJSON(
			"2024-01-01T10:00:05Z", 100, 10, 80,
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", callID, nil, "2024-01-01T10:00:06Z",
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
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	tail := testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallOutputJSON(
			callID, "second result", "2024-01-01T10:00:07Z",
		),
		testjsonl.CodexTokenCountJSON(
			"2024-01-01T10:00:08Z", 200, 20, 160,
		),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(tail)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)

	sess, err := database.GetSessionFull(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.True(t, sess.LastWriteIncremental)
	msgs, err := database.GetAllMessages(t.Context(), "codex:"+uuid)
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	require.Len(t, msgs[1].ToolCalls, 1)
	require.Len(t, msgs[2].ToolCalls, 1)
	require.Equal(t, "first result", msgs[1].ToolCalls[0].ResultContent)
	require.Equal(t, "second result", msgs[2].ToolCalls[0].ResultContent)
	require.NotEmpty(t, msgs[2].TokenUsage)
}
