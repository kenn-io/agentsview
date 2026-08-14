package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type digestFingerprintCountingProvider struct {
	parser.Provider
	calls atomic.Int64
}

func (p *digestFingerprintCountingProvider) Fingerprint(
	ctx context.Context, source parser.SourceRef,
) (parser.SourceFingerprint, error) {
	p.calls.Add(1)
	return p.Provider.Fingerprint(ctx, source)
}

func (p *digestFingerprintCountingProvider) ComputeMultiFileStatHash(
	path string,
) uint64 {
	hasher, ok := p.Provider.(parser.MultiFileStatHasher)
	if !ok {
		return 0
	}
	return hasher.ComputeMultiFileStatHash(path)
}

type digestFingerprintCountingFactory struct {
	provider *digestFingerprintCountingProvider
}

func (f digestFingerprintCountingFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f digestFingerprintCountingFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f digestFingerprintCountingFactory) NewProvider(
	parser.ProviderConfig,
) parser.Provider {
	return f.provider
}

// TestSyncClaudeForkWriteFailureRetriesWholeSource proves that freshness is a
// source-level outcome for Claude DAG transcripts. One committed branch cannot
// make the shared transcript fresh while another branch still needs a write.
func TestSyncClaudeForkWriteFailureRetriesWholeSource(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	projectDir := filepath.Join(root, "project-a")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "forked.jsonl")
	builder := newClaudeDAGBuilder(false)
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o644))

	initialEngine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "local",
	})
	t.Cleanup(initialEngine.Close)
	initial := initialEngine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, initial.Synced)
	require.Zero(t, initial.Failed)
	require.Equal(t, db.CurrentDataVersion(),
		database.GetSessionDataVersion("forked"))

	addClaudeDAGFork(builder)
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o644))

	raw, err := sql.Open("sqlite3", database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.Exec(`
		CREATE TRIGGER fail_claude_fork_insert
		BEFORE INSERT ON sessions
		WHEN NEW.id = 'forked-i'
		BEGIN
			SELECT RAISE(FAIL, 'injected Claude fork write failure');
		END;
		CREATE TRIGGER fail_claude_main_demotion
		BEFORE UPDATE OF data_version ON sessions
		WHEN NEW.id = 'forked'
		 AND OLD.data_version > NEW.data_version
		BEGIN
			SELECT RAISE(FAIL, 'injected Claude main demotion failure');
		END;
	`)
	require.NoError(t, err)

	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	failedDemotion := engine.SyncAll(context.Background(), nil)
	require.Zero(t, failedDemotion.Synced)
	require.Equal(t, 1, failedDemotion.Failed)
	require.Equal(t, db.CurrentDataVersion(),
		database.GetSessionDataVersion("forked"),
		"a rejected pre-write demotion must abort before changing the row")
	_, hasDigest, err := database.GetProviderStatHash(
		t.Context(), parser.AgentClaude, path,
	)
	require.NoError(t, err)
	assert.False(t, hasDigest,
		"a rejected source demotion must revoke the old digest")

	_, err = raw.Exec(`DROP TRIGGER fail_claude_main_demotion`)
	require.NoError(t, err)
	partialEngine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "local",
	})
	t.Cleanup(partialEngine.Close)
	failed := partialEngine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, failed.Synced)
	require.Equal(t, 1, failed.Failed)
	main, err := database.GetSession(t.Context(), "forked")
	require.NoError(t, err)
	require.NotNil(t, main, "the first DAG branch must commit before the failure")
	assert.Less(t, main.DataVersion, db.CurrentDataVersion(),
		"the committed main branch must remain retryable")
	fork, err := database.GetSession(t.Context(), "forked-i")
	require.NoError(t, err)
	assert.Nil(t, fork, "the injected fork write must leave the branch absent")

	_, hasDigest, err = database.GetProviderStatHash(
		t.Context(), parser.AgentClaude, path,
	)
	require.NoError(t, err)
	assert.False(t, hasDigest,
		"a partial DAG write must not mark the shared source fresh")

	_, err = raw.Exec(`DROP TRIGGER fail_claude_fork_insert`)
	require.NoError(t, err)
	restarted := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {root},
		},
		Machine: "local",
	})
	t.Cleanup(restarted.Close)

	retry := restarted.SyncAll(context.Background(), nil)
	require.Equal(t, 2, retry.Synced,
		"the unchanged DAG source must retry every branch after a partial write")
	require.Zero(t, retry.Failed)
	fork, err = database.GetSession(t.Context(), "forked-i")
	require.NoError(t, err)
	require.NotNil(t, fork, "the retry must restore the missing fork")

	_, hasDigest, err = database.GetProviderStatHash(
		t.Context(), parser.AgentClaude, path,
	)
	require.NoError(t, err)
	assert.True(t, hasDigest,
		"the digest may persist after every branch commits")
}

