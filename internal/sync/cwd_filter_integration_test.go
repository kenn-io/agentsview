package sync_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func setupClaudeEnvWithCwdPrefixes(
	t *testing.T, prefixes []string,
) *testEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	env := &testEnv{db: dbtest.OpenTestDB(t), claudeDir: t.TempDir()}
	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {env.claudeDir},
		},
		Machine:            "local",
		IncludeCwdPrefixes: prefixes,
	})
	return env
}

func TestSyncEngineCwdPrefixFilter(t *testing.T) {
	env := setupClaudeEnvWithCwdPrefixes(
		t, []string{"/Users/alice/work"},
	)

	inside := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Inside", "/Users/alice/work/my-app").
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	outside := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Outside", "/Users/alice/personal/blog").
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	sibling := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Sibling", "/Users/alice/workspace").
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()

	env.writeClaudeSessionForProject(
		t, "/Users/alice/work/my-app",
		"inside-session.jsonl", inside,
	)
	env.writeClaudeSessionForProject(
		t, "/Users/alice/personal/blog",
		"outside-session.jsonl", outside,
	)
	env.writeClaudeSessionForProject(
		t, "/Users/alice/workspace",
		"sibling-session.jsonl", sibling,
	)

	env.engine.SyncAll(context.Background(), nil)

	assertSessionProject(t, env.db, "inside-session", "my_app")
	for _, id := range []string{"outside-session", "sibling-session"} {
		sess, err := env.db.GetSession(context.Background(), id)
		require.NoError(t, err, "GetSession(%q)", id)
		assert.Nil(t, sess,
			"session %q outside the cwd allow-list must not be ingested", id)
	}
}

// A session archived before the cwd allow-list was configured must not
// keep receiving appended messages through the incremental JSONL path,
// which bypasses the prepareSessionWrite veto.
func TestSyncEngineCwdPrefixFilterBlocksIncrementalAppend(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Archive the outside-prefix session with no filter configured,
	// as if it was ingested before sync_include_cwd_prefixes was set.
	env := &testEnv{db: dbtest.OpenTestDB(t), claudeDir: t.TempDir()}
	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {env.claudeDir},
		},
		Machine: "local",
	})

	initial := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Outside", "/Users/alice/personal/blog").
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	path := env.writeClaudeSessionForProject(
		t, "/Users/alice/personal/blog",
		"outside-append.jsonl", initial,
	)
	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "outside-append", 2)

	// Turn the filter on and append to the archived session's file.
	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {env.claudeDir},
		},
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/Users/alice/work"},
	})

	appended := testjsonl.ClaudeUserJSON("appended", tsEarlyS1) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(appended)
	f.Close()
	require.NoError(t, err, "append")

	env.engine.SyncPaths([]string{path})

	// Neither the incremental path nor the full-parse fallback may
	// store the appended message; the archived rows stay untouched.
	assertSessionMessageCount(t, env.db, "outside-append", 2)
	assertMessageRoles(t, env.db, "outside-append", "user", "assistant")
}

