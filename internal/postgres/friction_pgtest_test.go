//go:build pgtest

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPushSchemaCurrentRequiresFrictionTables(t *testing.T) {
	for _, drop := range []string{"TABLE friction_findings", "COLUMN friction_hash"} {
		t.Run(drop, func(t *testing.T) {
			pgURL := testPGURL(t)
			cleanPGSchema(t, pgURL)
			t.Cleanup(func() { cleanPGSchema(t, pgURL) })
			pg, err := Open(pgURL, "agentsview", true)
			require.NoError(t, err)
			defer pg.Close()
			ctx := context.Background()
			require.NoError(t, EnsureSchema(ctx, pg, "agentsview"))
			require.True(t, pushSchemaCurrent(ctx, pg))
			stmt := "DROP " + drop
			if strings.HasPrefix(drop, "COLUMN") {
				// An upgrade interrupted after creating the findings table.
				stmt = "ALTER TABLE sessions DROP " + drop
			}
			_, err = pg.ExecContext(ctx, stmt)
			require.NoError(t, err)
			assert.False(t, pushSchemaCurrent(ctx, pg),
				"an upgraded hub must run EnsureSchema before pushing friction")
			require.NoError(t, EnsureSchema(ctx, pg, "agentsview"))
			assert.True(t, pushSchemaCurrent(ctx, pg))
		})
	}
}

func seedFrictionSession(t *testing.T, local *db.DB, id string) {
	t.Helper()
	started := time.Now().UTC().Format(time.RFC3339)
	first := "friction push test"
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{
		ID: id, Project: "friction-project", Machine: "local",
		Agent: "claude", FirstMessage: &first, StartedAt: &started,
		MessageCount: 1,
	}))
	require.NoError(t, local.InsertMessages(t.Context(), []db.Message{{
		SessionID: id, Ordinal: 0, Role: "user", Content: first,
	}}))
}

func pgFrictionFindings(t *testing.T, ps *Sync, id string) []db.FrictionFinding {
	t.Helper()
	rows, err := ps.pg.QueryContext(t.Context(), `
		SELECT session_id, kind, detector, message_ordinal, call_index,
		       tool_name, label, text, evidence, title, fingerprint,
		       occurred_at, seq, rules_version
		FROM friction_findings WHERE session_id = $1 ORDER BY seq`, id)
	require.NoError(t, err)
	defer rows.Close()
	out := []db.FrictionFinding{}
	for rows.Next() {
		var f db.FrictionFinding
		require.NoError(t, rows.Scan(&f.SessionID, &f.Kind, &f.Detector,
			&f.MessageOrdinal, &f.CallIndex, &f.ToolName, &f.Label, &f.Text,
			&f.Evidence, &f.Title, &f.Fingerprint, &f.OccurredAt, &f.Seq,
			&f.RulesVersion))
		if f.OccurredAt != nil {
			utc := f.OccurredAt.UTC()
			f.OccurredAt = &utc
		}
		out = append(out, f)
	}
	require.NoError(t, rows.Err())
	return out
}

func testFrictionRows(id string) []db.FrictionFinding {
	ord, call := 0, 0
	at := time.Date(2026, 9, 16, 1, 35, 0, 120000000, time.UTC)
	findings := []db.FrictionFinding{
		{
			SessionID: id, Kind: "correction", Detector: "correction.coding",
			MessageOrdinal: &ord, Text: "no, use the config file instead",
			Title:       "[friction/correction] " + id + ": no, use the config file instead",
			Fingerprint: "fl1:aa", OccurredAt: &at, Seq: 0,
			RulesVersion: friction.RulesVersion,
		},
		{
			SessionID: id, Kind: "error", Detector: "error", MessageOrdinal: &ord,
			CallIndex: &call, ToolName: "Bash",
			Text:  `{"error":"bash: go: command not found","success":false}`,
			Title: "[friction/error] Bash: error", Fingerprint: "fl1:bb",
			Seq: 1, RulesVersion: friction.RulesVersion,
		},
	}
	return findings
}

func TestPGPushFrictionRoundTrip(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })
	local := testDB(t)
	ps, err := New(pgURL, "agentsview", local, "machine-friction", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	ctx := t.Context()
	require.NoError(t, ps.EnsureSchema(ctx))

	const id = "friction-sess-001"
	seedFrictionSession(t, local, id)
	findings := testFrictionRows(id)
	hash := db.FrictionHash(findings, friction.RulesVersion)
	require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{
		Friction: &db.SessionFrictionUpdate{
			Findings: findings, RulesVersion: friction.RulesVersion, Hash: hash,
		},
	}))
	res, err := ps.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.SessionsPushed)
	assert.Equal(t, findings, pgFrictionFindings(t, ps, id))
	var count int
	var version, pgHash string
	require.NoError(t, ps.pg.QueryRowContext(ctx, `
		SELECT friction_count, friction_rules_version, friction_hash
		FROM sessions WHERE id = $1`, id).Scan(&count, &version, &pgHash))
	assert.Equal(t, 2, count)
	assert.Equal(t, friction.RulesVersion, version)
	assert.Equal(t, hash, pgHash)
	assert.Equal(t, hash, db.FrictionHash(pgFrictionFindings(t, ps, id), version))
}

