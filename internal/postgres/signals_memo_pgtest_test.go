//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestFrustrationMemoCommittedVersionsWithEqualTimestamps(t *testing.T) {
	const schema = "agentsview_signals_version_test"
	const id = "signals-version"
	syncer, local, pg, ctx := newSessionProvenancePushSync(t, schema)
	url := testPGURL(t)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: id, Agent: "claude", Project: "sample", Machine: "workstation", MessageCount: 1,
	}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{
		SessionID: id, Ordinal: 0, Role: "user", Content: "this is broken",
	}}))
	require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{
		QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion},
	}))
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, url, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	// Distinct committed writes can share a transaction-start timestamp.
	const timestamp = "2026-01-01T00:00:00.123456Z"
	_, err = pg.ExecContext(ctx, `UPDATE sessions SET display_name = 'Renamed', updated_at = $1::timestamptz WHERE id = $2`, timestamp, id)
	require.NoError(t, err)
	initial, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, initial.QualityHealth.Totals.FrustrationMarkerCount)

	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE messages SET content = 'Please explain this function.' WHERE session_id = $1`, id)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET updated_at = $1::timestamptz WHERE id = $2`, timestamp, id)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	warm, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	fresh, err := NewStore(ctx, url, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	scanned, err := fresh.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
	require.NoError(t, err)
	assert.Zero(t, scanned.QualityHealth.Totals.FrustrationMarkerCount)
	assert.Equal(t, scanned.QualityHealth.Totals.FrustrationMarkerCount, warm.QualityHealth.Totals.FrustrationMarkerCount)
}

func TestFrustrationMemoPublicationBetweenReads(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		want    int
	}{
		{"edit", "Please explain this function.", 1},
		{"remove-prompts", "this is broken", 0},
		{"remove-session", "this is broken", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			const schema = "agentsview_signals_memo_race_test"
			const id = "signals-memo-race"
			syncer, local, pg, ctx := newSessionProvenancePushSync(t, schema)
			url := testPGURL(t)
			t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
			require.NoError(t, local.UpsertSession(ctx, db.Session{
				ID: id, Agent: "claude", Project: "sample", Machine: "workstation", MessageCount: 1,
			}))
			require.NoError(t, local.InsertMessages(ctx, []db.Message{{
				SessionID: id, Ordinal: 0, Role: "user", Content: test.content,
			}}))
			_, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			store, err := NewStore(ctx, url, schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			versionColumn, err := sessionVersionColumnPG(ctx, pg)
			require.NoError(t, err)
			rows, versions, err := store.signalRows(ctx, db.AnalyticsFilter{}, versionColumn)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Contains(t, versions, id)

			// Publish after selection, before the separate prompt scan.
			tx, err := pg.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer tx.Rollback()
			switch test.name {
			case "edit":
				_, err = tx.ExecContext(ctx, `UPDATE messages SET content = 'this is broken' WHERE session_id = $1`, id)
			case "remove-prompts":
				_, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = $1`, id)
			case "remove-session":
				_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, id)
			}
			require.NoError(t, err)
			var publishedVersion string
			if test.name != "remove-session" {
				require.NoError(t, tx.QueryRowContext(ctx, `UPDATE sessions SET updated_at = updated_at + interval '1 second' WHERE id = $1 RETURNING `+versionColumn+`::text`, id).Scan(&publishedVersion))
			}
			require.NoError(t, tx.Commit())

			require.NoError(t, store.populateFrustrationMarkers(ctx, rows, versions, versionColumn))
			assert.Equal(t, test.want, rows[0].FrustrationMarkerCount)
			memo := &store.runtimeState().frustrationMarkers
			_, hit := memo.Lookup(memo.Generation(), id, versions[id])
			assert.False(t, hit, "the prompt scan must not cache published content under the selected old version")
			if test.name != "remove-session" {
				count, hit := memo.Lookup(memo.Generation(), id, publishedVersion)
				assert.True(t, hit, "the prompt scan must cache the version it read, including sessions without prompts")
				assert.Equal(t, test.want, count)
			}
		})
	}
}

func TestFrustrationMemo(t *testing.T) {
	for _, test := range []struct {
		name string
		want int
	}{
		{"reuse", 7},
		{"append", 2},
		{"replace", 0},
		{"recreate", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			const schema = "agentsview_signals_memo_test"
			const id = "signals-memo"
			syncer, local, _, ctx := newSessionProvenancePushSync(t, schema)
			url := testPGURL(t)
			t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
			session := db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "workstation", MessageCount: 1}
			require.NoError(t, local.UpsertSession(ctx, session))
			messages := []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "this is broken"}}
			require.NoError(t, local.InsertMessages(ctx, messages))
			require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{
				QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion},
			}))
			_, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			store, err := NewStore(ctx, url, schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			initial, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
			require.NoError(t, err)
			require.Equal(t, 1, initial.QualityHealth.Totals.FrustrationMarkerCount)
			versionColumn, err := sessionVersionColumnPG(ctx, store.pg)
			require.NoError(t, err)
			_, versions, err := store.signalRows(ctx, db.AnalyticsFilter{}, versionColumn)
			require.NoError(t, err)
			require.Contains(t, versions, id)
			switch test.name {
			case "reuse":
				memo := &store.runtimeState().frustrationMarkers
				memo.Store(memo.Generation(), id, versions[id], 7)
			case "append":
				messages = append(messages, db.Message{SessionID: id, Ordinal: 1, Role: "user", Content: "this is broken"})
			case "replace", "recreate":
				messages[0].Content = "Please explain this function."
			}
			if test.name != "reuse" {
				session.MessageCount = len(messages)
				_, err = local.WriteSessionBatchContext(ctx, []db.SessionBatchWrite{{Session: session, Messages: messages, ReplaceMessages: true}})
				require.NoError(t, err)
				require.NoError(t, local.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{
					QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion},
				}))
			}
			if test.name == "recreate" {
				_, err = syncer.pg.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, id)
				require.NoError(t, err)
			}
			if test.name != "reuse" {
				_, err = syncer.Push(ctx, false, nil)
				require.NoError(t, err)
			}
			warm, err := store.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
			require.NoError(t, err)
			assert.Equal(t, test.want, warm.QualityHealth.Totals.FrustrationMarkerCount)
			if test.name != "reuse" {
				fresh, err := NewStore(ctx, url, schema, true)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, fresh.Close()) })
				scanned, err := fresh.GetAnalyticsSignals(ctx, db.AnalyticsFilter{})
				require.NoError(t, err)
				assert.Equal(t, scanned.QualityHealth.Totals.FrustrationMarkerCount, warm.QualityHealth.Totals.FrustrationMarkerCount)
			}
		})
	}
}