func TestReconcileWatchRootsCwdFilteredSourceRevokesDeletionProof(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	env := &testEnv{db: dbtest.OpenTestDB(t), claudeDir: t.TempDir()}
	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {env.claudeDir},
		},
		Machine: "local",
	})
	t.Cleanup(func() { env.engine.Close() })

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Outside", "/workspace/personal/blog").
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	path := env.writeClaudeSessionForProject(
		t, "/workspace/personal/blog", "outside-reconcile.jsonl", content,
	)
	allowedContent := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Inside", "/workspace/work/project").
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	allowedPath := env.writeClaudeSessionForProject(
		t, "/workspace/work/project", "inside-reconcile.jsonl", allowedContent,
	)
	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "outside-reconcile", 2)
	assertSessionMessageCount(t, env.db, "inside-reconcile", 2)

	ownership, err := env.db.ListActiveSessionSourceOwnershipScopesPage(
		context.Background(), "local", string(parser.AgentClaude),
		[]db.StoredSourcePathHintScope{{Path: env.claudeDir}},
		db.SessionSourceCursor{},
	)
	require.NoError(t, err)
	require.Len(t, ownership, 2,
		"initial successful sync must establish deletion proof")

	// Truncate the source so reconciliation must parse it and evaluate the
	// newly configured allow-list instead of taking the unchanged-source skip.
	filtered := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Outside", "/workspace/personal/blog").
		String()
	require.NoError(t, os.WriteFile(path, []byte(filtered), 0o644))

	env.engine.Close()
	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {env.claudeDir},
		},
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/workspace/work"},
	})
	require.NoError(t, env.engine.ReconcileWatchRootsAfterLostEvents(
		context.Background(), []string{env.claudeDir}, false,
	))

	ownership, err = env.db.ListActiveSessionSourceOwnershipScopesPage(
		context.Background(), "local", string(parser.AgentClaude),
		[]db.StoredSourcePathHintScope{{Path: env.claudeDir}},
		db.SessionSourceCursor{},
	)
	require.NoError(t, err)
	require.Len(t, ownership, 1,
		"only the CWD-admitted source may retain deletion proof")
	assert.Equal(t, allowedPath, ownership[0].FilePath)
	assertSessionMessageCount(t, env.db, "outside-reconcile", 2)
	assertSessionMessageCount(t, env.db, "inside-reconcile", 2)

	require.NoError(t, os.Remove(path))
	require.NoError(t, env.engine.ReconcileWatchRootsAfterLostEvents(
		context.Background(), []string{env.claudeDir}, false,
	))
	assertSessionMessageCount(t, env.db, "outside-reconcile", 2)
	assertSessionMessageCount(t, env.db, "inside-reconcile", 2)
}

func TestSyncAllCwdFilterChangeRevokesSkippedSourceDeletionProof(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	env := &testEnv{db: dbtest.OpenTestDB(t), claudeDir: t.TempDir()}
	newEngine := func(sourceMachine string, prefixes []string) *sync.Engine {
		return sync.NewEngine(env.db, sync.EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentClaude: {env.claudeDir},
			},
			SourceMachines: map[parser.AgentType]map[string]string{
				parser.AgentClaude: {env.claudeDir: sourceMachine},
			},
			Machine:            "local",
			IncludeCwdPrefixes: prefixes,
		})
	}

	env.engine = newEngine("archivebox", nil)
	t.Cleanup(func() { env.engine.Close() })
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Outside", "/workspace/personal/blog").
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	path := env.writeClaudeSessionForProject(
		t, "/workspace/personal/blog", "outside-periodic.jsonl", content,
	)
	env.engine.SyncAll(t.Context(), nil)
	assertSessionMessageCount(t, env.db, "outside-periodic", 2)

	// Restart with a newly restrictive filter, leaving the source unchanged so
	// ordinary discovery takes its freshness-skip path. The skipped source must
	// lose the deletion proof established before the filter changed.
	env.engine.Close()
	env.engine = newEngine("renamedbox", []string{"/workspace/work"})
	env.engine.SyncAll(t.Context(), nil)
	ownership, err := env.db.ListActiveSessionSourceOwnershipScopesPage(
		t.Context(), "archivebox", string(parser.AgentClaude),
		[]db.StoredSourcePathHintScope{{Path: env.claudeDir}},
		db.SessionSourceCursor{},
	)
	require.NoError(t, err)
	assert.Empty(t, ownership,
		"an unchanged source rejected by the new CWD filter must lose deletion proof")

	require.NoError(t, os.Remove(path))
	require.NoError(t, env.engine.ReconcileWatchRoots(
		t.Context(), []string{env.claudeDir}, false,
	))
	assertSessionMessageCount(t, env.db, "outside-periodic", 2)
}

