package sync_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vsCopilotTraceLine builds one OpenTelemetry trace JSONL line carrying a
// single span for the given conversation.
func vsCopilotTraceLine(
	conversationID, spanID, name, start, end string,
	attrs map[string]string,
) string {
	allAttrs := []string{
		`{"key":"gen_ai.conversation.id","value":{"stringValue":"` +
			conversationID + `"}}`,
	}
	for key, value := range attrs {
		encoded, _ := json.Marshal(value)
		allAttrs = append(allAttrs,
			`{"key":"`+key+`","value":{"stringValue":`+
				string(encoded)+`}}`,
		)
	}
	return `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"trace",` +
		`"spanId":"` + spanID + `","name":"` + name +
		`","startTimeUnixNano":"` + start +
		`","endTimeUnixNano":"` + end +
		`","attributes":[` + strings.Join(allAttrs, ",") +
		`]}]}]}]}`
}

// TestSyncEngineVisualStudioCopilotMultipleConversationsPerFile verifies that a
// single trace file containing spans for more than one conversation syncs every
// conversation as its own session. The dominant conversation outranks the
// secondary one under the old "best conversation" heuristic, which used to drop
// the secondary conversation entirely.
func TestSyncEngineVisualStudioCopilotMultipleConversationsPerFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	dominant := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	secondary := "c0aca2e3-d1f2-4d28-bd5e-5dab29e2be28"

	data := strings.Join([]string{
		vsCopilotTraceLine(dominant, "d1", "chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Update the XAML."}]}]`,
			}),
		vsCopilotTraceLine(dominant, "d2", "chat gpt-5.5",
			"1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Now run the build."}]}]`,
			}),
		vsCopilotTraceLine(secondary, "s1", "chat gpt-5.5",
			"1781294552800436000", "1781294586729109400",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Refactor the parser."}]}]`,
			}),
	}, "\n") + "\n"
	tracePath := filepath.Join(
		tracesDir, "20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(tracePath, []byte(data), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})

	stats := engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 2, stats.Synced,
		"both conversations in the file should sync")

	assertSessionMessageCount(t, database,
		"visualstudio-copilot:"+dominant, 2)
	assertSessionMessageCount(t, database,
		"visualstudio-copilot:"+secondary, 1)

	// The stored file_path is a virtual sync key, but `session export`
	// and other source consumers must still resolve it to an openable
	// trace file.
	stored := database.GetSessionFilePath("visualstudio-copilot:" + dominant)
	require.NotEmpty(t, stored)
	resolved := parser.ResolveSourceFilePath(stored)
	f, err := os.Open(resolved)
	require.NoError(t, err,
		"resolved source path should open: %s", resolved)
	require.NoError(t, f.Close())
}

// TestSyncEngineVisualStudioCopilotReadErrorNotCachedAsSkip verifies that a
// trace read failure is reported on every sync rather than cached as a skip.
// Caching by mtime would hide the failure once the file became readable
// without a content change (e.g. a permission fix).
func TestSyncEngineVisualStudioCopilotReadErrorNotCachedAsSkip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	tracesDir := t.TempDir()
	// An unreadable trace file: a symlink to a directory cannot be
	// scanned as JSONL, and its (followed) mtime is stable across syncs.
	target := filepath.Join(t.TempDir(), "dir")
	require.NoError(t, os.Mkdir(target, 0o755))
	tracePath := filepath.Join(
		tracesDir, "20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.Symlink(target, tracePath))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})

	first := engine.SyncAll(context.Background(), nil)
	require.NotZero(t, first.Failed, "read failure should be reported")

	// The same engine syncs again with the file unchanged. The failure
	// must surface again instead of being silently skipped from cache.
	second := engine.SyncAll(context.Background(), nil)
	assert.NotZero(t, second.Failed,
		"read failure must not be cached as a skip")
	assert.Zero(t, second.Skipped,
		"an unreadable file must not be recorded as a skip")
}

// TestSyncEngineVisualStudioCopilotClearsStaleReadErrorSkip verifies that a
// skip-cache entry persisted by an older build (which cached VS Copilot read
// errors) is cleared when a new engine is constructed, so the read failure is
// retried rather than silently skipped after upgrade.
func TestSyncEngineVisualStudioCopilotClearsStaleReadErrorSkip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	tracesDir := t.TempDir()
	target := filepath.Join(t.TempDir(), "dir")
	require.NoError(t, os.Mkdir(target, 0o755))
	tracePath := filepath.Join(
		tracesDir, "20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.Symlink(target, tracePath))

	database := dbtest.OpenTestDB(t)
	// Seed the skip cache as an older build would have: the physical trace
	// path keyed by its current mtime, so an unchanged file matches and is
	// skipped before the read error can surface.
	info, err := os.Stat(tracePath)
	require.NoError(t, err)
	require.NoError(t, database.ReplaceSkippedFiles(map[string]int64{
		tracePath: info.ModTime().UnixNano(),
	}))

	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})

	stats := engine.SyncAll(context.Background(), nil)
	assert.NotZero(t, stats.Failed,
		"stale read-error skip must be cleared so the failure is retried")
	assert.Zero(t, stats.Skipped,
		"the stale skip entry must not suppress the file")
}

