package sync

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestCodexCheckpointAdoptionIsLazyForUpgradedArchive pins the upgrade path:
// deleting the machine-local checkpoint from an already stored Codex session
// must leave unchanged archives on the existing stat-digest fast path. The
// checkpoint is adopted on the next real source change, when an authoritative
// parse is already required.
func TestCodexCheckpointAdoptionIsLazyForUpgradedArchive(t *testing.T) {
	require := require.New(t)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	root := writeCodexParityRoot(t, uuid)
	sessionID := "codex:" + uuid

	database, err := db.Open(filepath.Join(t.TempDir(), "bootstrap.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = database.Close() })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	// Initial cold sync commits content and a checkpoint.
	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	cp, ok, err := database.GetParserCheckpoint(sessionID)
	require.NoError(err)
	require.True(ok)
	require.Equal(codexCheckpointVersion, cp.Version)
	before, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(err)
	require.NotEmpty(before)

	// Simulate an archive written before parser checkpoints existed.
	require.NoError(database.DeleteParserCheckpoint(sessionID))

	// An unchanged archive stays on the current-main stat-digest path. It
	// neither rewrites the session nor eagerly migrates optimization state.
	if runtime.GOOS == "linux" {
		rcharBefore := processRchar(t)
		stats := engine.SyncAll(t.Context(), nil)
		require.Zero(stats.Synced)
		require.Less(processRchar(t)-rcharBefore, int64(1<<20))
	} else {
		require.Zero(engine.SyncAll(t.Context(), nil).Synced)
	}
	_, ok, err = database.GetParserCheckpoint(sessionID)
	require.NoError(err)
	require.False(ok, "unchanged archives must not be eagerly migrated")
	afterSkip, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(err)
	require.Equal(before, afterSkip)

	// A real append already requires parsing. The same authoritative write
	// adopts a checkpoint atomically with the updated projection.
	path := filepath.Join(
		root, "2024", "01", "01",
		"rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	appended := testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallOutputJSON(
			"call_a", "late output", "2024-01-01T10:00:13Z",
		),
		testjsonl.CodexTokenCountJSON(
			"2024-01-01T10:00:14Z", 240, 60, 160,
		),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(err)
	_, err = f.WriteString(appended)
	require.NoError(err)
	require.NoError(f.Close())

	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(stats.Failed)
	require.Equal(1, stats.Synced)
	cp, ok, err = database.GetParserCheckpoint(sessionID)
	require.NoError(err)
	require.True(ok, "a real source change must adopt a checkpoint")
	require.Equal(codexCheckpointVersion, cp.Version)

	// Authoritative full-parse parity for the changed source.
	cfg := parser.ProviderConfig{Roots: []string{root}, Machine: "local"}
	provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
	require.True(ok)
	source, found, err := provider.FindSource(
		t.Context(), parser.FindSourceRequest{FullSessionID: sessionID},
	)
	require.NoError(err)
	require.True(found)
	collecting := parser.NewCodexCollectingSink(0)
	_, msgs, _, _, _, _, err := parser.ParseCodexSessionStreaming(
		t.Context(), cfg, source, collecting,
	)
	require.NoError(err)
	stored, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(err)
	for i := range stored {
		stored[i].ID = 0
		stored[i].SessionID = ""
		for j := range stored[i].ToolCalls {
			stored[i].ToolCalls[j].MessageID = 0
			stored[i].ToolCalls[j].SessionID = ""
		}
	}
	expected := toDBMessages(pendingWrite{sess: parser.ParsedSession{Agent: parser.AgentCodex}, msgs: msgs}, nil)
	for i := range expected {
		for j := range expected[i].ToolCalls {
			// Rendering is parser input to the write projection, not a stored field.
			expected[i].ToolCalls[j].Rendering = ""
		}
	}
	require.Equal(expected, stored)
}

func TestCodexCheckpointMissingHonorsMatchingSkipEntry(t *testing.T) {
	require := require.New(t)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c07"
	root := writeCodexParityRoot(t, uuid)
	sessionID := "codex:" + uuid

	database, err := db.Open(filepath.Join(t.TempDir(), "bootstrap-skip.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = database.Close() })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(database.DeleteParserCheckpoint(sessionID))

	path := filepath.Join(
		root, "2024", "01", "01",
		"rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	cfg := parser.ProviderConfig{Roots: []string{root}, Machine: "local"}
	provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
	require.True(ok)
	source, found, err := provider.FindSource(
		t.Context(), parser.FindSourceRequest{FullSessionID: sessionID},
	)
	require.NoError(err)
	require.True(found)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	file := parser.DiscoveredFile{
		Path: path, Agent: parser.AgentCodex,
		ProviderSource: &source, ProviderProcess: true,
	}
	cacheKey := providerProcessCacheKey(
		file, source, fingerprint, provider.Capabilities().Sync,
	)
	engine.InjectSkipCache(map[string]int64{cacheKey: fingerprint.MTimeNS})

	require.Zero(engine.SyncAll(t.Context(), nil).Synced,
		"missing optimization state must not defeat a valid skip entry")
	_, ok, err = database.GetParserCheckpoint(sessionID)
	require.NoError(err)
	require.False(ok)
}

func TestCodexCheckpointDeviceChangeVerifiesContentBeforeReparsing(t *testing.T) {
	for _, tt := range []struct {
		name        string
		changed     bool
		missingHash bool
	}{
		{name: "unchanged"},
		{name: "changed", changed: true},
		{name: "changed without stored hash", changed: true, missingHash: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)

			const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c11"
			root := writeCodexParityRoot(t, uuid)
			path := filepath.Join(root, "2024", "01", "01",
				"rollout-2024-01-01T10-00-00-"+uuid+".jsonl")
			database := openTestDB(t)
			cfg := EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
				Machine:   "local",
			}
			engine := NewEngine(database, cfg)
			require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
			engine.Close()
			cp, ok, err := database.GetParserCheckpoint("codex:" + uuid)
			require.NoError(err)
			require.True(ok)
			blobs, ok, err := database.GetParserCheckpointBlobs(cp.SessionID)
			require.NoError(err)
			require.True(ok)
			// Device numbers can change across boots. The checkpoint must no
			// longer authorize an append, but the transcript may be unchanged.
			cp.FileDevice++
			require.NoError(database.UpsertParserCheckpoint(*cp, blobs))
			if tt.missingHash {
				session, err := database.GetSessionFull(t.Context(), cp.SessionID)
				require.NoError(err)
				require.NotNil(session)
				session.FileHash = nil
				require.NoError(database.UpsertSession(*session))
			}
			require.NoError(database.DeleteProviderStatHash(t.Context(), parser.AgentCodex, path))
			wantMessage, wantSynced := "run the suite", 0
			if tt.changed {
				info, err := os.Stat(path)
				require.NoError(err)
				content, err := os.ReadFile(path)
				require.NoError(err)
				require.NoError(os.WriteFile(path,
					[]byte(strings.Replace(string(content), "run the suite", "fix the suite", 1)), 0o644))
				require.NoError(os.Chtimes(path, info.ModTime(), info.ModTime()))
				wantMessage, wantSynced = "fix the suite", 1
			}
			fresh := NewEngine(database, cfg)
			t.Cleanup(fresh.Close)
			stats := fresh.SyncAll(t.Context(), nil)
			require.Zero(stats.Failed)
			messages, err := database.GetAllMessages(t.Context(), cp.SessionID)
			require.NoError(err)
			require.NotEmpty(messages)
			require.Equal(wantMessage, messages[0].Content)
			require.Equal(wantSynced, stats.Synced)
			require.Zero(fresh.SyncAll(t.Context(), nil).Synced)
		})
	}
}

func TestCodexCheckpointInvalidIsDiscardedOnNextSourceChange(t *testing.T) {
	require := require.New(t)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c08"
	root := writeCodexParityRoot(t, uuid)
	sessionID := "codex:" + uuid
	path := filepath.Join(
		root, "2024", "01", "01",
		"rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)

	database, err := db.Open(filepath.Join(t.TempDir(), "invalid-cp.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = database.Close() })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	cp, ok, err := database.GetParserCheckpoint(sessionID)
	require.NoError(err)
	require.True(ok)
	blobs, hasBlobs, err := database.GetParserCheckpointBlobs(sessionID)
	require.NoError(err)
	require.True(hasBlobs)
	corrupted := *cp
	corrupted.Hash = "corrupted-proof"
	require.NoError(database.UpsertParserCheckpoint(corrupted, blobs))

	// Stat-digest freshness may skip an unchanged source without consulting
	// disposable checkpoint state. A real source change reaches checkpoint
	// validation, rejects the corrupted proof, and performs a full repair.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(err)
	_, err = f.WriteString(testjsonl.CodexMsgJSON(
		"user", "changed", "2024-01-01T10:00:13Z",
	))
	require.NoError(err)
	require.NoError(f.Close())

	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	_, ok, err = database.GetParserCheckpoint(sessionID)
	require.NoError(err)
	require.False(ok,
		"an unsafe full-parse boundary must discard the invalid checkpoint")
	stored, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(err)
	require.Equal("changed", stored[len(stored)-1].Content)
}

func TestCodexCheckpointAuditDeepVerifiesDespiteWarmGates(t *testing.T) {
	require := require.New(t)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c09"
	root := writeCodexParityRoot(t, uuid)

	database, err := db.Open(filepath.Join(t.TempDir(), "audit-cp.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = database.Close() })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	require.Zero(engine.SyncAll(t.Context(), nil).Synced,
		"second pass must be a warm no-op")

	engine.SetCheckpointAudit(true)
	t.Cleanup(func() { engine.SetCheckpointAudit(false) })
	require.Zero(engine.SyncAll(t.Context(), nil).Synced,
		"an unchanged source still skips after the audit's full hash check")
}

func TestCodexCheckpointAuditRepairsPrefixRewriteBeforeAppend(t *testing.T) {
	require := require.New(t)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c10"
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	// Keep the rewrite well before the trailing 128KiB anchor so a
	// tail-append alone cannot detect it.
	body := "OLD-MARKER" + strings.Repeat("p", 200*1024)
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/tmp", "user", "2024-01-01T10:00:00Z",
		),
		testjsonl.CodexTurnContextJSON(
			"gpt-5.4", "2024-01-01T10:00:01Z",
		),
		testjsonl.CodexMsgJSON(
			"user", body, "2024-01-01T10:00:02Z",
		),
	)
	require.NoError(os.WriteFile(path, []byte(initial), 0o644))

	database, err := db.Open(filepath.Join(t.TempDir(), "audit-rewrite.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = database.Close() })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	sessionID := "codex:" + uuid

	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(err)
	require.NotEmpty(before)
	require.Contains(before[0].Content, "OLD-MARKER")

	info, err := os.Stat(path)
	require.NoError(err)
	rewritten := strings.Replace(initial, "OLD-MARKER", "NEW-MARKER", 1)
	require.NotEqual(initial, rewritten)
	require.NoError(os.WriteFile(path, []byte(rewritten), 0o644))
	require.NoError(os.Chtimes(path, info.ModTime(), info.ModTime()))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(err)
	_, err = f.WriteString(testjsonl.CodexTokenCountJSON(
		"2024-01-01T10:00:03Z", 100, 50, 80,
	) + "\n")
	require.NoError(err)
	require.NoError(f.Close())

	engine.SetCheckpointAudit(true)
	t.Cleanup(func() { engine.SetCheckpointAudit(false) })
	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced,
		"the audit must repair a rewritten prefix instead of tail-applying")
	after, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(err)
	require.NotEmpty(after)
	require.Contains(after[0].Content, "NEW-MARKER",
		"the repaired prefix must replace the stale stored content")
}

func TestCodexIncrementalResumeHashFailureRetainsCheckpoint(t *testing.T) {
	require := require.New(t)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122c13"
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/tmp", "user", "2024-01-01T10:00:00Z",
		),
		testjsonl.CodexTurnContextJSON(
			"gpt-5.4", "2024-01-01T10:00:01Z",
		),
		testjsonl.CodexMsgJSON(
			"user", "run the command", "2024-01-01T10:00:02Z",
		),
		testjsonl.CodexMsgJSON(
			"assistant", "running", "2024-01-01T10:00:03Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_x", nil, "2024-01-01T10:00:04Z",
		),
	)
	require.NoError(os.WriteFile(path, []byte(initial), 0o644))
	sessionID := "codex:" + uuid

	database, err := db.Open(filepath.Join(t.TempDir(), "resume-fail.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = database.Close() })
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	before, ok, err := database.GetParserCheckpoint(sessionID)
	require.NoError(err)
	require.True(ok)
	oldOffset := before.Offset

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallOutputJSON(
			"call_x", "late output", "2024-01-01T10:00:13Z",
		),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(err)
	_, err = f.WriteString(appended)
	require.NoError(err)
	require.NoError(f.Close())

	orig := codexResumeHashFn
	codexResumeHashFn = func(
		string, int64, int64, []byte,
	) ([]byte, string, error) {
		return nil, "", errors.New("injected resume failure")
	}
	t.Cleanup(func() { codexResumeHashFn = orig })

	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	cp, ok, err := database.GetParserCheckpoint(sessionID)
	require.NoError(err)
	require.True(ok)
	require.Equal(oldOffset, cp.Offset,
		"a failed reconstruction must not advance the checkpoint")

	// The failed hash-state reconstruction does not invalidate the content
	// transaction: the stored projection carries the authoritative full-file
	// hash, while the disposable checkpoint remains at its previous offset.
	storedHash, hasHash := database.GetFileHashByAgentPath(path, "codex")
	require.True(hasHash)
	actualHash, err := ComputeFileHash(path)
	require.NoError(err)
	require.Equal(actualHash, storedHash)

	// Lazy checkpoint adoption leaves an unchanged source on the stat-digest
	// fast path. Restoring the hasher alone must not rewrite the session or
	// advance the stale checkpoint.
	codexResumeHashFn = orig
	unchanged := engine.SyncAll(t.Context(), nil)
	require.Zero(unchanged.Failed)
	require.Zero(unchanged.Synced)
	skipped, ok, err := database.GetParserCheckpoint(sessionID)
	require.NoError(err)
	require.True(ok)
	require.Equal(oldOffset, skipped.Offset)

	// The next real source change reaches checkpoint validation and repairs
	// the stale optimization state in the same authoritative write.
	repairBoundary := testjsonl.JoinJSONL(
		testjsonl.CodexTokenCountJSON(
			"2024-01-01T10:00:14Z", 240, 60, 160,
		),
	)
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(err)
	_, err = f.WriteString(repairBoundary)
	require.NoError(err)
	require.NoError(f.Close())

	repaired := engine.SyncAll(t.Context(), nil)
	require.Zero(repaired.Failed)
	require.Equal(1, repaired.Synced)
	fixed, ok, err := database.GetParserCheckpoint(sessionID)
	require.NoError(err)
	require.True(ok)
	require.Greater(fixed.Offset, oldOffset)
	require.Equal(int64(len(initial)+len(appended)+len(repairBoundary)),
		fixed.Offset,
	)
	storedHash, hasHash = database.GetFileHashByAgentPath(path, "codex")
	require.True(hasHash)
	actualHash, err = ComputeFileHash(path)
	require.NoError(err)
	require.Equal(actualHash, storedHash)
	require.Equal(storedHash, fixed.Hash)
}

// processRchar returns the process's cumulative read bytes (Linux).
func processRchar(t *testing.T) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/io")
	require.NoError(t, err)
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "rchar: "); ok {
			v, err := strconv.ParseInt(
				strings.TrimSpace(rest), 10, 64,
			)
			require.NoError(t, err)
			return v
		}
	}
	t.Fatal("no rchar in /proc/self/io")
	return 0
}