func TestCwdFilterBaselinesOnlyAdmittedStaleClaudeForkAfterZeroResultParse(
	t *testing.T,
) {
	for _, reconcile := range []bool{false, true} {
		name := "full sync"
		if reconcile {
			name = "watch reconciliation"
		}
		t.Run(name, func(t *testing.T) {
			env := setupClaudeEnvWithCwdPrefixes(
				t, []string{"/workspace/work"},
			)
			pureReplay := strings.Join([]string{
				`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-01-01T10:00:00Z","sessionId":"fork-2222","sessionKind":"bg","message":{"content":"first question"}}`,
				`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-01-01T10:00:05Z","sessionId":"fork-2222","sessionKind":"bg","message":{"id":"msg_01","content":[{"type":"text","text":"first answer"}]}}`,
			}, "\n") + "\n"
			path := env.writeClaudeSession(
				t, "project", "fork-2222.jsonl", pureReplay,
			)
			info, err := os.Stat(path)
			require.NoError(t, err)
			fileSize := info.Size()
			fileMtime := info.ModTime().UnixNano()
			fileHash := fmt.Sprintf("%x", sha256.Sum256([]byte(pureReplay)))
			parentID := "fork-2222"
			allowedID := parentID + "-11111111-2222-4333-8444-555555555555"
			disallowedID := parentID + "-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			for id, cwd := range map[string]string{
				allowedID:    "/workspace/work/project",
				disallowedID: "/workspace/personal/project",
			} {
				require.NoError(t, env.db.UpsertSession(db.Session{
					ID:               id,
					Project:          "project",
					Machine:          "local",
					Agent:            "claude",
					Cwd:              cwd,
					ParentSessionID:  &parentID,
					RelationshipType: "fork",
					FilePath:         &path,
					FileSize:         &fileSize,
					FileMtime:        &fileMtime,
					FileHash:         &fileHash,
				}))
				require.NoError(t, env.db.SetSessionDataVersion(id, 0))
			}
			syncSource := func() {
				if reconcile {
					require.NoError(t, env.engine.ReconcileWatchRootsAfterLostEvents(
						t.Context(), []string{env.claudeDir}, false,
					))
					return
				}
				stats := env.engine.SyncAll(t.Context(), nil)
				require.Zero(t, stats.Failed)
			}

			syncSource()
			for id, want := range map[string]int{
				allowedID: 1, disallowedID: 0,
			} {
				var got int
				require.NoError(t, env.db.Reader().QueryRow(`
					SELECT count(*) FROM local_session_source_baselines
					WHERE session_id = ?`, id,
				).Scan(&got))
				assert.Equal(t, want, got,
					"only the CWD-admitted stale member may gain deletion proof")
			}

			syncSource()
			allowed, err := env.db.GetSessionFull(t.Context(), allowedID)
			require.NoError(t, err)
			require.NotNil(t, allowed)
			require.NotNil(t, allowed.DeletionCause,
				"the admitted stale fork must converge after baseline establishment")
			assert.Equal(t, "source_missing", *allowed.DeletionCause)
			disallowed, err := env.db.GetSession(t.Context(), disallowedID)
			require.NoError(t, err)
			assert.NotNil(t, disallowed,
				"a stale fork outside the CWD allow-list must remain active")

			if !reconcile {
				env.engine.Close()
				env.engine = sync.NewEngine(env.db, sync.EngineConfig{
					AgentDirs: map[parser.AgentType][]string{
						parser.AgentClaude: {env.claudeDir},
					},
					Machine:            "local",
					IncludeCwdPrefixes: []string{"/workspace/work"},
				})
				steady := env.engine.SyncAll(t.Context(), nil)
				require.Zero(t, steady.Failed)
				assert.Equal(t, 1, steady.Skipped,
					"a persisted current-version rowless marker must restore source freshness")

				env.engine.Close()
				env.engine = sync.NewEngine(env.db, sync.EngineConfig{
					AgentDirs: map[parser.AgentType][]string{
						parser.AgentClaude: {env.claudeDir},
					},
					Machine:            "local",
					IncludeCwdPrefixes: []string{"/workspace/personal"},
				})
				t.Cleanup(env.engine.Close)
				broadened := env.engine.SyncAll(t.Context(), nil)
				require.Zero(t, broadened.Failed)
				assert.Zero(t, broadened.Skipped,
					"a broader CWD filter must revoke the freshness exemption")
				require.Zero(t, env.engine.SyncAll(t.Context(), nil).Failed)
				disallowed, err = env.db.GetSessionFull(t.Context(), disallowedID)
				require.NoError(t, err)
				require.NotNil(t, disallowed)
				require.NotNil(t, disallowed.DeletionCause,
					"the newly admitted stale fork must resume reconciliation")
			}
		})
	}
}

