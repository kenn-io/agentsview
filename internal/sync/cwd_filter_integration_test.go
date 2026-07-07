package sync_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