// vsCopilotSingleConversationTrace writes a one-conversation trace file and
// returns its path and the synced session id.
func vsCopilotSingleConversationTrace(
	t *testing.T, tracesDir, conversationID string,
) (string, string) {
	t.Helper()
	data := vsCopilotTraceLine(conversationID, "d1", "chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Hello."}]}]`,
		}) + "\n"
	tracePath := filepath.Join(
		tracesDir, "20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(tracePath, []byte(data), 0o644))
	return tracePath, "visualstudio-copilot:" + conversationID
}

// TestFindSourceFileVisualStudioCopilotReturnsVirtualPath verifies that source
// resolution returns the <traceFile>#<conversationID> virtual path. The stored
// path is virtual, so the existence check must resolve it to the physical trace
// while still returning the virtual path, keeping a re-sync scoped to the one
// conversation instead of re-parsing every conversation in the file.
func TestFindSourceFileVisualStudioCopilotReturnsVirtualPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	tracePath, sessionID := vsCopilotSingleConversationTrace(
		t, tracesDir, conversationID,
	)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)

	want := parser.VisualStudioCopilotVirtualPath(tracePath, conversationID)
	assert.Equal(t, want, engine.FindSourceFile(sessionID),
		"source resolution must return the conversation virtual path")
	assert.NotZero(t, engine.SourceMtime(sessionID),
		"mtime must resolve the virtual path to the physical trace")
}

func TestSyncRootsSinceVisualStudioCopilotPollTombstonesDeletedVS2026Session(
	t *testing.T,
) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	sessionPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "sessions",
		conversationID,
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		vsCopilotTraceLine(conversationID, "d1", "chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Hello."}]}]`,
			})+"\n"), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {root},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	assertSessionMessageCount(t, database, sessionID, 1)

	require.NoError(t, os.Remove(sessionPath))
	engine.SyncRootsSince(context.Background(), []string{root}, time.Time{}, nil)

	gone, err := database.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	assert.Nil(t, gone,
		"unwatched polling must tombstone deleted VS 2026 session files")
}

func TestSyncRootsSinceVisualStudioCopilotPollPreservesVS2026SessionWhenRootMissing(
	t *testing.T,
) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	sessionPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "sessions",
		conversationID,
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		vsCopilotTraceLine(conversationID, "d1", "chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Hello."}]}]`,
			})+"\n"), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {root},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	assertSessionMessageCount(t, database, sessionID, 1)

	require.NoError(t, os.RemoveAll(root))
	engine.SyncRootsSince(context.Background(), []string{root}, time.Time{}, nil)

	preserved, err := database.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, preserved,
		"polling must preserve archived VS 2026 sessions when the root is unavailable")
	assertSessionMessageCount(t, database, sessionID, 1)
}

func TestSyncRootsSinceVisualStudioCopilotRecanonicalizesDeletedVS2026SessionToOldLegacyTrace(
	t *testing.T,
) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	root := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	data := vsCopilotTraceLine(conversationID, "d1", "chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Hello."}]}]`,
		}) + "\n"
	legacyPath := filepath.Join(
		root, "20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	sessionPath := filepath.Join(
		root, ".vs", "SampleApp", "copilot-chat", "thread", "sessions",
		conversationID,
	)
	require.NoError(t, os.WriteFile(legacyPath, []byte(data), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(data), 0o644))
	legacyMtime := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	sessionMtime := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(legacyPath, legacyMtime, legacyMtime))
	require.NoError(t, os.Chtimes(sessionPath, sessionMtime, sessionMtime))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {root},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	vsVirtualPath := parser.VisualStudioCopilotVirtualPath(
		sessionPath, conversationID,
	)
	require.Equal(t, vsVirtualPath, database.GetSessionFilePath(sessionID))

	require.NoError(t, os.Remove(sessionPath))
	stats := engine.SyncRootsSince(
		context.Background(), []string{root},
		time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC), nil,
	)
	require.NotZero(t, stats.Synced)

	legacyVirtualPath := parser.VisualStudioCopilotVirtualPath(
		legacyPath, conversationID,
	)
	assert.Equal(t, legacyVirtualPath, database.GetSessionFilePath(sessionID))
	assertSessionMessageCount(t, database, sessionID, 1)
}