func TestSyncSingleSessionBaselinesOnlyAdmittedStaleClaudeFork(t *testing.T) {
	env := setupClaudeEnvWithCwdPrefixes(
		t, []string{"/workspace/work"},
	)
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "hello", "/workspace/work/project").
		String()
	path := env.writeClaudeSession(
		t, "project", "single-cwd.jsonl", content,
	)
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)

	parentID := "single-cwd"
	allowedID := parentID + "-11111111-2222-4333-8444-555555555555"
	disallowedID := parentID + "-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for id, cwd := range map[string]string{
		allowedID:    "/workspace/work/project",
		disallowedID: "/workspace/personal/project",
	} {
		require.NoError(t, env.db.UpsertSession(db.Session{
			ID:               id,
			Project:          "project",
			Machine:          "local",
			Agent:            "claude",
			Cwd:              cwd,
			ParentSessionID:  &parentID,
			RelationshipType: "fork",
			FilePath:         &path,
		}))
		require.NoError(t, env.db.SetSessionDataVersion(id, 0))
	}
	require.NoError(t, env.db.ReplaceActiveSessionSourceBaselines(
		t.Context(), "local",
		[]db.SessionSourcePath{{Agent: "claude", FilePath: path}}, nil,
	))

	require.NoError(t, env.engine.SyncSingleSession(parentID))
	for id, want := range map[string]int{allowedID: 1, disallowedID: 0} {
		var got int
		require.NoError(t, env.db.Reader().QueryRow(`
			SELECT count(*) FROM local_session_source_baselines
			WHERE session_id = ?`, id,
		).Scan(&got))
		assert.Equal(t, want, got,
			"single-session sync must baseline only CWD-admitted stale forks")
	}

	require.NoError(t, env.engine.SyncSingleSession(parentID))
	allowed, err := env.db.GetSessionFull(t.Context(), allowedID)
	require.NoError(t, err)
	require.NotNil(t, allowed)
	require.NotNil(t, allowed.DeletionCause,
		"a later single-session sync must retire the admitted stale fork")
	assert.Equal(t, "source_missing", *allowed.DeletionCause)
	disallowed, err := env.db.GetSession(t.Context(), disallowedID)
	require.NoError(t, err)
	assert.NotNil(t, disallowed,
		"single-session sync must preserve a filtered stale fork")
}

func TestSyncSingleSessionEmitsSessionsForStaleClaudeForkTombstone(
	t *testing.T,
) {
	emitter := &fakeEmitter{}
	env := &testEnv{db: dbtest.OpenTestDB(t), claudeDir: t.TempDir()}
	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {env.claudeDir},
		},
		Machine: "local",
		Emitter: emitter,
	})
	t.Cleanup(env.engine.Close)
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "hello", "/workspace/work/project").
		String()
	path := env.writeClaudeSession(
		t, "project", "single-notify.jsonl", content,
	)
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	emitter.mu.Lock()
	emitter.scopes = nil
	emitter.mu.Unlock()

	parentID := "single-notify"
	staleID := parentID + "-11111111-2222-4333-8444-555555555555"
	require.NoError(t, env.db.UpsertSession(db.Session{
		ID:               staleID,
		Project:          "project",
		Machine:          "local",
		Agent:            "claude",
		ParentSessionID:  &parentID,
		RelationshipType: "fork",
		FilePath:         &path,
	}))
	require.NoError(t, env.db.SetSessionDataVersion(staleID, 0))
	require.NoError(t, env.db.BaselineActiveSessionSourceOwnerships(
		t.Context(), []db.SessionSourceOwnership{{
			ID: staleID, Machine: "local", Agent: "claude", FilePath: path,
		}},
	))

	require.NoError(t, env.engine.SyncSingleSession(parentID))
	assert.Equal(t, []string{"messages", "sessions"}, emitter.got(),
		"single-session fork cleanup must refresh messages and the session index")
	stale, err := env.db.GetSessionFull(t.Context(), staleID)
	require.NoError(t, err)
	require.NotNil(t, stale)
	require.NotNil(t, stale.DeletionCause)
	assert.Equal(t, "source_missing", *stale.DeletionCause)
}

