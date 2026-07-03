package sync

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

// TestRecomputeSignalsFailedFindingsLeavesSessionStale pins the
// write-order contract BackfillSignals depends on: the quality signal
// version must only advance after secret findings persist, so a
// session whose compute failed partway stays below
// CurrentQualitySignalVersion and remains a backfill candidate on the
// next startup instead of silently keeping stale findings forever.
func TestRecomputeSignalsFailedFindingsLeavesSessionStale(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	d := dbtest.OpenTestDBAt(t, path)
	engine := NewEngine(d, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {t.TempDir()},
		},
		Machine: "local",
	})
	defer engine.Close()

	const id = "s1"
	require.NoError(t, d.UpsertSession(db.Session{
		ID: id, Project: "proj", Machine: "m", Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
	}))
	require.NoError(t, d.ReplaceSessionMessages(id, []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "hello"},
	}))

	// Break findings persistence only; signal columns still write.
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open raw connection")
	defer raw.Close()
	_, err = raw.Exec("DROP TABLE secret_findings")
	require.NoError(t, err, "drop findings table")

	require.Error(t, engine.RecomputeSignals(ctx, id),
		"recompute must fail when findings cannot persist")

	sess, err := d.GetSessionFull(ctx, id)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess)
	assert.Less(t,
		sess.QualitySignalVersion, db.CurrentQualitySignalVersion,
		"failed compute must leave the session eligible for backfill retry")
}