func TestPGPushFrictionChangeRepushesAndUnchangedSkips(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })
	local := testDB(t)
	ps, err := New(pgURL, "agentsview", local, "machine-friction-2", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	ctx := t.Context()
	require.NoError(t, ps.EnsureSchema(ctx))

	const id = "friction-sess-002"
	seedFrictionSession(t, local, id)
	findings := testFrictionRows(id)
	require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{
		Friction: &db.SessionFrictionUpdate{
			Findings: findings, RulesVersion: friction.RulesVersion,
			Hash: db.FrictionHash(findings, friction.RulesVersion),
		},
	}))
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err)
	r2, err := ps.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, r2.SessionsPushed)
	assert.Len(t, pgFrictionFindings(t, ps, id), 2)

	only := findings[:1]
	require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{
		Friction: &db.SessionFrictionUpdate{
			Findings: only, RulesVersion: friction.RulesVersion,
			Hash: db.FrictionHash(only, friction.RulesVersion),
		},
	}))
	r3, err := ps.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, r3.SessionsPushed)
	assert.Equal(t, only, pgFrictionFindings(t, ps, id))

	require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{
		Friction: &db.SessionFrictionUpdate{
			RulesVersion: friction.RulesVersion,
			Hash:         db.FrictionHash(nil, friction.RulesVersion),
		},
	}))
	cleared, err := ps.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, cleared.SessionsPushed)
	assert.Empty(t, pgFrictionFindings(t, ps, id))
}

func TestPGPushMetadataPreservesUnchangedFriction(t *testing.T) {
	for _, mode := range []string{"incremental", "without preload", "full"} {
		t.Run(mode, func(t *testing.T) {
			pgURL := testPGURL(t)
			cleanPGSchema(t, pgURL)
			t.Cleanup(func() { cleanPGSchema(t, pgURL) })
			local := testDB(t)
			ps, err := New(pgURL, "agentsview", local, "machine-friction", true, storage.PusherOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { ps.Close() })
			ctx := t.Context()
			require.NoError(t, ps.EnsureSchema(ctx))
			const id = "friction-metadata"
			seedFrictionSession(t, local, id)
			findings := testFrictionRows(id)
			require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{
				Friction: &db.SessionFrictionUpdate{
					Findings: findings, RulesVersion: friction.RulesVersion,
					Hash: db.FrictionHash(findings, friction.RulesVersion),
				},
			}))
			_, err = ps.Push(ctx, false, nil)
			require.NoError(t, err)

			var findingID int64
			require.NoError(t, ps.pg.QueryRowContext(ctx,
				`SELECT min(id) FROM friction_findings WHERE session_id = $1`, id,
			).Scan(&findingID))
			// Observe writes, including an otherwise redundant updated_at UPDATE.
			_, err = ps.pg.ExecContext(ctx, `
				CREATE TABLE session_write_counts (session_id TEXT);
				CREATE FUNCTION count_session_write() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					INSERT INTO session_write_counts VALUES (NEW.id);
					RETURN NULL;
				END; $$;
				CREATE TRIGGER count_session_write AFTER UPDATE ON sessions
					FOR EACH ROW EXECUTE FUNCTION count_session_write()`)
			require.NoError(t, err)
			require.NoError(t, local.RefreshSessionName(ctx, id, new("Renamed session")))
			if mode == "without preload" {
				sessions, err := local.ListSessionsForMirrorWindow(ctx, "", nil, nil)
				require.NoError(t, err)
				require.Len(t, sessions, 1)
				var markerID string
				require.NoError(t, ps.pg.QueryRowContext(ctx,
					`SELECT owner_marker FROM sessions WHERE id = $1`, id,
				).Scan(&markerID))
				var pushed []db.Session
				result, err := ps.pushBatchAttempt(ctx, sessions, false, markerID, nil, nil, &pushed, false)
				require.NoError(t, err)
				require.True(t, result.ok)
				require.Equal(t, 1, result.sessions)
				assert.Zero(t, result.messages)
			} else {
				result, err := ps.Push(ctx, mode == "full", nil)
				require.NoError(t, err)
				require.Equal(t, 1, result.SessionsPushed)
				if mode == "incremental" {
					assert.Zero(t, result.MessagesPushed)
				}
			}

			var newFindingID int64
			var name string
			require.NoError(t, ps.pg.QueryRowContext(ctx,
				`SELECT min(id) FROM friction_findings WHERE session_id = $1`, id,
			).Scan(&newFindingID))
			require.NoError(t, ps.pg.QueryRowContext(ctx,
				`SELECT session_name FROM sessions WHERE id = $1`, id,
			).Scan(&name))
			assert.Equal(t, "Renamed session", name)
			assert.Equal(t, findings, pgFrictionFindings(t, ps, id))
			if mode == "full" {
				assert.NotEqual(t, findingID, newFindingID, "full pushes still replace derived rows")
			} else {
				assert.Equal(t, findingID, newFindingID, "metadata updates leave findings untouched")
				var writes int
				require.NoError(t, ps.pg.QueryRowContext(ctx,
					`SELECT count(*) FROM session_write_counts WHERE session_id = $1`, id,
				).Scan(&writes))
				assert.Equal(t, 1, writes, "only the changed session metadata is written")
			}
		})
	}
}

func TestPushFrictionReportsChange(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })
	local := testDB(t)
	ps, err := New(pgURL, "agentsview", local, "machine-friction-3", true, storage.PusherOptions{})
	require.NoError(t, err)
	defer ps.Close()
	ctx := t.Context()
	require.NoError(t, ps.EnsureSchema(ctx))
	const id = "friction-sess-003"
	seedFrictionSession(t, local, id)
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err)

	pushOnce := func() bool {
		tx, err := ps.pg.BeginTx(ctx, nil)
		require.NoError(t, err)
		f, err := ps.pushFrictionFindings(ctx, tx, id)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		return f
	}
	assert.False(t, pushOnce())
	findings := testFrictionRows(id)
	require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{
		Friction: &db.SessionFrictionUpdate{
			Findings: findings, RulesVersion: friction.RulesVersion,
			Hash: db.FrictionHash(findings, friction.RulesVersion),
		},
	}))
	assert.True(t, pushOnce())
}