func TestSyncSingleSessionFreshnessSkipReconcilesNarrowedCwdBaselines(
	t *testing.T,
) {
	env := setupClaudeEnvWithCwdPrefixes(t, nil)
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "hello", "/workspace/work/project").
		String()
	path := env.writeClaudeSession(
		t, "project", "single-fresh-cwd.jsonl", content,
	)
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)

	parentID := "single-fresh-cwd"
	rejectedID := parentID + "-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	require.NoError(t, env.db.UpsertSession(db.Session{
		ID:               rejectedID,
		Project:          "project",
		Machine:          "local",
		Agent:            "claude",
		Cwd:              "/workspace/personal/project",
		ParentSessionID:  &parentID,
		RelationshipType: "fork",
		FilePath:         &path,
	}))
	require.NoError(t, env.db.SetSessionDataVersion(rejectedID, 0))
	require.NoError(t, env.db.BaselineActiveSessionSourcePaths(
		t.Context(), "local",
		[]db.SessionSourcePath{{Agent: "claude", FilePath: path}},
	))

	env.engine.Close()
	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {env.claudeDir},
		},
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/workspace/work"},
	})
	t.Cleanup(env.engine.Close)

	require.NoError(t, env.engine.SyncSingleSession(parentID))
	require.NoError(t, os.Remove(path))
	require.NoError(t, env.engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{env.claudeDir}, false,
	))

	primary, err := env.db.GetSessionFull(t.Context(), parentID)
	require.NoError(t, err)
	require.NotNil(t, primary)
	require.NotNil(t, primary.DeletionCause,
		"the admitted primary must retain exact deletion proof")
	assert.Equal(t, "source_missing", *primary.DeletionCause)
	rejected, err := env.db.GetSession(t.Context(), rejectedID)
	require.NoError(t, err)
	assert.NotNil(t, rejected,
		"a fresh single-session skip must revoke the rejected fork's old proof")
}

func TestSyncAllCwdRejectedStaleClaudeForkReturnsToFreshnessSkip(
	t *testing.T,
) {
	env := setupClaudeEnvWithCwdPrefixes(
		t, []string{"/workspace/work"},
	)
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "hello", "/workspace/work/project").
		String()
	path := env.writeClaudeSession(
		t, "project", "freshness-cwd.jsonl", content,
	)
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)

	parentID := "freshness-cwd"
	allowedID := parentID + "-11111111-2222-4333-8444-555555555555"
	disallowedID := parentID + "-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for id, cwd := range map[string]string{
		allowedID:    "/workspace/work/project",
		disallowedID: "/workspace/personal/project",
	} {
		require.NoError(t, env.db.UpsertSession(db.Session{
			ID:               id,
			Project:          "project",
			Machine:          "local",
			Agent:            "claude",
			Cwd:              cwd,
			ParentSessionID:  &parentID,
			RelationshipType: "fork",
			FilePath:         &path,
		}))
		require.NoError(t, env.db.SetSessionDataVersion(id, 0))
	}

	first := env.engine.SyncAll(t.Context(), nil)
	require.Zero(t, first.Failed)
	second := env.engine.SyncAll(t.Context(), nil)
	require.Zero(t, second.Failed)
	allowed, err := env.db.GetSessionFull(t.Context(), allowedID)
	require.NoError(t, err)
	require.NotNil(t, allowed)
	require.NotNil(t, allowed.DeletionCause,
		"the admitted stale fork must be retired before testing freshness")
	assert.Equal(t, "source_missing", *allowed.DeletionCause)
	disallowed, err := env.db.GetSession(t.Context(), disallowedID)
	require.NoError(t, err)
	require.NotNil(t, disallowed,
		"the CWD-rejected stale fork must remain active")

	third := env.engine.SyncAll(t.Context(), nil)
	require.Zero(t, third.Failed)
	assert.Zero(t, third.Synced,
		"a rejected stale fork must not force an unchanged source to reparse")
	assert.Equal(t, 1, third.Skipped,
		"the unchanged source must return to the Claude freshness path")

	require.NoError(t, os.Remove(path))
	require.NoError(t, env.engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{env.claudeDir}, false,
	))
	primary, err := env.db.GetSessionFull(t.Context(), parentID)
	require.NoError(t, err)
	require.NotNil(t, primary)
	require.NotNil(t, primary.DeletionCause,
		"the admitted primary must retain proof through the mixed-CWD skip")
	assert.Equal(t, "source_missing", *primary.DeletionCause)
	disallowed, err = env.db.GetSession(t.Context(), disallowedID)
	require.NoError(t, err)
	assert.NotNil(t, disallowed,
		"the mixed-CWD skip must not grant proof to the rejected fork")
}

