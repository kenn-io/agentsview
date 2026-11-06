package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// The snapshot must answer exactly what the per-file queries it replaces
// answer, for rows that are active, user-deleted, source-missing, or shared
// between agents.
func TestSourceFreshnessSnapshotMatchesLiveQueries(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	dir := t.TempDir()
	paths := map[string]string{
		"active":        dir + "/active.jsonl",
		"twoRows":       dir + "/two.jsonl",
		"deleted":       dir + "/deleted.jsonl",
		"missing":       dir + "/missing.jsonl",
		"shared":        dir + "/shared.jsonl",
		"unknown":       dir + "/unknown.jsonl",
		"staleOnly":     dir + "/stale.jsonl",
		"nullSize":      dir + "/null-size.jsonl",
		"nullMtime":     dir + "/null-mtime.jsonl",
		"negativeMtime": dir + "/negative-mtime.jsonl",
		"freebuff":      dir + "/freebuff.jsonl",
	}
	var lookupPaths []string
	for _, path := range paths {
		lookupPaths = append(lookupPaths, path)
	}
	insertSessionWithSourcePath(t, d, "a", "claude", paths["active"])
	insertSessionWithSourcePath(t, d, "t1", "claude", paths["twoRows"])
	insertSessionWithSourcePath(t, d, "t2", "claude", paths["twoRows"])
	insertSessionWithSourcePath(t, d, "d", "claude", paths["deleted"])
	insertSessionWithSourcePath(t, d, "m", "claude", paths["missing"])
	insertSessionWithSourcePath(t, d, "s1", "codex", paths["shared"])
	insertSessionWithSourcePath(t, d, "s2", "traex", paths["shared"])
	insertSessionWithSourcePath(t, d, "st", "claude", paths["staleOnly"])
	insertSessionWithSourcePath(t, d, "ns", "claude", paths["nullSize"])
	insertSessionWithSourcePath(t, d, "nm", "claude", paths["nullMtime"])
	insertSessionWithSourcePath(t, d, "neg-null", "claude", paths["negativeMtime"])
	insertSessionWithSourcePath(t, d, "neg", "claude", paths["negativeMtime"])
	insertSessionWithSourcePath(t, d, "fb", "freebuff", paths["freebuff"])
	for _, q := range []string{
		`UPDATE sessions SET file_size=NULL, file_mtime=100 WHERE id='ns'`,
		`UPDATE sessions SET file_size=10, file_mtime=NULL WHERE id IN ('nm','neg-null')`,
		`UPDATE sessions SET project='negative', file_size=10, file_mtime=-1 WHERE id='neg'`,
		`UPDATE sessions SET project='p', file_size=10, file_mtime=100, data_version=` + itoa(CurrentDataVersion()) + ` WHERE id IN ('a','d','m','s1','st','fb')`,
		`UPDATE sessions SET project='old', file_size=10, file_mtime=50,  data_version=` + itoa(CurrentDataVersion()) + ` WHERE id='t1'`,
		`UPDATE sessions SET project='new', file_size=10, file_mtime=200, data_version=1 WHERE id='t2'`,
		`UPDATE sessions SET project='p', file_size=10, file_mtime=100, data_version=` + itoa(CurrentDataVersion()) + ` WHERE id='s2'`,
		`UPDATE sessions SET deleted_at='2026-01-01T00:00:00Z' WHERE id='d'`,
		`UPDATE sessions SET source_missing_at='2026-01-01T00:00:00Z' WHERE id='m'`,
		`UPDATE sessions SET data_version=1 WHERE id='st'`,
	} {
		_, err := d.getWriter().Exec(ctx, q)
		require.NoError(t, err, q)
	}
	_, err := d.getWriter().Exec(ctx,
		`INSERT INTO provider_freshness (agent, file_path, stat_hash, updated_at)
		 VALUES ('claude', ?, 42, '2026-01-01T00:00:00Z')`, paths["active"])
	require.NoError(t, err)
	require.NoError(t, d.UpsertProviderStatHash(ctx, parser.AgentCodebuff, paths["freebuff"], 84))

	snap, err := d.LoadSourceFreshnessSnapshot(ctx, []parser.AgentType{parser.AgentClaude, parser.AgentCodex, parser.AgentTraeX, parser.AgentCodebuff}, lookupPaths)
	require.NoError(t, err)

	for name, path := range paths {
		for _, agent := range []string{"claude", "codex", "traex", "codebuff", "freebuff"} {
			_, _, liveFound := d.GetFileInfoByAgentPath(ctx, path, agent)
			project, version, _, _, liveActive := d.GetSourceRepairStateByAgentPath(ctx, path, agent)
			snapProject, snapVersion, active, found := snap.State(agent, path)
			require.Equal(t, liveFound, found, "%s/%s row presence", name, agent)
			if !found {
				continue
			}
			assert.Equal(t, liveActive, active, "%s/%s active", name, agent)
			assert.Equal(t, project, snapProject, "%s/%s project", name, agent)
			assert.Equal(t, version, snapVersion, "%s/%s data version", name, agent)
		}
	}
	// Ties can choose different projects under SQLite's query plans. The caller
	// must query live instead of imposing a new tie order.
	_, err = d.getWriter().Exec(ctx, `UPDATE sessions SET file_mtime=100 WHERE id IN ('t1','t2')`)
	require.NoError(t, err)
	snapTie, err := d.LoadSourceFreshnessSnapshot(ctx, []parser.AgentType{parser.AgentClaude}, lookupPaths)
	require.NoError(t, err)
	_, _, _, okTie := snapTie.State("claude", paths["twoRows"])
	assert.False(t, okTie)
	filtered, err := d.LoadSourceFreshnessSnapshot(ctx, []parser.AgentType{parser.AgentCodex}, lookupPaths)
	require.NoError(t, err)
	_, okFiltered := filtered.StatHash("claude", paths["active"])
	assert.False(t, okFiltered)
	_, _, _, okFiltered = filtered.State("claude", paths["active"])
	assert.False(t, okFiltered)

	h, ok := snap.StatHash("claude", paths["active"])
	assert.True(t, ok)
	assert.EqualValues(t, 42, h)
	_, ok = snap.StatHash("codex", paths["active"])
	assert.False(t, ok)
	h, ok = snap.StatHash("codebuff", paths["freebuff"])
	assert.True(t, ok)
	liveHash, livePresent, err := d.GetProviderStatHash(ctx, parser.AgentCodebuff, paths["freebuff"])
	require.NoError(t, err)
	assert.Equal(t, livePresent, ok)
	assert.Equal(t, liveHash, h)
	_, ok = snap.StatHash("freebuff", paths["freebuff"])
	assert.False(t, ok)
	var nilSnap *SourceFreshnessSnapshot
	_, _, _, ok = nilSnap.State("claude", paths["active"])
	assert.False(t, ok)
}