func TestRestartedDigestGateIgnoresStaleTrashedClaudeMembers(t *testing.T) {
	for _, tc := range []struct {
		name         string
		trashMainToo bool
	}{
		{name: "active main with trashed fork"},
		{name: "all members trashed", trashMainToo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			projectDir := filepath.Join(root, "project-a")
			require.NoError(t, os.MkdirAll(projectDir, 0o755))
			path := filepath.Join(projectDir, "forked.jsonl")
			require.NoError(t, os.WriteFile(
				path, []byte(newClaudeDAGBuilder(true).String()), 0o644,
			))

			initial := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentClaude: {root},
				},
				Machine: "local",
			})
			require.Equal(t, 2, initial.SyncAll(t.Context(), nil).Synced)
			initial.Close()
			_, hasDigest, err := database.GetProviderStatHash(
				t.Context(), parser.AgentClaude, path,
			)
			require.NoError(t, err)
			require.True(t, hasDigest)

			require.NoError(t, database.SoftDeleteSession("forked-i"))
			require.NoError(t, database.SetSessionDataVersion(
				"forked-i", db.CurrentDataVersion()-1,
			))
			if tc.trashMainToo {
				require.NoError(t, database.SoftDeleteSession("forked"))
				require.NoError(t, database.SetSessionDataVersion(
					"forked", db.CurrentDataVersion()-1,
				))
			}

			innerFactory, ok := parser.ProviderFactoryByType(parser.AgentClaude)
			require.True(t, ok)
			inner := innerFactory.NewProvider(parser.ProviderConfig{
				Roots: []string{root}, Machine: "local",
			})
			require.NotNil(t, inner)
			counting := &digestFingerprintCountingProvider{Provider: inner}
			restarted := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentClaude: {root},
				},
				Machine: "local",
				ProviderFactories: []parser.ProviderFactory{
					digestFingerprintCountingFactory{provider: counting},
				},
			})
			t.Cleanup(restarted.Close)
			var contentHashCalls atomic.Int64
			originalComputeFileHashPrefix := computeFileHashPrefix
			computeFileHashPrefix = func(
				path string, size int64,
			) (string, error) {
				contentHashCalls.Add(1)
				return originalComputeFileHashPrefix(path, size)
			}
			t.Cleanup(func() {
				computeFileHashPrefix = originalComputeFileHashPrefix
			})

			stats := restarted.SyncAll(t.Context(), nil)

			assert.Zero(t, stats.Synced)
			assert.Zero(t, stats.Failed)
			assert.Zero(t, counting.calls.Load(),
				"stale trashed members must not defeat the persisted digest")
			assert.Zero(t, contentHashCalls.Load(),
				"stale trashed members must not force a transcript hash")
		})
	}
}