// TestSyncSingleSessionContextVisualStudioCopilotPreservesProject verifies that
// a single-session re-sync keeps the stored project rather than overwriting it
// with the provider's default project.
func TestSyncSingleSessionContextVisualStudioCopilotPreservesProject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	_, sessionID := vsCopilotSingleConversationTrace(
		t, tracesDir, conversationID,
	)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)

	before, err := database.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.Equal(t, "visualstudio", before.Project)
	before.Project = "stored-solution"
	require.NoError(t, database.UpsertSession(*before))

	require.NoError(t, engine.SyncSingleSessionContext(
		context.Background(), sessionID,
	))

	after, err := database.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, "stored-solution", after.Project,
		"single-session re-sync must preserve the stored project")
}

// TestSyncEngineVisualStudioCopilotUnreadableSiblingBlocksPartialSession
// verifies that a conversation is not indexed from a subset of its trace files
// when a sibling is unreadable. A conversation's spans can live in any sibling,
// so an unreadable sibling might hold some of them. Reconstructing from only the
// readable files would index a partial transcript that full message replacement
// later treats as complete, so the conversation must fail to sync and be retried
// until every sibling is readable.
func TestSyncEngineVisualStudioCopilotUnreadableSiblingBlocksPartialSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	readable := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(readable, []byte(
		vsCopilotTraceLine(conversationID, "x1", "chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Update the XAML."}]}]`,
			})+"\n"), 0o644))

	// A sibling trace file that exists but cannot be read: a symlink to a
	// directory opens but cannot be scanned as JSONL. It may hold more of the
	// conversation's spans, so the conversation must not be reconstructed from
	// the readable file alone.
	target := filepath.Join(t.TempDir(), "dir")
	require.NoError(t, os.Mkdir(target, 0o755))
	sibling := filepath.Join(
		tracesDir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.Symlink(target, sibling))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})

	stats := engine.SyncAll(context.Background(), nil)
	assert.NotZero(t, stats.Failed,
		"an unreadable sibling must surface as a sync failure")

	sess, err := database.GetSession(
		context.Background(), "visualstudio-copilot:"+conversationID,
	)
	require.NoError(t, err)
	assert.Nil(t, sess,
		"the conversation must not be indexed as a partial transcript while "+
			"a sibling is unreadable")
}

// TestSyncSingleSessionVisualStudioCopilotReparsesWhenSiblingChanges verifies
// that a single-session re-sync re-parses a conversation when a sibling trace
// file gains spans, even though the representative trace file is unchanged. A
// conversation's transcript is rebuilt from every sibling trace file, so the
// skip fingerprint must span all of them; keying it on the representative file
// alone would skip the re-sync and leave the session stale.
func TestSyncSingleSessionVisualStudioCopilotReparsesWhenSiblingChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	primary := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(primary, []byte(
		vsCopilotTraceLine(conversationID, "a1", "chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"First task."}]}]`,
			})+"\n"), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	assertSessionMessageCount(t, database, sessionID, 1)

	// A sibling trace file gains a second turn for the same conversation while
	// the representative trace file (primary) is left untouched. The stored
	// source path still points at the primary, so a single-session re-sync must
	// notice the sibling change and re-gather the conversation's spans.
	sibling := filepath.Join(
		tracesDir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(sibling, []byte(
		vsCopilotTraceLine(conversationID, "b1", "chat gpt-5.5",
			"1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Second task."}]}]`,
			})+"\n"), 0o644))

	require.NoError(t, engine.SyncSingleSessionContext(
		context.Background(), sessionID,
	))
	assertSessionMessageCount(t, database, sessionID, 2)
}

// TestSourceMtimeVisualStudioCopilotReflectsSiblingChanges verifies that
// SourceMtime returns the composite mtime across all sibling trace files, not
// just the representative one. The file-watcher fallback compares SourceMtime to
// decide whether to resync; if it only saw the representative file, a sibling
// trace gaining spans would be missed and the session would never refresh.
func TestSourceMtimeVisualStudioCopilotReflectsSiblingChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	primary := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(primary, []byte(
		vsCopilotTraceLine(conversationID, "a1", "chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"First."}]}]`,
			})+"\n"), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)

	before := engine.SourceMtime(sessionID)
	require.NotZero(t, before)

	// A sibling trace file gains spans with a strictly newer mtime while the
	// representative trace file is left untouched.
	sibling := filepath.Join(
		tracesDir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(sibling, []byte(
		vsCopilotTraceLine(conversationID, "b1", "chat gpt-5.5",
			"1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Second."}]}]`,
			})+"\n"), 0o644))
	newer := time.Unix(0, before+int64(time.Hour))
	require.NoError(t, os.Chtimes(sibling, newer, newer))

	after := engine.SourceMtime(sessionID)
	assert.Greater(t, after, before,
		"SourceMtime must reflect a newer sibling trace file")
	assert.Equal(t, newer.UnixNano(), after,
		"SourceMtime must return the composite max sibling mtime")
}

// TestSyncSingleSessionVisualStudioCopilotDirReadErrorSurfaces verifies that a
// failure to enumerate sibling trace files during the skip check surfaces as a
// sync error instead of being hidden by the fingerprint's stat fallback. With
// the fallback, a single-trace directory whose ReadDir fails would yield a
// fingerprint identical to the stored one and be skipped as "unchanged",
// silently caching the directory read error.
func TestSyncSingleSessionVisualStudioCopilotDirReadErrorSurfaces(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("directory permission semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read permissions")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	primary := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(primary, []byte(
		vsCopilotTraceLine(conversationID, "a1", "chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"First."}]}]`,
			})+"\n"), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	assertSessionMessageCount(t, database, sessionID, 1)

	// Make the directory traversable but not readable: the stored trace file can
	// still be stat'd and opened, but enumerating siblings via ReadDir fails.
	require.NoError(t, os.Chmod(tracesDir, 0o100))
	t.Cleanup(func() { _ = os.Chmod(tracesDir, 0o755) })

	err := engine.SyncSingleSessionContext(context.Background(), sessionID)
	require.Error(t, err,
		"a sibling directory read error must surface, not be cached as an "+
			"unchanged skip")
}