func TestSourceFreshnessSnapshotUnrelatedRows(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	path := t.TempDir() + "/local.jsonl"
	insertSessionWithSourcePath(t, d, "local", "claude", path)
	require.NoError(t, d.UpsertProviderStatHash(ctx, parser.AgentClaude, path, 42))
	// Retain both local and remote rows for the same provider. Neither set's
	// paths can match the one discovered source.
	_, err := d.getWriter().Exec(ctx, `
		WITH RECURSIVE retained(n) AS (
			VALUES(1) UNION ALL SELECT n+1 FROM retained WHERE n < 40000
		)
		INSERT INTO sessions (id, project, machine, agent, file_path)
		SELECT 'retained-' || n, 'p', CASE WHEN n % 2 = 0 THEN 'local' ELSE 'remote' END,
		       'claude', '/retained/' || n || '.jsonl' FROM retained`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(ctx, `
		INSERT INTO provider_freshness (agent, file_path, stat_hash)
		SELECT agent, file_path, 99 FROM sessions WHERE id LIKE 'retained-%'`)
	require.NoError(t, err)
	snap, err := d.LoadSourceFreshnessSnapshot(ctx, []parser.AgentType{parser.AgentClaude}, []string{path})
	require.NoError(t, err)
	assert.Len(t, snap.statHash, 1)
	assert.Len(t, snap.rows, 1)
	hash, found := snap.StatHash("claude", path)
	assert.True(t, found)
	assert.EqualValues(t, 42, hash)
}