func TestSyncAllParsesPrimaryBeforeTrustingPreservedClaudeForkFreshness(
	t *testing.T,
) {
	env := setupClaudeEnvWithCwdPrefixes(
		t, []string{"/workspace/work"},
	)
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "new primary", "/workspace/work/project").
		String()
	path := env.writeClaudeSession(
		t, "project", "upgrade-primary.jsonl", content,
	)
	info, err := os.Stat(path)
	require.NoError(t, err)
	fileSize := info.Size()
	fileMtime := info.ModTime().UnixNano()
	fileHash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	parentID := "upgrade-primary"
	staleID := parentID + "-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	require.NoError(t, env.db.UpsertSession(db.Session{
		ID:               staleID,
		Project:          "project",
		Machine:          "local",
		Agent:            "claude",
		Cwd:              "/workspace/personal/project",
		ParentSessionID:  &parentID,
		RelationshipType: "fork",
		FilePath:         &path,
		FileSize:         &fileSize,
		FileMtime:        &fileMtime,
		FileHash:         &fileHash,
	}))
	require.NoError(t, env.db.SetSessionDataVersion(staleID, 0))

	stats := env.engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	assert.Equal(t, 1, stats.Synced,
		"legacy fork metadata must not suppress the current parser's first pass")
	primary, err := env.db.GetSession(t.Context(), parentID)
	require.NoError(t, err)
	assert.NotNil(t, primary,
		"the newly parseable primary session must be archived")
	stale, err := env.db.GetSession(t.Context(), staleID)
	require.NoError(t, err)
	assert.NotNil(t, stale,
		"the CWD-rejected legacy fork must remain preserved")
}

func TestReconcileWatchRootsReportsSourceMissingClaudeForkTombstone(
	t *testing.T,
) {
	emitter := &fakeEmitter{}
	env := &testEnv{db: dbtest.OpenTestDB(t), claudeDir: t.TempDir()}
	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {env.claudeDir},
		},
		Machine: "local",
		Emitter: emitter,
	})
	t.Cleanup(env.engine.Close)
	original := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-01-01T10:00:00Z","sessionId":"audit-original","message":{"content":"first question"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-01-01T10:00:05Z","sessionId":"audit-original","message":{"id":"msg_01","content":[{"type":"text","text":"first answer"}]}}`,
	}, "\n") + "\n"
	pureReplay := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-01-01T10:00:00Z","sessionId":"audit-replay","sessionKind":"bg","message":{"content":"first question"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-01-01T10:00:05Z","sessionId":"audit-replay","sessionKind":"bg","message":{"id":"msg_01","content":[{"type":"text","text":"first answer"}]}}`,
	}, "\n") + "\n"
	env.writeClaudeSession(
		t, "project", "audit-original.jsonl", original,
	)
	path := env.writeClaudeSession(
		t, "project", "audit-replay.jsonl", pureReplay,
	)
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced,
		"the original transcript should seed without writing the replay")
	emitter.mu.Lock()
	emitter.scopes = nil
	emitter.mu.Unlock()
	parentID := "audit-replay"
	staleID := parentID + "-11111111-2222-4333-8444-555555555555"
	require.NoError(t, env.db.UpsertSession(db.Session{
		ID:               staleID,
		Project:          "project",
		Machine:          "local",
		Agent:            "claude",
		Cwd:              "/workspace/work/project",
		ParentSessionID:  &parentID,
		RelationshipType: "fork",
		FilePath:         &path,
	}))
	require.NoError(t, env.db.SetSessionDataVersion(staleID, 0))
	require.NoError(t, env.db.BaselineActiveSessionSourceOwnerships(
		t.Context(), []db.SessionSourceOwnership{{
			ID: staleID, Machine: "local", Agent: "claude", FilePath: path,
		}},
	))

	stats, tombstoned, err := env.engine.ReconcileWatchRootsWithStats(
		t.Context(), []string{env.claudeDir}, false, nil,
	)
	require.NoError(t, err)
	assert.Zero(t, stats.Synced,
		"a zero-result replay should not report an ordinary session write")
	assert.Equal(t, 1, tombstoned,
		"the audit-facing reconciliation result must report the member tombstone")
	assert.Equal(t, []string{"sessions"}, emitter.got(),
		"member-only reconciliation changes must emit a sessions event")
	stale, err := env.db.GetSessionFull(t.Context(), staleID)
	require.NoError(t, err)
	require.NotNil(t, stale)
	require.NotNil(t, stale.DeletionCause)
	assert.Equal(t, "source_missing", *stale.DeletionCause)
}