// TestSyncEngineVisualStudioCopilotPreservesSessionWhenSiblingDeleted verifies
// that deleting a sibling trace file that contributed part of a conversation
// does not overwrite the archived session with a partial transcript. A
// conversation is rebuilt from every sibling trace file and written with full
// message replacement, so a reparse after a sibling is rotated away would
// otherwise permanently drop messages already stored in SQLite.
func TestSyncEngineVisualStudioCopilotPreservesSessionWhenSiblingDeleted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	primary := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(primary, []byte(
		vsCopilotTraceLine(conversationID, "a1", "chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"First task."}]}]`,
			})+"\n"), 0o644))
	sibling := filepath.Join(
		tracesDir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(sibling, []byte(
		vsCopilotTraceLine(conversationID, "b1", "chat gpt-5.5",
			"1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Second task."}]}]`,
			})+"\n"), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	assertSessionMessageCount(t, database, sessionID, 2)
	require.NoError(t, database.SetSessionDataVersion(
		sessionID, db.CurrentDataVersion()-1,
	))

	// The sibling that contributed the second turn is rotated away. A reparse now
	// sees only the first turn; the archived two-turn transcript must be
	// preserved rather than force-replaced with the partial one.
	require.NoError(t, os.Remove(sibling))
	currentSize, currentMtime := parser.VisualStudioCopilotTraceFingerprint(
		primary,
	)

	require.NoError(t, engine.SyncSingleSessionContext(
		context.Background(), sessionID,
	))
	assertSessionMessageCount(t, database, sessionID, 2)
	sess, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess)
	require.NotNil(t, sess.FileSize)
	assert.Equal(t, currentSize, *sess.FileSize)
	require.NotNil(t, sess.FileMtime)
	assert.Equal(t, currentMtime, *sess.FileMtime)
	assert.Equal(t, db.CurrentDataVersion(), sess.DataVersion)
}

// TestSyncEngineVisualStudioCopilotPreservesToolResultsWhenTraceShrinks verifies
// that a same-message-count reparse which loses tool result events does not
// overwrite the richer archived transcript.
func TestSyncEngineVisualStudioCopilotPreservesToolResultsWhenTraceShrinks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	tracePath := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	toolSpan := func(result string) string {
		attrs := map[string]string{
			"gen_ai.tool.name":           "run_command_in_terminal",
			"gen_ai.tool.call.id":        "call_build",
			"gen_ai.tool.call.arguments": `{"command":"dotnet build"}`,
		}
		if result != "" {
			attrs["gen_ai.tool.call.result"] = result
		}
		return vsCopilotTraceLine(conversationID, "tool_build",
			"execute_tool run_command_in_terminal",
			"1781293600000000000", "1781293610000000000",
			attrs) + "\n"
	}
	require.NoError(t, os.WriteFile(tracePath, []byte(
		toolSpan(`{"Value":"Build succeeded."}`),
	), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	msgs := fetchMessages(t, database, sessionID)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].ToolCalls, 1)
	require.Len(t, msgs[0].ToolCalls[0].ResultEvents, 1)

	require.NoError(t, os.WriteFile(tracePath, []byte(toolSpan("")), 0o644))
	later := time.Unix(1781293700, 0)
	require.NoError(t, os.Chtimes(tracePath, later, later))

	require.NoError(t, engine.SyncSingleSessionContext(
		context.Background(), sessionID,
	))
	msgs = fetchMessages(t, database, sessionID)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].ToolCalls, 1)
	require.Len(t, msgs[0].ToolCalls[0].ResultEvents, 1,
		"archived tool result event must be preserved")
	assert.Equal(t, "Build succeeded.",
		msgs[0].ToolCalls[0].ResultEvents[0].Content)
}

// TestSyncEngineVisualStudioCopilotMergesRicherMatchedMessageWhenTraceShrinks
// verifies that a shrink caused by a rotated sibling does not hide richer data
// that appears on a remaining same-key span.
func TestSyncEngineVisualStudioCopilotMergesRicherMatchedMessageWhenTraceShrinks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	primary := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	toolSpan := func(result string) string {
		attrs := map[string]string{
			"gen_ai.tool.name":           "run_command_in_terminal",
			"gen_ai.tool.call.id":        "call_build",
			"gen_ai.tool.call.arguments": `{"command":"dotnet build"}`,
		}
		if result != "" {
			attrs["gen_ai.tool.call.result"] = result
		}
		return vsCopilotTraceLine(conversationID, "tool_build",
			"execute_tool run_command_in_terminal",
			"1781293600000000000", "1781293610000000000",
			attrs) + "\n"
	}
	rotatedSibling := filepath.Join(
		tracesDir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	firstPrompt := "Retained first archived prompt."
	lastPrompt := strings.Repeat("Retained final archived prompt. ", 30)
	require.NoError(t, os.WriteFile(primary, []byte(toolSpan("")), 0o644))
	require.NoError(t, os.WriteFile(rotatedSibling, []byte(strings.Join([]string{
		vsCopilotTraceLine(conversationID, "first_chat", "chat gpt-5.5",
			"1781293580000000000", "1781293590000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"` + firstPrompt + `"}]}]`,
			}),
		vsCopilotTraceLine(conversationID, "last_chat", "chat gpt-5.5",
			"1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"` + lastPrompt + `"}]}]`,
			}),
	}, "\n")+"\n"), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	msgs := fetchMessages(t, database, sessionID)
	require.Len(t, msgs, 3)
	require.Len(t, msgs[1].ToolCalls, 1)
	require.Empty(t, msgs[1].ToolCalls[0].ResultEvents)

	require.NoError(t, os.Remove(rotatedSibling))
	require.NoError(t, os.WriteFile(primary, []byte(
		toolSpan(`{"Value":"Build succeeded."}`),
	), 0o644))
	later := time.Unix(1781293800, 0)
	require.NoError(t, os.Chtimes(primary, later, later))
	currentSize, currentMtime := parser.VisualStudioCopilotTraceFingerprint(
		primary,
	)

	require.NoError(t, engine.SyncSingleSessionContext(
		context.Background(), sessionID,
	))
	msgs = fetchMessages(t, database, sessionID)
	require.Len(t, msgs, 3,
		"archived-only sibling message must be retained")
	assert.Equal(t, strings.TrimSpace(firstPrompt), msgs[0].Content)
	require.Len(t, msgs[1].ToolCalls, 1)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1,
		"richer same-key tool result must be merged into archive")
	assert.Equal(t, "Build succeeded.",
		msgs[1].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, strings.TrimSpace(lastPrompt), msgs[2].Content)

	assertSessionState(t, database, sessionID, func(sess *db.Session) {
		require.NotNil(t, sess.FirstMessage)
		assert.Equal(t, strings.TrimSpace(firstPrompt), *sess.FirstMessage)
		require.NotNil(t, sess.StartedAt)
		require.NotNil(t, sess.EndedAt)
		assert.Equal(t, time.Unix(0, 1781293580000000000).UTC().
			Format(time.RFC3339Nano), *sess.StartedAt)
		assert.Equal(t, time.Unix(0, 1781293630000000000).UTC().
			Format(time.RFC3339Nano), *sess.EndedAt)
	})
	sess, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess)
	require.NotNil(t, sess.FileSize)
	assert.Equal(t, currentSize, *sess.FileSize)
	require.NotNil(t, sess.FileMtime)
	assert.Equal(t, currentMtime, *sess.FileMtime)
}

