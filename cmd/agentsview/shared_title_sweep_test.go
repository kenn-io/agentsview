package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// writeSharedTitleDatabaseFixture creates an Antigravity conversation title
// store holding one renamed conversation.
func writeSharedTitleDatabaseFixture(
	t *testing.T, path, id, title string,
) {
	t.Helper()
	dbtest.WriteTestFile(t, path, nil)
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `CREATE TABLE conversation_summaries (
		conversation_id text PRIMARY KEY, title text)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(),
		`INSERT INTO conversation_summaries (conversation_id, title) VALUES (?, ?)`,
		id, title)
	require.NoError(t, err)
}

// TestStartSharedTitleSweepRunsImmediatelyAndStopsOnCancel pins the loop's two
// observable obligations. The catch-up must not wait a full cycle before its
// first pass, because a rename that landed while AgentsView was closed has no
// event to arrive later. And cancelling the context must stop the loop, so
// process shutdown is never held open by the ticker.
func TestStartSharedTitleSweepRunsImmediatelyAndStopsOnCancel(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentAntigravity: {root},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)

	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	const sessionID = "antigravity:" + id
	stale := "stale name"
	// The stored source keeps the session under this root, which is what lets
	// the refresh confirm the summaries database owns it.
	conversationPath := filepath.Join(root, "conversations", id+".db")
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: sessionID, Agent: string(parser.AgentAntigravity),
		Machine: "devbox", SessionName: &stale, FilePath: &conversationPath,
	}))

	renamed := "renamed while the app was closed"
	writeSharedTitleDatabaseFixture(
		t, parser.AntigravityTitleDatabasePath(root), id, renamed)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		startSharedTitleSweep(ctx, engine)
	}()

	assert.EventuallyWithT(t, func(collect *assert.CollectT) {
		name, found, err := database.GetSessionName(t.Context(), sessionID)
		if !assert.NoError(collect, err) || !assert.True(collect, found) {
			return
		}
		assert.Equal(collect, renamed, name)
	}, 10*time.Second, 25*time.Millisecond,
		"the first sweep must run without waiting for a tick")

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("startSharedTitleSweep did not stop after cancellation")
	}
}