func TestSyncClaudeDAGIntentionalSkipCompletesActiveMembers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		trashed    bool
		syncSingle bool
	}{
		{name: "sync_all/excluded"},
		{name: "sync_all/trashed", trashed: true},
		{name: "sync_single/excluded", syncSingle: true},
		{name: "sync_single/trashed", trashed: true, syncSingle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			projectDir := filepath.Join(root, "project-a")
			require.NoError(t, os.MkdirAll(projectDir, 0o755))
			path := filepath.Join(projectDir, "forked.jsonl")
			builder := newClaudeDAGBuilder(true)
			require.NoError(t,
				os.WriteFile(path, []byte(builder.String()), 0o644))

			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentClaude: {root},
				},
				Machine: "local",
			})
			t.Cleanup(engine.Close)
			initial := engine.SyncAll(context.Background(), nil)
			require.Equal(t, 2, initial.Synced)
			require.Zero(t, initial.Failed)
			if tc.trashed {
				require.NoError(t, database.SoftDeleteSession("forked-i"))
			} else {
				require.NoError(t, database.DeleteSession("forked-i"))
			}

			builder.AddClaudeUserWithUUID(
				"2024-01-01T10:02:00Z", "main-6", "m", "l",
			).AddClaudeAssistantWithUUID(
				"2024-01-01T10:02:01Z", "ok-6", "n", "m",
			)
			require.NoError(t, os.Remove(path))
			require.NoError(t,
				os.WriteFile(path, []byte(builder.String()), 0o644))

			if tc.syncSingle {
				require.NoError(t, engine.SyncSingleSession("forked"))
			} else {
				changed := engine.SyncAll(context.Background(), nil)
				require.Equal(t, 1, changed.Synced)
				require.Zero(t, changed.Failed)
			}
			assert.Equal(t, db.CurrentDataVersion(),
				database.GetSessionDataVersion("forked"))
			if tc.trashed {
				assert.Equal(t, db.CurrentDataVersion(),
					database.GetSessionDataVersion("forked-i"),
					"the trashed fork must not be demoted")
			}
			_, hasDigest, err := database.GetProviderStatHash(
				t.Context(), parser.AgentClaude, path,
			)
			require.NoError(t, err)
			assert.True(t, hasDigest,
				"an intentional fork skip must not block source freshness")

			restarted := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentClaude: {root},
				},
				Machine: "local",
			})
			t.Cleanup(restarted.Close)
			noop := restarted.SyncAll(context.Background(), nil)
			assert.Zero(t, noop.Synced,
				"a completed active branch must not rewrite on restart")
			assert.Zero(t, noop.Failed)

			if !tc.trashed {
				return
			}
			restoredCount, err := database.RestoreSession("forked-i")
			require.NoError(t, err)
			require.EqualValues(t, 1, restoredCount)
			assert.Less(t,
				database.GetSessionDataVersion("forked-i"),
				db.CurrentDataVersion(),
				"a restored fork must be eligible for source reparse",
			)
			_, hasDigest, err = database.GetProviderStatHash(
				t.Context(), parser.AgentClaude, path,
			)
			require.NoError(t, err)
			assert.False(t, hasDigest,
				"restoring a fork must invalidate the source digest")

			// Pin the source stat to the current main row. The restored fork's
			// stale data version must still defeat the stem-based Claude gate.
			// Normalize the project and unavailable file identity so this does
			// not depend on temporary-directory naming or inode reuse by the host.
			raw, err := sql.Open("sqlite3", database.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			_, err = raw.Exec(
				`UPDATE sessions
				 SET project = ?, file_inode = 0, file_device = 0
				 WHERE file_path = ?`,
				"project-a", path,
			)
			require.NoError(t, err)
			mainSession, err := database.GetSession(t.Context(), "forked")
			require.NoError(t, err)
			require.NotNil(t, mainSession)
			require.Equal(t, "project-a", mainSession.Project)
			require.Equal(t, path, database.GetSessionFilePath("forked"))
			storedSize, storedMtime, ok := database.GetSessionFileInfo("forked")
			require.True(t, ok)
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, storedSize, info.Size())
			storedTime := time.Unix(0, storedMtime)
			require.NoError(t, os.Chtimes(path, storedTime, storedTime))
			info, err = os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, storedMtime, info.ModTime().UnixNano())

			if tc.syncSingle {
				require.NoError(t, restarted.SyncSingleSession("forked"))
			} else {
				restoredSync := restarted.SyncAll(context.Background(), nil)
				require.Zero(t, restoredSync.Failed)
				assert.Equal(t, 2, restoredSync.Synced,
					"restoring a fork must reparse the complete DAG")
			}
			assert.Equal(t, db.CurrentDataVersion(),
				database.GetSessionDataVersion("forked-i"))
		})
	}
}

func newClaudeDAGBuilder(includeFork bool) *testjsonl.SessionBuilder {
	builder := testjsonl.NewSessionBuilder().
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:00Z", "start", "a", "",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:01Z", "ok", "b", "a",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:02Z", "main-2", "c", "b",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:03Z", "ok-2", "d", "c",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:04Z", "main-3", "e", "d",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:05Z", "ok-3", "f", "e",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:06Z", "main-4", "g", "f",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:07Z", "ok-4", "h", "g",
		).
		AddClaudeUserWithUUID(
			"2024-01-01T10:00:08Z", "main-5", "k", "h",
		).
		AddClaudeAssistantWithUUID(
			"2024-01-01T10:00:09Z", "ok-5", "l", "k",
		)
	if includeFork {
		addClaudeDAGFork(builder)
	}
	return builder
}

func addClaudeDAGFork(builder *testjsonl.SessionBuilder) {
	builder.AddClaudeUserWithUUID(
		"2024-01-01T10:01:00Z", "fork", "i", "b",
	).AddClaudeAssistantWithUUID(
		"2024-01-01T10:01:01Z", "fork-ok", "j", "i",
	)
}