func TestSyncEngineVisualStudioCopilotMergesUpdateAndPreservesIncompleteSameCount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	tracePath := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	toolSpan := func(spanID, callID, command, start, end, result string) string {
		attrs := map[string]string{
			"gen_ai.tool.name":           "run_command_in_terminal",
			"gen_ai.tool.call.id":        callID,
			"gen_ai.tool.call.arguments": `{"command":"` + command + `"}`,
		}
		if result != "" {
			attrs["gen_ai.tool.call.result"] = result
		}
		return vsCopilotTraceLine(conversationID, spanID,
			"execute_tool run_command_in_terminal",
			start, end, attrs)
	}
	longResult := strings.Repeat("test output ", 120)
	initial := strings.Join([]string{
		toolSpan("tool_build", "call_build", "dotnet build",
			"1781293600000000000", "1781293610000000000", ""),
		toolSpan("tool_test", "call_test", "dotnet test",
			"1781293620000000000", "1781293630000000000",
			`{"Value":"`+longResult+`"}`),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(tracePath, []byte(initial), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	msgs := fetchMessages(t, database, sessionID)
	require.Len(t, msgs, 2)
	require.Empty(t, msgs[0].ToolCalls[0].ResultEvents)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)

	reparse := strings.Join([]string{
		toolSpan("tool_build", "call_build", "dotnet build",
			"1781293600000000000", "1781293610000000000",
			`{"Value":"Build succeeded."}`),
		toolSpan("tool_test", "call_test", "dotnet test",
			"1781293620000000000", "1781293630000000000", ""),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(tracePath, []byte(reparse), 0o644))
	later := time.Unix(1781293800, 0)
	require.NoError(t, os.Chtimes(tracePath, later, later))

	require.NoError(t, engine.SyncSingleSessionContext(
		context.Background(), sessionID,
	))
	msgs = fetchMessages(t, database, sessionID)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[0].ToolCalls[0].ResultEvents, 1,
		"same-count richer message must be merged")
	assert.Equal(t, "Build succeeded.",
		msgs[0].ToolCalls[0].ResultEvents[0].Content)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1,
		"same-count incomplete message must keep archived result")
	assert.Equal(t, strings.TrimSpace(longResult),
		msgs[1].ToolCalls[0].ResultEvents[0].Content)
}