func TestWatchReconcilePreservesCwdRejectedForkAfterMixedSourceDeleted(
	t *testing.T,
) {
	env := setupClaudeEnvWithCwdPrefixes(
		t, []string{"/workspace/work"},
	)
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "hello", "/workspace/work/project").
		String()
	path := env.writeClaudeSession(
		t, "project", "deleted-mixed-cwd.jsonl", content,
	)
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)

	parentID := "deleted-mixed-cwd"
	disallowedID := parentID + "-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	require.NoError(t, env.db.UpsertSession(db.Session{
		ID:               disallowedID,
		Project:          "project",
		Machine:          "local",
		Agent:            "claude",
		Cwd:              "/workspace/personal/project",
		ParentSessionID:  &parentID,
		RelationshipType: "fork",
		FilePath:         &path,
	}))
	require.NoError(t, env.db.SetSessionDataVersion(disallowedID, 0))

	changed := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "hello", "/workspace/work/project").
		AddClaudeAssistant(tsEarlyS5, "changed").
		String()
	require.NoError(t, os.WriteFile(path, []byte(changed), 0o644))
	parsed := env.engine.SyncAll(t.Context(), nil)
	require.Zero(t, parsed.Failed)
	require.Equal(t, 1, parsed.Synced,
		"the changed mixed-CWD source must take the full write path")

	require.NoError(t, os.Remove(path))
	require.NoError(t, env.engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{env.claudeDir}, false,
	))
	primary, err := env.db.GetSessionFull(t.Context(), parentID)
	require.NoError(t, err)
	require.NotNil(t, primary)
	require.NotNil(t, primary.DeletionCause,
		"the admitted primary must retain deletion proof")
	assert.Equal(t, "source_missing", *primary.DeletionCause)
	disallowed, err := env.db.GetSession(t.Context(), disallowedID)
	require.NoError(t, err)
	assert.NotNil(t, disallowed,
		"source-wide admission must not authorize deleting the rejected fork")
}

