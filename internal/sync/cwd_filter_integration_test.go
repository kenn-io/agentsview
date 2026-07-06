package sync_test

import (
	"context"
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