func TestSyncEngineVisualStudioCopilotMergeDerivesFirstMessageFromMergedRows(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	primary := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	toolSpan := func(command string) string {
		return vsCopilotTraceLine(conversationID, "tool_build",
			"execute_tool run_command_in_terminal",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.tool.name":           "run_command_in_terminal",
				"gen_ai.tool.call.id":        "call_build",
				"gen_ai.tool.call.arguments": `{"command":"` + command + `"}`,
			})
	}
	rotatedSibling := filepath.Join(
		tracesDir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	laterPrompt := strings.Repeat("Retained later archived prompt. ", 30)
	require.NoError(t, os.WriteFile(primary, []byte(
		toolSpan("dotnet bui")+"\n",
	), 0o644))
	require.NoError(t, os.WriteFile(rotatedSibling, []byte(
		vsCopilotTraceLine(conversationID, "later_chat", "chat gpt-5.5",
			"1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"` + laterPrompt + `"}]}]`,
			})+"\n"), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	assertSessionState(t, database, sessionID, func(sess *db.Session) {
		require.NotNil(t, sess.FirstMessage)
		assert.Equal(t, "Run command: dotnet bui", *sess.FirstMessage)
	})

	require.NoError(t, os.Remove(rotatedSibling))
	require.NoError(t, os.WriteFile(primary, []byte(
		toolSpan("dotnet build --configuration Release")+"\n",
	), 0o644))
	later := time.Unix(1781293800, 0)
	require.NoError(t, os.Chtimes(primary, later, later))

	require.NoError(t, engine.SyncSingleSessionContext(
		context.Background(), sessionID,
	))
	msgs := fetchMessages(t, database, sessionID)
	require.Len(t, msgs, 2)
	assert.Equal(t, "[Bash: run_command_in_terminal]\n$ "+
		"dotnet build --configuration Release",
		msgs[0].Content)
	assert.Equal(t, strings.TrimSpace(laterPrompt), msgs[1].Content)
	assertSessionState(t, database, sessionID, func(sess *db.Session) {
		require.NotNil(t, sess.FirstMessage)
		assert.Equal(t,
			"Run command: dotnet build --configuration Release",
			*sess.FirstMessage,
		)
	})
}

// TestSyncEngineVisualStudioCopilotMergesNewMessageWhenTraceShrinks verifies
// that a shrink caused by a rotated sibling does not hide new spans that appear
// in a remaining or newly written trace file while still retaining archived-only
// messages.
func TestSyncEngineVisualStudioCopilotMergesNewMessageWhenTraceShrinks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	primary := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(primary, []byte(
		vsCopilotTraceLine(conversationID, "tool_build",
			"execute_tool run_command_in_terminal",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.tool.name":           "run_command_in_terminal",
				"gen_ai.tool.call.id":        "call_build",
				"gen_ai.tool.call.arguments": `{"command":"dotnet build"}`,
				"gen_ai.tool.call.result":    `{"Value":"Build succeeded."}`,
			})+"\n"), 0o644))
	rotatedSibling := filepath.Join(
		tracesDir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	oldPrompt := strings.Repeat("Retained archived prompt. ", 80)
	require.NoError(t, os.WriteFile(rotatedSibling, []byte(strings.Join([]string{
		vsCopilotTraceLine(conversationID, "old_chat_1", "chat gpt-5.5",
			"1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"` + oldPrompt + `"}]}]`,
			}),
		vsCopilotTraceLine(conversationID, "old_chat_2", "chat gpt-5.5",
			"1781293640000000000", "1781293650000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Another archived prompt."}]}]`,
			}),
	}, "\n")+"\n"), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	assertSessionMessageCount(t, database, sessionID, 3)

	require.NoError(t, os.Remove(rotatedSibling))
	newSibling := filepath.Join(
		tracesDir, "20260613T145205_cccc3333_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(newSibling, []byte(
		vsCopilotTraceLine(conversationID, "new_chat", "chat gpt-5.5",
			"1781293660000000000", "1781293670000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"New follow-up."}]}]`,
			})+"\n"), 0o644))
	later := time.Unix(1781293800, 0)
	require.NoError(t, os.Chtimes(newSibling, later, later))

	require.NoError(t, engine.SyncSingleSessionContext(
		context.Background(), sessionID,
	))
	msgs := fetchMessages(t, database, sessionID)
	require.Len(t, msgs, 4)
	assert.Equal(t, "[Bash: run_command_in_terminal]\n$ dotnet build",
		msgs[0].Content)
	assert.Equal(t, strings.TrimSpace(oldPrompt), msgs[1].Content)
	assert.Equal(t, "Another archived prompt.", msgs[2].Content)
	assert.Equal(t, "New follow-up.", msgs[3].Content)
}