// A full resync where the cwd allow-list vetoes every discovered
// session is an intentional result, not a broken rebuild: the swap
// must proceed and the orphan copy must restore the archived rows
// (the filter gates ingestion only). Without a distinct filtered
// counter the abort guard reads such a run as an unsafe empty
// rebuild and leaves NeedsResync true forever.
func TestResyncAllProceedsWhenAllSessionsCwdFiltered(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Archive two sessions with no filter configured.
	env := &testEnv{db: dbtest.OpenTestDB(t), claudeDir: t.TempDir()}
	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {env.claudeDir},
		},
		Machine: "local",
	})

	first := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "First", "/Users/alice/personal/blog").
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	second := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Second", "/Users/alice/personal/notes").
		AddClaudeAssistant(tsEarlyS5, "ok").
		String()
	env.writeClaudeSessionForProject(
		t, "/Users/alice/personal/blog",
		"filtered-one.jsonl", first,
	)
	env.writeClaudeSessionForProject(
		t, "/Users/alice/personal/notes",
		"filtered-two.jsonl", second,
	)
	env.engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, env.db, "filtered-one", 2)
	assertSessionMessageCount(t, env.db, "filtered-two", 2)

	// Resync with an allow-list that excludes every session.
	env.engine = sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {env.claudeDir},
		},
		Machine:            "local",
		IncludeCwdPrefixes: []string{"/Users/alice/work"},
	})
	stats := env.engine.ResyncAll(context.Background(), nil)

	require.False(t, stats.Aborted,
		"all-filtered resync must not abort: %+v", stats.Warnings)
	assert.Equal(t, 0, stats.Synced, "synced")
	assert.Equal(t, 0, stats.Failed, "failed")
	assert.Equal(t, 2, stats.OrphanedCopied, "orphaned copied")

	// The archived sessions survive the swap via the orphan copy.
	assertSessionMessageCount(t, env.db, "filtered-one", 2)
	assertSessionMessageCount(t, env.db, "filtered-two", 2)
	assert.False(t, env.db.NeedsResync(),
		"completed resync must clear the needs-resync marker")
}

func TestSyncAllSourceMissingPrimaryWithRejectedForkReturnsToFreshnessSkip(
	t *testing.T,
) {
	env := setupClaudeEnvWithCwdPrefixes(
		t, []string{"/workspace/work"},
	)
	pureReplay := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-01-01T10:00:00Z","sessionId":"missing-primary","sessionKind":"bg","message":{"content":"first question"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-01-01T10:00:05Z","sessionId":"missing-primary","sessionKind":"bg","message":{"id":"msg_01","content":[{"type":"text","text":"first answer"}]}}`,
	}, "\n") + "\n"
	path := env.writeClaudeSession(
		t, "project", "missing-primary.jsonl", pureReplay,
	)

	parentID := "missing-primary"
	rejectedID := parentID + "-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	require.NoError(t, env.db.UpsertSession(db.Session{
		ID:       parentID,
		Project:  "project",
		Machine:  "local",
		Agent:    "claude",
		Cwd:      "/workspace/work/project",
		FilePath: &path,
	}))
	require.NoError(t, env.db.BaselineActiveSessionSourceOwnerships(
		t.Context(), []db.SessionSourceOwnership{{
			ID: parentID, Machine: "local", Agent: "claude", FilePath: path,
		}},
	))
	tombstoned, err := env.db.SoftDeleteSessionSourceOwnership(
		t.Context(), "local", "claude", parentID, path,
	)
	require.NoError(t, err)
	require.True(t, tombstoned, "seed a source-missing canonical primary")
	require.NoError(t, env.db.UpsertSession(db.Session{
		ID:               rejectedID,
		Project:          "project",
		Machine:          "local",
		Agent:            "claude",
		Cwd:              "/workspace/personal/project",
		ParentSessionID:  &parentID,
		RelationshipType: "fork",
		FilePath:         &path,
	}))
	require.NoError(t, env.db.SetSessionDataVersion(rejectedID, 0))

	first := env.engine.SyncAll(t.Context(), nil)
	require.Zero(t, first.Failed)
	second := env.engine.SyncAll(t.Context(), nil)
	require.Zero(t, second.Failed)
	assert.Zero(t, second.Synced)
	assert.Equal(t, 1, second.Skipped,
		"a source-missing primary must not defeat the rowless freshness skip")

	primary, err := env.db.GetSessionFull(t.Context(), parentID)
	require.NoError(t, err)
	require.NotNil(t, primary)
	require.NotNil(t, primary.DeletionCause,
		"the source-missing primary must stay tombstoned")
	assert.Equal(t, "source_missing", *primary.DeletionCause)
	rejected, err := env.db.GetSession(t.Context(), rejectedID)
	require.NoError(t, err)
	assert.NotNil(t, rejected,
		"the CWD-rejected stale fork must remain active")
}