// TestSyncEngineVisualStudioCopilotMergesNewMessageWhenCompositeGrows verifies
// that archive reconciliation is driven by message presence, not just by a
// shrinking composite trace size. If a rotated-away sibling is replaced by a
// larger new trace, the composite size can grow even though archived-only
// messages still need to be retained.
func TestSyncEngineVisualStudioCopilotMergesNewMessageWhenCompositeGrows(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	primary := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(primary, []byte(
		vsCopilotTraceLine(conversationID, "tool_build",
			"execute_tool run_command_in_terminal",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.tool.name":           "run_command_in_terminal",
				"gen_ai.tool.call.id":        "call_build",
				"gen_ai.tool.call.arguments": `{"command":"dotnet build"}`,
				"gen_ai.tool.call.result":    `{"Value":"Build succeeded."}`,
			})+"\n"), 0o644))
	rotatedSibling := filepath.Join(
		tracesDir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(rotatedSibling, []byte(
		vsCopilotTraceLine(conversationID, "old_chat", "chat gpt-5.5",
			"1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Archived prompt."}]}]`,
			})+"\n"), 0o644))
	storedSize, _ := parser.VisualStudioCopilotTraceFingerprint(primary)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	assertSessionMessageCount(t, database, sessionID, 2)

	require.NoError(t, os.Remove(rotatedSibling))
	newSibling := filepath.Join(
		tracesDir, "20260613T145205_cccc3333_VSGitHubCopilot_traces.jsonl",
	)
	newPrompt := strings.Repeat("New follow-up with enough detail. ", 120)
	require.NoError(t, os.WriteFile(newSibling, []byte(
		vsCopilotTraceLine(conversationID, "new_chat", "chat gpt-5.5",
			"1781293640000000000", "1781293650000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"` + newPrompt + `"}]}]`,
			})+"\n"), 0o644))
	currentSize, _ := parser.VisualStudioCopilotTraceFingerprint(primary)
	require.Greater(t, currentSize, storedSize,
		"test setup must grow the composite trace size")
	later := time.Unix(1781293800, 0)
	require.NoError(t, os.Chtimes(newSibling, later, later))

	require.NoError(t, engine.SyncSingleSessionContext(
		context.Background(), sessionID,
	))
	msgs := fetchMessages(t, database, sessionID)
	require.Len(t, msgs, 3)
	assert.Equal(t, "[Bash: run_command_in_terminal]\n$ dotnet build",
		msgs[0].Content)
	assert.Equal(t, "Archived prompt.", msgs[1].Content)
	assert.Equal(t, strings.TrimSpace(newPrompt), msgs[2].Content)
}

// TestSyncEngineVisualStudioCopilotDoesNotAppendRotatedDuplicateToolCall
// verifies that a re-emitted copy of an archived tool span with a changed
// timestamp replaces the archived copy instead of being appended as a new
// transcript row while another archived-only sibling row is retained.
func TestSyncEngineVisualStudioCopilotDoesNotAppendRotatedDuplicateToolCall(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	primary := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	toolSpan := func(start, end string) string {
		return vsCopilotTraceLine(conversationID, "tool_build",
			"execute_tool run_command_in_terminal", start, end,
			map[string]string{
				"gen_ai.tool.name":           "run_command_in_terminal",
				"gen_ai.tool.call.id":        "call_build",
				"gen_ai.tool.call.arguments": `{"command":"dotnet build"}`,
				"gen_ai.tool.call.result":    `{"Value":"Build succeeded."}`,
			}) + "\n"
	}
	require.NoError(t, os.WriteFile(primary, []byte(
		toolSpan("1781293600000000000", "1781293610000000000"),
	), 0o644))
	rotatedSibling := filepath.Join(
		tracesDir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(rotatedSibling, []byte(
		vsCopilotTraceLine(conversationID, "archived_chat", "chat gpt-5.5",
			"1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Archived prompt."}]}]`,
			})+"\n"), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	assertSessionMessageCount(t, database, sessionID, 2)

	require.NoError(t, os.Remove(rotatedSibling))
	require.NoError(t, os.WriteFile(primary, []byte(
		toolSpan("1781293660000000000", "1781293670000000000"),
	), 0o644))
	later := time.Unix(1781293800, 0)
	require.NoError(t, os.Chtimes(primary, later, later))

	require.NoError(t, engine.SyncSingleSessionContext(
		context.Background(), sessionID,
	))
	msgs := fetchMessages(t, database, sessionID)
	require.Len(t, msgs, 2)
	assert.Equal(t, "[Bash: run_command_in_terminal]\n$ dotnet build",
		msgs[0].Content)
	require.Len(t, msgs[0].ToolCalls, 1)
	assert.Equal(t, "call_build", msgs[0].ToolCalls[0].ToolUseID)
	require.Len(t, msgs[0].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "Build succeeded.",
		msgs[0].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "Archived prompt.", msgs[1].Content)
}

// TestSyncAllVisualStudioCopilotSkipCacheUsesCompositeFingerprint verifies that
// a cached <traceFile>#<conversationID> skip entry does not short-circuit the
// composite sibling fingerprint. The generic skip cache keys on the
// representative trace's mtime alone; once a conversation is cached (here by
// trashing it) a later sibling-only change that leaves the representative mtime
// unchanged must still be detected, so Visual Studio Copilot is excluded from
// the generic skip cache.
func TestSyncAllVisualStudioCopilotSkipCacheUsesCompositeFingerprint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	primary := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(primary, []byte(
		vsCopilotTraceLine(conversationID, "a1", "chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"First."}]}]`,
			})+"\n"), 0o644))
	// Pin the representative's mtime; the generic skip cache keys on it.
	repTime := time.Unix(1781293610, 0)
	require.NoError(t, os.Chtimes(primary, repTime, repTime))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	assertSessionMessageCount(t, database, sessionID, 1)

	// Trash the conversation and re-sync so its virtual path lands in the skip
	// cache with the composite mtime (equal to the representative's mtime).
	require.NoError(t, database.SoftDeleteSession(sessionID))
	require.NoError(t, database.ResetAllMtimes())
	engine.SyncAll(context.Background(), nil)
	require.NotZero(t,
		engine.SnapshotSkipCache()[parser.VisualStudioCopilotVirtualPath(
			primary, conversationID,
		)],
		"trashed conversation must be in the skip cache for this test to "+
			"exercise the bypass",
	)

	// Restore the conversation and add a sibling (older mtime, so the
	// representative stays primary and its mtime is unchanged) that gives the
	// conversation a second turn. The composite size grows but the
	// representative mtime does not, so only the composite fingerprint can
	// detect the change.
	_, restoreErr := database.RestoreSession(sessionID)
	require.NoError(t, restoreErr)
	sibling := filepath.Join(
		tracesDir, "20260610T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(sibling, []byte(
		vsCopilotTraceLine(conversationID, "b1", "chat gpt-5.5",
			"1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Second."}]}]`,
			})+"\n"), 0o644))
	olderTime := time.Unix(1781293600, 0)
	require.NoError(t, os.Chtimes(sibling, olderTime, olderTime))

	engine.SyncAll(context.Background(), nil)
	assertSessionMessageCount(t, database, sessionID, 2)
}
